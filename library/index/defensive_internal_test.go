// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index

// In-package: these use a sandbox's own connection and change SQLite's process-wide memory limits.

import (
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
)

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
