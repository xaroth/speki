// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/xaroth/speki/telemetry"
)

// Query limits.
const (
	// QueryTimeout is how long a query may take, waiting for a connection included.
	QueryTimeout    = 3 * time.Second
	MaxQuerySources = 10
	maxQueryRows    = 200
	maxQueryBytes   = 1 << 20
	maxValueBytes   = 100_000 // one string or blob
	maxQueryColumns = 50      // so a row is at most a few MB before the size check
	// sandboxConns caps queries running at once, for all users, so few queries share the memory budget.
	sandboxConns = 2
	// queueWait is how long a query waits for one of the others to finish before giving up as busy.
	queueWait = time.Second
)

// ErrBadQuery means the query isn't valid or isn't allowed. Like the other query errors, it wraps a
// message that is safe to show the agent.
var ErrBadQuery = errors.New("bad query")

// ErrQueryTimeout means the query ran longer than allowed.
var ErrQueryTimeout = errors.New("query timed out")

// ErrQueryTooLarge means the query needed more memory or produced more than allowed.
var ErrQueryTooLarge = errors.New("query needs too much")

// ErrQueryBusy means too many queries are running, or SQLite is short of memory. Trying again later works.
var ErrQueryBusy = errors.New("too busy for the query")

// Rows is a query's result.
type Rows struct {
	Columns   []string
	Rows      [][]any
	Truncated bool // the row or size limit was hit
}

// Sandbox runs SQL written by agents against read-only copies of indexes. Each query gets a fresh
// in-memory connection with only the user's vaults attached, read-only, behind an authorizer.
type Sandbox struct {
	db    *sql.DB
	slots chan struct{}
	wait  time.Duration
}

// NewSandbox returns a sandbox. Close it when done.
func NewSandbox() (*Sandbox, error) {
	err := harden()
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(sandboxConns)
	db.SetMaxIdleConns(0) // a connection is never reused (see Query): its attachments and views go with it
	return &Sandbox{db: db, slots: make(chan struct{}, sandboxConns), wait: queueWait}, nil
}

// Close closes the sandbox.
func (s *Sandbox) Close() error { return s.db.Close() }

// Query runs one SELECT against the sources, which appear in the views' vault column by name. ctx bounds
// the whole call, waiting for a connection included.
func (s *Sandbox) Query(ctx context.Context, sources []Source, query string) (Rows, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "index.Query", trace.WithAttributes(attribute.Int("speki.vaults", len(sources))))
	defer span.End()
	if len(sources) > MaxQuerySources {
		return Rows{}, fmt.Errorf("%w: at most %d vaults per query", ErrBadQuery, MaxQuerySources)
	}
	err := singleStatement(query)
	if err != nil {
		return Rows{}, err
	}

	err = s.take(ctx)
	if err != nil {
		return Rows{}, err
	}
	defer func() { <-s.slots }()
	if overQueryBudget() {
		return Rows{}, fmt.Errorf("%w: speki is short of memory", ErrQueryBusy)
	}

	gens, release, err := acquireAll(sources)
	defer release()
	if err != nil {
		return Rows{}, fmt.Errorf("query %w", err)
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return Rows{}, queryError(ctx, err, sources)
	}
	defer func() {
		// Never hand this connection out again: database/sql gives a released connection straight to a
		// waiting caller, whatever the idle limit, and this one has attachments and an authorizer.
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		_ = conn.Close()
	}()
	err = conn.Raw(func(dc any) error {
		return setUp(dc.(*sqlite3.SQLiteConn), sources, gens)
	})
	if serr, ok := errors.AsType[sqlite3.Error](err); ok && serr.Code == sqlite3.ErrInterrupt && ctx.Err() == nil {
		return Rows{}, fmt.Errorf("%w: speki is short of memory", ErrQueryBusy)
	}
	if err != nil {
		return Rows{}, fmt.Errorf("set up the query sandbox: %w", err)
	}

	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return Rows{}, queryError(ctx, err, sources)
	}
	defer func() { _ = rows.Close() }()
	return readRows(ctx, rows, sources)
}

// take waits a little for a free query slot.
func (s *Sandbox) take(ctx context.Context) error {
	timer := time.NewTimer(s.wait)
	defer timer.Stop()
	select {
	case s.slots <- struct{}{}:
		return nil
	case <-timer.C:
		return fmt.Errorf("%w: %d queries are running", ErrQueryBusy, cap(s.slots))
	case <-ctx.Done():
		return queryError(ctx, ctx.Err(), nil)
	}
}

