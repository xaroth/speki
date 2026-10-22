// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/mattn/go-sqlite3" // also registers the sqlite3 driver
)

const filePrefix, fileSuffix = "index-", ".db"

// generation is one index file with its connections.
type generation struct {
	n      int
	dir    string
	writer *sql.DB // a single connection, used by Apply
	reader *sql.DB // read-only pool
	commit atomic.Value
	vocab  atomic.Pointer[vocabulary]

	mu      sync.Mutex
	refs    int
	retired bool
	remove  bool // delete the file once closed
}

func (g *generation) hold() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.retired {
		return false
	}
	g.refs++
	return true
}

func (g *generation) release() {
	g.mu.Lock()
	g.refs--
	done := g.retired && g.refs == 0
	remove := g.remove
	g.mu.Unlock()
	if done {
		g.close(remove)
	}
}

// retire stops new readers and closes g once the current ones are done, deleting its file if remove is
// set.
func (g *generation) retire(remove bool) {
	g.mu.Lock()
	g.retired = true
	g.remove = remove
	done := g.refs == 0
	g.mu.Unlock()
	if done {
		g.close(remove)
	}
}

func (g *generation) close(remove bool) {
	_ = g.reader.Close()
	_ = g.writer.Close()
	if remove {
		removeGeneration(g.dir, g.n)
	}
}

func generationPath(dir string, n int) string {
	return filepath.Join(dir, filePrefix+strconv.Itoa(n)+fileSuffix)
}

// generationsIn lists the generation numbers found in dir, oldest first.
func generationsIn(dir string) ([]int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("list index dir: %w", err)
	}
	var gens []int
	for _, e := range entries {
		name, ok := strings.CutPrefix(e.Name(), filePrefix)
		if !ok {
			continue
		}
		name, ok = strings.CutSuffix(name, fileSuffix)
		if !ok {
			continue // -wal and -shm files belong to their database
		}
		n, err := strconv.Atoi(name)
		if err != nil {
			continue // not a generation of ours
		}
		gens = append(gens, n)
	}
	slices.Sort(gens)
	return gens, nil
}

func removeGeneration(dir string, n int) {
	p := generationPath(dir, n)
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		_ = os.Remove(p + suffix)
	}
}

// errUnusable marks a generation that can't be used and won't become usable: corrupt, of another schema,
// or without a recorded commit.
var errUnusable = errors.New("unusable index")

func openGeneration(ctx context.Context, dir string, n int) (*generation, error) {
	g, err := connect(ctx, dir, n)
	if err != nil {
		return nil, unusable(err)
	}
	commit, err := checkGeneration(ctx, g.writer)
	if err != nil {
		g.close(false)
		return nil, unusable(err)
	}
	g.commit.Store(commit)
	return g, nil
}

// checkGeneration checks a generation's file and schema version, and returns its commit.
func checkGeneration(ctx context.Context, db *sql.DB) (string, error) {
	var check string
	err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check)
	if err != nil {
		return "", err
	}
	if check != "ok" {
		return "", fmt.Errorf("%w: quick_check: %s", errUnusable, check)
	}
	var version string
	err = db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'schema'").Scan(&version)
	if err != nil {
		return "", err
	}
	if version != schemaVersion {
		return "", fmt.Errorf("%w: schema version %s, want %s", errUnusable, version, schemaVersion)
	}
	var commit string
	err = db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'commit'").Scan(&commit)
	return commit, err
}

// unusable marks the errors that mean a generation is unusable for good.
func unusable(err error) error {
	serr, isSQLite := errors.AsType[sqlite3.Error](err)
	switch {
	case errors.Is(err, errUnusable):
		return err
	case errors.Is(err, sql.ErrNoRows):
	case isSQLite && (serr.Code == sqlite3.ErrCorrupt || serr.Code == sqlite3.ErrNotADB ||
		(serr.Code == sqlite3.ErrError && strings.Contains(serr.Error(), "no such table"))):
	default:
		return err
	}
	return fmt.Errorf("%w: %w", errUnusable, err)
}

func createGeneration(ctx context.Context, dir string, n int) (*generation, error) {
	removeGeneration(dir, n)
	g, err := connect(ctx, dir, n)
	if err != nil {
		return nil, err
	}
	err = initEmpty(ctx, g.writer)
	if err != nil {
		g.close(true)
		return nil, fmt.Errorf("create generation %d: %w", n, err)
	}
	g.commit.Store("")
	return g, nil
}

// initEmpty sets up an empty index in db: the schema, the triggers and no commit.
func initEmpty(ctx context.Context, db *sql.DB) error {
	err := initSchema(ctx, db)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, triggers)
	if err != nil {
		return err
	}
	return recordCommit(ctx, db, "")
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// initSchema creates the tables. It doesn't record a commit: a generation without one is unusable, so the
// commit is written last.
func initSchema(ctx context.Context, db execer) error {
	_, err := db.ExecContext(ctx, schema)
	if err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	_, err = db.ExecContext(ctx, "INSERT INTO meta (key, value) VALUES ('schema', ?)", schemaVersion)
	if err != nil {
		return fmt.Errorf("write meta: %w", err)
	}
	return nil
}

func recordCommit(ctx context.Context, db execer, commit string) error {
	_, err := db.ExecContext(ctx, "INSERT INTO meta (key, value) VALUES ('commit', ?)", commit)
	if err != nil {
		return fmt.Errorf("record commit: %w", err)
	}
	return nil
}

// connect opens the writer and reader pools of a generation file, creating the file when needed.
func connect(ctx context.Context, dir string, n int) (*generation, error) {
	p := generationPath(dir, n)
	writer, err := sql.Open("sqlite3", "file:"+p+"?_journal_mode=WAL&_foreign_keys=1&_busy_timeout=5000&_synchronous=NORMAL")
	if err != nil {
		return nil, err
	}
	writer.SetMaxOpenConns(1)
	err = writer.PingContext(ctx)
	if err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("open %s: %w", p, err)
	}
	err = requireFTS5(ctx, writer)
	if err != nil {
		_ = writer.Close()
		return nil, err
	}
	reader, err := sql.Open("sqlite3", "file:"+p+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		_ = writer.Close()
		return nil, err
	}
	reader.SetMaxOpenConns(4)
	return &generation{
		n:      n,
		dir:    dir,
		writer: writer,
		reader: reader,
	}, nil
}

func requireFTS5(ctx context.Context, db *sql.DB) error {
	var ok bool
	err := db.QueryRowContext(ctx, "SELECT sqlite_compileoption_used('ENABLE_FTS5')").Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("SQLite was built without FTS5: build speki with -tags sqlite_fts5")
	}
	return nil
}
