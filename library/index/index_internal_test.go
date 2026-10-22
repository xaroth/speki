// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index

// In-package: these hold generations, inject SQL failures, read internal tables, or change SQLite's
// process-wide memory limits, none of which the exported API reaches.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mattn/go-sqlite3"
)

var vault = map[string]string{
	"README.md":    "# About\n\nKnowledge about our fleet.\n",
	"cache.md":     "# Cache\n\nHard problems: cache invalidation and naming things.\n",
	"ops/fleet.md": "# Fleet\n\nSee [the readme](../README.md).\n",
}

func TestGenerations(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	x, err := Open(t.Context(), dir, Discard())
	if err != nil {
		t.Fatalf("Open: got error %v, want nil", err)
	}
	if got := x.Commit(); got != "" {
		t.Errorf("empty index commit: got %q, want empty", got)
	}

	err = x.Build(t.Context(), "c1", Changes(vault))
	if err != nil {
		t.Fatalf("Build: got error %v, want nil", err)
	}
	// A reader on the first build keeps working while a second build replaces it.
	old, err := x.acquire()
	if err != nil {
		t.Fatalf("acquire: got error %v, want nil", err)
	}
	err = x.Build(t.Context(), "c2", Changes(map[string]string{"only.md": "# Only\n"}))
	if err != nil {
		t.Fatalf("Build: got error %v, want nil", err)
	}
	var n int
	err = old.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM documents").Scan(&n)
	if err != nil || n != len(vault) {
		t.Errorf("old generation during swap: got %d documents, %v; want %d, nil", n, err, len(vault))
	}
	old.release()
	x.Close()

	files, _ := filepath.Glob(filepath.Join(dir, "index-*.db"))
	if len(files) != 1 {
		t.Errorf("generation files: got %v, want one", files)
	}

	// Garbage next to it is cleaned up, and the newest usable generation is reopened.
	err = os.WriteFile(filepath.Join(dir, "index-99.db"), []byte("not sqlite"), 0o644)
	if err != nil {
		t.Fatalf("WriteFile: got error %v, want nil", err)
	}
	x, err = Open(t.Context(), dir, Discard())
	if err != nil {
		t.Fatalf("reopen: got error %v, want nil", err)
	}
	t.Cleanup(x.Close)
	if got := x.Commit(); got != "c2" {
		t.Errorf("reopened commit: got %q, want c2", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "index-99.db")); !os.IsNotExist(err) {
		t.Errorf("unusable generation: got stat error %v, want not exist", err)
	}
	stats, err := x.Stats(t.Context())
	if err != nil || stats.Documents != 1 {
		t.Errorf("Stats: got %+v, %v; want 1 document", stats, err)
	}
}

// A failure that isn't the file's (here a failing insert) leaves the commit where it was, so the next
// sync tries again instead of losing the change.
func TestApplyFailureKeepsTheCommit(t *testing.T) {
	t.Parallel()
	x := NewTestIndex(t, vault)
	g, err := x.acquire()
	if err != nil {
		t.Fatalf("acquire: got error %v, want nil", err)
	}
	defer g.release()
	_, err = g.writer.ExecContext(t.Context(), `CREATE TRIGGER fail_headings BEFORE INSERT ON headings
		BEGIN SELECT RAISE(ABORT, 'disk full'); END`)
	if err != nil {
		t.Fatalf("create trigger: got error %v, want nil", err)
	}
	content := "# README\n\nNew text.\n"
	changes := []Change{
		{Path: "bad.md", Version: Version("z"), Content: []byte("not utf-8 \xff")},
		{Path: "README.md", Version: Version(content), Content: []byte(content)},
	}
	err = x.Apply(t.Context(), "c2", changes)
	if err == nil {
		t.Fatal("Apply with failing inserts: got nil error, want one")
	}
	if got := x.Commit(); got != "c1" {
		t.Errorf("commit: got %q, want c1", got)
	}

	_, err = g.writer.ExecContext(t.Context(), "DROP TRIGGER fail_headings")
	if err != nil {
		t.Fatalf("drop trigger: got error %v, want nil", err)
	}
	err = x.Apply(t.Context(), "c2", changes)
	if err != nil || x.Commit() != "c2" {
		t.Fatalf("retry: got %v, commit %q; want nil, c2", err, x.Commit())
	}
	if d, err := x.Document(t.Context(), "bad.md"); err != nil || d.Status != StatusBroken {
		t.Errorf("bad.md after the retry: got %q, %v; want %q", d.Status, err, StatusBroken)
	}
	if d, err := x.Document(t.Context(), "README.md"); err != nil || d.Version != Version(content) {
		t.Errorf("README.md after the retry: got version %q, %v; want %q", d.Version, err, Version(content))
	}
}