// setUp attaches the sources read-only, creates the views and locks the connection down. It runs before
// the authorizer is registered, so its own ATTACH and CREATE aren't refused.
func setUp(c *sqlite3.SQLiteConn, sources []Source, gens []*generation) error {
	var dbs []string
	rows, err := c.Query("PRAGMA database_list", nil)
	if err != nil {
		return err
	}
	values := make([]driver.Value, 3)
	for rows.Next(values) == nil {
		dbs = append(dbs, fmt.Sprint(values[1]))
	}
	_ = rows.Close()
	if !slices.Equal(dbs, []string{"main"}) && !slices.Equal(dbs, []string{"main", "temp"}) {
		return fmt.Errorf("a fresh connection has databases %v", dbs)
	}

	for i, g := range gens {
		_, err := c.Exec(fmt.Sprintf("ATTACH %s AS v%d", literal((&url.URL{Scheme: "file", Path: generationPath(g.dir, g.n), RawQuery: "mode=ro"}).String()), i), nil)
		if err != nil {
			return fmt.Errorf("attach %s: %w", sources[i].Name, err)
		}
	}
	// temp_store first: changing it throws the temp database, and the views in it, away.
	for _, p := range []string{"PRAGMA temp_store = MEMORY", "PRAGMA cache_size = -8000"} {
		_, err := c.Exec(p, nil)
		if err != nil {
			return err
		}
	}
	for _, v := range viewSQL(sources) {
		_, err := c.Exec(v, nil)
		if err != nil {
			return fmt.Errorf("create views: %w", err)
		}
	}
	_, err = c.Exec("PRAGMA query_only = 1", nil)
	if err != nil {
		return err
	}
	for limit, value := range map[int]int{
		sqlite3.SQLITE_LIMIT_ATTACHED:            len(sources),
		sqlite3.SQLITE_LIMIT_LENGTH:              maxValueBytes,
		sqlite3.SQLITE_LIMIT_COLUMN:              maxQueryColumns,
		sqlite3.SQLITE_LIMIT_SQL_LENGTH:          100_000,
		sqlite3.SQLITE_LIMIT_EXPR_DEPTH:          50,
		sqlite3.SQLITE_LIMIT_COMPOUND_SELECT:     10,
		sqlite3.SQLITE_LIMIT_LIKE_PATTERN_LENGTH: 100,
	} {
		c.SetLimit(limit, value)
	}
	c.RegisterAuthorizer(authorize(len(sources)))
	return nil
}

// viewSQL creates the temp views agents query: one UNION ALL arm per attached vault.
func viewSQL(sources []Source) []string {
	arms := func(f func(db, vault string) string) string {
		var parts []string
		for i, src := range sources {
			parts = append(parts, f(fmt.Sprintf("v%d", i), literal(src.Name)))
		}
		return strings.Join(parts, "\nUNION ALL\n")
	}
	if len(sources) == 0 { // empty views with the right columns
		return []string{
			"CREATE TEMP VIEW documents AS SELECT NULL AS vault, NULL AS path, NULL AS title, NULL AS description, NULL AS status, NULL AS tags, NULL AS aliases, NULL AS related, NULL AS created, NULL AS updated, NULL AS extra, NULL AS body, NULL AS bytes, NULL AS lines, NULL AS version, NULL AS broken WHERE 0",
			"CREATE TEMP VIEW tags AS SELECT NULL AS vault, NULL AS path, NULL AS tag WHERE 0",
			"CREATE TEMP VIEW links AS SELECT NULL AS vault, NULL AS path, NULL AS target, NULL AS target_path, NULL AS kind, NULL AS text WHERE 0",
			"CREATE TEMP VIEW headings AS SELECT NULL AS vault, NULL AS path, NULL AS level, NULL AS text, NULL AS line WHERE 0",
			"CREATE TEMP VIEW fts AS SELECT NULL AS vault, NULL AS path, NULL AS title, NULL AS aliases, NULL AS tags, NULL AS description, NULL AS headings, NULL AS body WHERE 0",
		}
	}
	return []string{
		"CREATE TEMP VIEW documents AS " + arms(func(db, vault string) string {
			return "SELECT " + vault + " AS vault, path, title, description, status, tags, aliases, related, created, updated, extra, body, bytes, lines, version, broken FROM " + db + ".documents"
		}),
		"CREATE TEMP VIEW tags AS " + arms(func(db, vault string) string {
			return "SELECT " + vault + " AS vault, d.path, t.tag FROM " + db + ".tags t JOIN " + db + ".documents d ON d.id = t.doc_id"
		}),
		"CREATE TEMP VIEW links AS " + arms(func(db, vault string) string {
			return "SELECT " + vault + " AS vault, d.path, l.target, l.target_path, l.kind, l.text FROM " + db + ".resolved_links l JOIN " + db + ".documents d ON d.id = l.doc_id"
		}),
		"CREATE TEMP VIEW headings AS " + arms(func(db, vault string) string {
			return "SELECT " + vault + " AS vault, d.path, h.level, h.text, h.line FROM " + db + ".headings h JOIN " + db + ".documents d ON d.id = h.doc_id"
		}),
		"CREATE TEMP VIEW fts AS " + arms(func(db, vault string) string {
			// Without a MATCH, FTS5 reads every row of documents, broken ones included.
			return "SELECT " + vault + " AS vault, d.path, f.title, f.aliases, f.tags, f.description, f.headings, f.body FROM " + db + ".fts f JOIN " + db + ".documents d ON d.id = f.rowid WHERE d.status != 'broken'"
		}),
	}
}