// Broken documents stay out of the full-text tables, whether built or applied, so their file-name titles
// don't match or feed typo corrections; adding and removing them leaves those tables intact.
func TestBrokenDocumentsStayOutOfFullText(t *testing.T) {
	t.Parallel()
	bad := "# Zanzibar\n\nNot UTF-8 \xff\n"
	testCases := []struct {
		name  string
		index func(tb testing.TB) *Index
	}{
		{name: "built", index: func(tb testing.TB) *Index {
			files := map[string]string{"zanzibar.md": bad}
			for k, v := range vault {
				files[k] = v
			}
			return NewTestIndex(tb, files)
		}},
		{name: "applied", index: func(tb testing.TB) *Index {
			x := NewTestIndex(tb, vault)
			err := x.Apply(tb.Context(), "c2", []Change{{Path: "zanzibar.md", Version: Version(bad), Content: []byte(bad)}})
			if err != nil {
				tb.Fatalf("Apply: got error %v, want nil", err)
			}
			return x
		}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			x := testCase.index(t)
			g, err := x.acquire()
			if err != nil {
				t.Fatalf("acquire: got error %v, want nil", err)
			}
			for _, q := range []string{
				"SELECT count(*) FROM fts WHERE title MATCH 'zanzibar'",
				"SELECT count(*) FROM names WHERE names MATCH 'zanzi'",
				"SELECT count(*) FROM words_vocab WHERE term = 'zanzibar'",
			} {
				var n int
				err := g.reader.QueryRowContext(ctx, q).Scan(&n)
				if err != nil || n != 0 {
					t.Errorf("%s: got %d, %v; want 0, nil", q, n, err)
				}
			}
			v, err := loadVocabulary(ctx, g)
			if err != nil {
				t.Fatalf("loadVocabulary: got error %v, want nil", err)
			}
			if got := v.suggest("zanzibat", 2); len(got) != 0 {
				t.Errorf("suggestions: got %v, want none", got)
			}
			g.release()

			good := "# Zanzibar\n\nFixed.\n"
			err = x.Apply(ctx, "c3", []Change{{Path: "zanzibar.md", Version: Version(good), Content: []byte(good)}})
			if err != nil {
				t.Fatalf("Apply: got error %v, want nil", err)
			}
			res, err := Search(ctx, []Source{{Name: "kb", Index: x}}, "zanzibar", Options{})
			if err != nil || len(res.Hits) != 1 {
				t.Errorf("search for the fixed document: got %+v, %v; want zanzibar.md", res.Hits, err)
			}
			err = x.Apply(ctx, "c4", []Change{{Path: "zanzibar.md", Version: Version(bad), Content: []byte(bad)}})
			if err != nil {
				t.Fatalf("Apply: got error %v, want nil", err)
			}
			err = x.Apply(ctx, "c5", []Change{{Path: "zanzibar.md"}})
			if err != nil {
				t.Fatalf("Apply: got error %v, want nil", err)
			}
			g, err = x.acquire()
			if err != nil {
				t.Fatalf("acquire: got error %v, want nil", err)
			}
			defer g.release()
			for _, table := range []string{"fts", "names", "words"} {
				_, err := g.writer.ExecContext(ctx, "INSERT INTO "+table+" ("+table+") VALUES ('integrity-check')")
				if err != nil {
					t.Errorf("%s integrity check: got error %v, want nil", table, err)
				}
			}
		})
	}
}

// Right after a change, before the vocabulary is reloaded, a partial word or a number isn't corrected.
func TestSearchDoesNotCorrectPartialWordsOrNumbers(t *testing.T) {
	t.Parallel()
	x := NewTestIndex(t, map[string]string{"a.md": "# Network\n\nnetwork notes for 2025\n"})
	x.current.Load().vocab.Store(nil)
	src := []Source{{Name: "kb", Index: x}}
	testCases := []struct {
		name  string
		query string
		hits  int
	}{
		{name: "partial word", query: "netw", hits: 1},
		{name: "number", query: "2026", hits: 0},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			res, err := Search(t.Context(), src, testCase.query, Options{})
			if err != nil {
				t.Fatalf("Search: got error %v, want nil", err)
			}
			if res.Corrected != nil || len(res.Hits) != testCase.hits {
				t.Errorf("got %d hits, corrected %v; want %d, none", len(res.Hits), res.Corrected, testCase.hits)
			}
		})
	}
}

// A snippet cut inside a match still closes the bold span, and stays within the limit.
func TestBoldMatches(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name string
		in   string
		want string // when short enough to compare
	}{
		{name: "cut inside a match", in: strings.Repeat("a", 490) + "\x01" + strings.Repeat("b", 30) + "\x02 c"},
		{name: "cut inside a character", in: "\x01" + strings.Repeat("é", 400) + "\x02"},
		{name: "short", in: "short \x01match\x02 here", want: "short **match** here"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got := boldMatches(testCase.in)
			if len(got) > maxDescription || strings.Count(got, "**")%2 != 0 || !utf8.ValidString(got) {
				t.Errorf("got %d bytes, %d markers, valid UTF-8 %v; want at most %d, even, valid",
					len(got), strings.Count(got, "**"), utf8.ValidString(got), maxDescription)
			}
			if testCase.want != "" && got != testCase.want {
				t.Errorf("got %q, want %q", got, testCase.want)
			}
		})
	}
}

// Only errors that mean a generation is unusable for good get it discarded; anything else, such as a
// cancelled context or too many open files, keeps the file for the next try.
func TestUnusableErrors(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "no meta row", err: sql.ErrNoRows, want: true},
		{name: "quick check", err: fmt.Errorf("%w: quick_check: row 3 missing", errUnusable), want: true},
		{name: "not a database", err: sqlite3.Error{Code: sqlite3.ErrNotADB}, want: true},
		{name: "corrupt", err: sqlite3.Error{Code: sqlite3.ErrCorrupt}, want: true},
		{name: "cancelled", err: context.Canceled, want: false},
		{name: "can't open", err: sqlite3.Error{Code: sqlite3.ErrCantOpen}, want: false},
		{name: "busy", err: sqlite3.Error{Code: sqlite3.ErrBusy}, want: false},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := errors.Is(unusable(testCase.err), errUnusable); got != testCase.want {
				t.Errorf("unusable(%v): got %v, want %v", testCase.err, got, testCase.want)
			}
		})
	}
}

// A build whose inserting fails keeps any other error next to it, so a broken git copy is noticed, but
// not the cancellation the failed insert caused.
func TestFillError(t *testing.T) {
	t.Parallel()
	insertErr := errors.New("insert a.md: disk full")
	readErr := errors.New("blob missing")
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	testCases := []struct {
		name      string
		ctx       context.Context
		insertErr error
		readErr   error
		want      []error
		wantText  string // when set, the whole message
	}{
		{name: "with a read error", ctx: t.Context(), insertErr: insertErr, readErr: fmt.Errorf("x: %w", readErr), want: []error{insertErr, readErr}},
		{name: "with the build's own cancellation", ctx: t.Context(), insertErr: insertErr, readErr: context.Canceled, wantText: insertErr.Error()},
		{name: "with the caller's cancellation", ctx: cancelled, insertErr: insertErr, readErr: context.Canceled, want: []error{context.Canceled}},
		{name: "with only a read error", ctx: t.Context(), readErr: readErr, want: []error{readErr}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			err := fillError(testCase.ctx, testCase.insertErr, testCase.readErr)
			for _, want := range testCase.want {
				if !errors.Is(err, want) {
					t.Errorf("got %v, want it to wrap %v", err, want)
				}
			}
			if testCase.wantText != "" && (err == nil || err.Error() != testCase.wantText) {
				t.Errorf("got %v, want %q", err, testCase.wantText)
			}
		})
	}
}

func TestScrubPaths(t *testing.T) {
	t.Parallel()
	relative := []Source{{Name: "acme/kb", Index: &Index{dir: "data/100/index"}}}
	testCases := []struct {
		name string
		msg  string
		want string
	}{
		{name: "absolute", msg: "unable to open database: /data/vaults/x/index/3.db", want: "unable to open database: <file>"},
		{name: "uri", msg: "cannot open file:/data/a.db?mode=ro", want: "cannot open <file>"},
		{name: "uri with host part", msg: "cannot open file:///data/a/b.db?mode=ro", want: "cannot open <file>"},
		{name: "in brackets", msg: "bad [/x/y/z.db]", want: "bad [<file>"}, // the bracket goes too
		{name: "relative index folder", msg: "cannot open data/100/index/index-1.db", want: "cannot open <file>"},
		{name: "relative index folder as uri", msg: "cannot open file:data/100/index/index-1.db?mode=ro", want: "cannot open <file>"},
		{name: "a table name", msg: "no such table: a/b/c", want: "no such table: a/b/c"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := scrubPaths(testCase.msg, relative); got != testCase.want {
				t.Errorf("got %q, want %q", got, testCase.want)
			}
		})
	}
}