// readRows reads up to the row and size limits.
func readRows(ctx context.Context, rows *sql.Rows, sources []Source) (Rows, error) {
	cols, err := rows.Columns()
	if err != nil {
		return Rows{}, queryError(ctx, err, sources)
	}
	out := Rows{Columns: cols, Rows: [][]any{}}
	size := 0
	for _, c := range cols {
		size += encodedSize(c) + 1
	}
	for rows.Next() {
		if len(out.Rows) == maxQueryRows {
			out.Truncated = true
			break
		}
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		err := rows.Scan(ptrs...)
		if err != nil {
			return Rows{}, queryError(ctx, err, sources)
		}
		size += 2 // the row's brackets
		for i, v := range values {
			switch v := v.(type) {
			case []byte:
				values[i] = string(v)
			case float64:
				if math.IsNaN(v) || math.IsInf(v, 0) { // JSON has no such numbers
					values[i] = strconv.FormatFloat(v, 'g', -1, 64)
				}
			}
			size += encodedSize(values[i]) + 1 // and its comma
		}
		if size > maxQueryBytes {
			out.Truncated = true
			break
		}
		out.Rows = append(out.Rows, values)
	}
	err = rows.Err()
	if err != nil {
		return Rows{}, queryError(ctx, err, sources)
	}
	return out, nil
}

// encodedSize is how many bytes v takes in the result, which is encoded with encoding/json: escapes for
// <, > and &, invalid UTF-8 and line separators, numbers in full. It's exact by being json.Marshal.
func encodedSize(v any) int {
	raw, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(raw)
}

// paths matches absolute file paths in SQLite messages, as a fallback for paths that aren't the
// sources' own: at least two segments, not inside a word. So a lone slash ("SELECT / 1") isn't one, nor
// is a name with slashes inside ('a/b/c').
var paths = regexp.MustCompile(`(^|[^\w/])(?:file:(?://)?)?(?:/[^\s'"/]+){2,}/?`)

// scrubPaths replaces file paths in msg with "<file>": first the sources' index folders, as given
// (relative or not, plain or as a file: URI), then anything that looks like an absolute path.
func scrubPaths(msg string, sources []Source) string {
	for _, src := range sources {
		if src.Index == nil || src.Index.dir == "" {
			continue
		}
		dir := strings.TrimPrefix(filepath.ToSlash(src.Index.dir), "/")
		re := regexp.MustCompile(`(?:file:)?/*` + regexp.QuoteMeta(dir) + `[^\s'"]*`)
		msg = re.ReplaceAllString(msg, "<file>")
	}
	return paths.ReplaceAllString(msg, "$1<file>")
}

// aliases matches the names vaults are attached under, where they qualify a table or column.
var aliases = regexp.MustCompile(`\bv(\d+)\.`)

// queryError turns a SQLite failure into an error for the agent: the cause wrapped in a sentinel, with
// the internal database names replaced by vault names and file paths removed.
func queryError(ctx context.Context, err error, sources []Source) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err() // the caller went away; that's not the query's doing
	}
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: it ran longer than allowed", ErrQueryTimeout)
	}
	serr, ok := errors.AsType[sqlite3.Error](err)
	if !ok {
		return err
	}
	msg := scrubPaths(serr.Error(), sources) // before vault names, which contain a slash
	msg = aliases.ReplaceAllStringFunc(msg, func(m string) string {
		n, err := strconv.Atoi(m[1 : len(m)-1])
		if err != nil || n >= len(sources) {
			return m
		}
		return sources[n].Name + "."
	})
	switch serr.Code {
	case sqlite3.ErrInterrupt: // with the context still live, only the memory budget interrupts
		return fmt.Errorf("%w: it needs more memory than queries may use", ErrQueryTooLarge)
	case sqlite3.ErrTooBig, sqlite3.ErrNomem, sqlite3.ErrFull:
		return fmt.Errorf("%w: %s", ErrQueryTooLarge, msg)
	case sqlite3.ErrAuth:
		return fmt.Errorf("%w: not allowed in queries: %s", ErrBadQuery, msg)
	case sqlite3.ErrError, sqlite3.ErrRange, sqlite3.ErrMismatch, sqlite3.ErrConstraint, sqlite3.ErrReadonly:
		return fmt.Errorf("%w: %s", ErrBadQuery, msg)
	}
	return err
}

func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