// The hardening hook turns on DEFENSIVE for sandbox connections, which refuses schema edits even with
// writable_schema, and leaves index files alone.
func TestSandboxConnectionsAreDefensive(t *testing.T) {
	t.Parallel()
	sb, err := NewSandbox()
	if err != nil {
		t.Fatalf("NewSandbox: got error %v, want nil", err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	conn, err := sb.db.Conn(t.Context())
	if err != nil {
		t.Fatalf("Conn: got error %v, want nil", err)
	}
	defer func() { _ = conn.Close() }()
	for _, stmt := range []string{"PRAGMA writable_schema = ON", "CREATE TABLE t (a)"} {
		_, err := conn.ExecContext(t.Context(), stmt)
		if err != nil {
			t.Fatalf("%s: got error %v, want nil", stmt, err)
		}
	}
	_, err = conn.ExecContext(t.Context(), "UPDATE sqlite_schema SET sql = 'CREATE TABLE t (b)' WHERE name = 't'")
	if err == nil {
		t.Error("schema edit on a sandbox connection: got nil error, want one")
	}

	file, err := sql.Open("sqlite3", "file:"+t.TempDir()+"/x.db")
	if err != nil {
		t.Fatalf("sql.Open: got error %v, want nil", err)
	}
	defer func() { _ = file.Close() }()
	_, err = file.ExecContext(t.Context(), "PRAGMA journal_mode = OFF")
	if err != nil {
		t.Errorf("journal_mode on an index file: got error %v, want nil", err)
	}
}

// Past sandboxConns running queries, the next one waits briefly, then gives up as busy.
func TestQueryBusy(t *testing.T) {
	t.Parallel()
	sb, err := NewSandbox()
	if err != nil {
		t.Fatalf("NewSandbox: got error %v, want nil", err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	sb.wait = 10 * time.Millisecond
	for range sandboxConns { // as if that many queries were running
		sb.slots <- struct{}{}
	}
	_, err = sb.Query(t.Context(), nil, "SELECT 1")
	if !errors.Is(err, ErrQueryBusy) {
		t.Errorf("all slots taken: got %v, want %v", err, ErrQueryBusy)
	}
	<-sb.slots
	_, err = sb.Query(t.Context(), nil, "SELECT 1")
	if err != nil {
		t.Errorf("a slot free: got %v, want nil", err)
	}
}

// The memory tests below change SQLite's process-wide limits, so they don't run in parallel: other
// tests' queries would run into them. Go runs them before any parallel test resumes.

const heavyQuery = "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 5000) SELECT x, printf('%.*c', 90000, 'a') AS s FROM c ORDER BY s, x DESC"

func memorySources(tb testing.TB) (*Sandbox, []Source) {
	tb.Helper()
	sb, err := NewSandbox()
	if err != nil {
		tb.Fatalf("NewSandbox: got error %v, want nil", err)
	}
	tb.Cleanup(func() { _ = sb.Close() })
	return sb, []Source{{Name: "kb", Index: NewTestIndex(tb, vault)}}
}

// A query that needs more memory than SQLite may take fails as too large instead of growing the process.
func TestQueryMemoryLimit(t *testing.T) {
	sb, sources := memorySources(t)
	setHardHeapLimit(64 << 20) // lowers the soft limit with it
	t.Cleanup(func() { SetMemoryLimits(DefaultMemoryLimit, DefaultQueryMemory) })
	_, err := sb.Query(t.Context(), sources, heavyQuery)
	if !errors.Is(err, ErrQueryTooLarge) {
		t.Errorf("got %v, want %v", err, ErrQueryTooLarge)
	}
}

// A heavy query is stopped at the sandbox budget, below the hard limit, so searches keep getting memory.
func TestQueryMemoryBudget(t *testing.T) {
	sb, sources := memorySources(t)
	setQueryMemoryBudget(32 << 20)
	t.Cleanup(func() { setQueryMemoryBudget(DefaultQueryMemory << 20) })
	var wg sync.WaitGroup
	var queryErr error
	done := make(chan struct{})
	wg.Go(func() {
		defer close(done)
		_, queryErr = sb.Query(t.Context(), sources, heavyQuery)
	})
	for searching := true; searching; {
		_, err := Search(t.Context(), sources, "cache", Options{})
		if err != nil {
			t.Errorf("search during the query: got %v, want nil", err)
		}
		select {
		case <-done:
			searching = false
		default:
		}
	}
	wg.Wait()
	if !errors.Is(queryErr, ErrQueryTooLarge) || !strings.Contains(queryErr.Error(), "memory") {
		t.Errorf("got %v, want %v for memory", queryErr, ErrQueryTooLarge)
	}
}

// A query doesn't start while SQLite is already past the budget.
func TestQueryShortOfMemory(t *testing.T) {
	sb, sources := memorySources(t)
	setQueryMemoryBudget(1)
	t.Cleanup(func() { setQueryMemoryBudget(DefaultQueryMemory << 20) })
	_, err := sb.Query(t.Context(), sources, "SELECT 1")
	if !errors.Is(err, ErrQueryBusy) {
		t.Errorf("got %v, want %v", err, ErrQueryBusy)
	}
}
