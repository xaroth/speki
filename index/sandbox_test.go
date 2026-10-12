package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// sandboxFixture has two vaults the user may query, and a third index that must stay out of reach.
func sandboxFixture(t *testing.T) (*Sandbox, []Source, *Index) {
	t.Helper()
	sb, err := NewSandbox()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	kb := newIndex(t, map[string]string{
		"runbooks/deploy.md":    "---\ntitle: Deploy\ntags: [runbook]\nupdated: 2025-03-01T00:00:00Z\nowner: ops\n---\n# Deploy\n\n## Steps\n\nClear the cache before invalidation of the proxy.\n",
		"runbooks/restart.md":   "---\ntitle: Restart\ntags: [runbook]\nupdated: 2026-05-01T00:00:00Z\n---\n# Restart\n\nSee [deploy](deploy.md) and [gone](nowhere.md) and [ESI](../services/esi-proxy.md).\n",
		"services/esi-proxy.md": "---\ntitle: ESI proxy\nstatus: active\nupdated: 2026-09-01T00:00:00Z\n---\n# ESI proxy\n\nThe cache and its invalidation rules.\n",
	})
	other := newIndex(t, map[string]string{
		"notes.md": "---\ntitle: Notes\ntags: [runbook]\n---\nNothing about caches.\n",
	})
	secret := newIndex(t, map[string]string{"secret.md": "# Secret\n\nThe launch codes.\n"})
	return sb, []Source{{Name: "ours/kb", Index: kb}, {Name: "ours/other", Index: other}}, secret
}

func runQuery(t *testing.T, sb *Sandbox, sources []Source, q string) (Rows, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), QueryTimeout)
	defer cancel()
	return sb.Query(ctx, sources, q)
}

// The examples from docs/functional/search-and-query.md.
func TestQueryExamples(t *testing.T) {
	sb, sources, _ := sandboxFixture(t)
	tests := []struct {
		name string
		sql  string
		want string // the rows, formatted
	}{
		{
			name: "runbooks nobody touched this year",
			sql: `SELECT d.vault, d.path, d.updated
				FROM documents d JOIN tags t USING (vault, path)
				WHERE t.tag = 'runbook' AND (d.updated IS NULL OR d.updated < '2026-01-01')
				ORDER BY d.updated;`,
			want: "[ours/other notes.md <nil>] [ours/kb runbooks/deploy.md 2025-03-01T00:00:00Z]",
		},
		{
			name: "what links to a document",
			sql: `SELECT vault, path, text FROM links
				WHERE vault = 'ours/kb' AND target_path = 'services/esi-proxy.md';`,
			want: "[ours/kb runbooks/restart.md ESI]",
		},
		{
			name: "links that point at nothing",
			sql: `SELECT vault, path, target FROM links
				WHERE kind != 'related' AND target_path IS NULL AND target NOT LIKE 'http%';`,
			want: "[ours/kb runbooks/restart.md nowhere.md]",
		},
		{
			name: "unknown frontmatter keys",
			sql: `SELECT vault, path, json_extract(extra, '$.owner') AS owner
				FROM documents WHERE json_extract(extra, '$.owner') IS NOT NULL;`,
			want: "[ours/kb runbooks/deploy.md ops]",
		},
		{
			name: "text search with metadata",
			sql: `SELECT f.vault, f.path, d.updated
				FROM fts f JOIN documents d USING (vault, path)
				WHERE f.body MATCH 'cache AND invalidation' AND d.status = 'active'
				ORDER BY d.updated DESC LIMIT 20;`,
			want: "[ours/kb services/esi-proxy.md 2026-09-01T00:00:00Z] [ours/kb runbooks/deploy.md 2025-03-01T00:00:00Z]",
		},
		{
			name: "headings and window functions",
			sql:  `SELECT path, text, row_number() OVER (PARTITION BY path ORDER BY line) FROM headings WHERE path = 'runbooks/deploy.md'`,
			want: "[runbooks/deploy.md Deploy 1] [runbooks/deploy.md Steps 2]",
		},
		{
			name: "json_each over tags",
			sql:  `SELECT j.value, count(*) FROM documents, json_each(documents.tags) j GROUP BY j.value ORDER BY j.value`,
			want: "[runbook 3]",
		},
		{
			name: "values and infinity",
			sql:  `VALUES (1e999, -1e999)`,
			want: "[+Inf -Inf]",
		},
		{
			name: "recursive CTE",
			sql:  `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 3) SELECT group_concat(i) FROM n`,
			want: "[1,2,3]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := runQuery(t, sb, sources, tt.sql)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, r := range rows.Rows {
				got = append(got, fmt.Sprint(r))
			}
			if strings.Join(got, " ") != tt.want {
				t.Errorf("rows = %s\nwant   %s", strings.Join(got, " "), tt.want)
			}
		})
	}
}

// Every one of these must fail without reading or changing anything it shouldn't.
func TestQueryEscapes(t *testing.T) {
	sb, sources, secret := sandboxFixture(t)
	secretPath := generationPath(secret.current.Load().dir, secret.current.Load().n)
	tests := []struct {
		name string
		sql  string
		want error
	}{
		{"insert", "INSERT INTO documents (path) VALUES ('x')", ErrBadQuery},
		{"insert into an attached table", "INSERT INTO v0.tags VALUES (1, 'x')", ErrBadQuery},
		{"update", "UPDATE v0.documents SET title = 'x'", ErrBadQuery},
		{"delete", "DELETE FROM v0.documents", ErrBadQuery},
		{"create table", "CREATE TABLE x (a)", ErrBadQuery},
		{"create temp table", "CREATE TEMP TABLE x AS SELECT 1", ErrBadQuery},
		{"drop view", "DROP VIEW documents", ErrBadQuery},
		{"attach another vault", "ATTACH '" + secretPath + "' AS s", ErrBadQuery},
		{"attach as uri", "ATTACH 'file:" + secretPath + "?mode=ro' AS s", ErrBadQuery},
		{"detach", "DETACH v0", ErrBadQuery},
		{"pragma", "PRAGMA table_info(documents)", ErrBadQuery},
		{"pragma write", "PRAGMA query_only = 0", ErrBadQuery},
		{"writable schema", "PRAGMA writable_schema = 1", ErrBadQuery},
		{"pragma function", "SELECT * FROM pragma_table_info('documents')", ErrBadQuery},
		{"pragma database list", "SELECT * FROM pragma_database_list", ErrBadQuery},
		{"schema", "SELECT * FROM sqlite_master", ErrBadQuery},
		{"attached schema", "SELECT * FROM v0.sqlite_master", ErrBadQuery},
		{"temp schema", "SELECT * FROM sqlite_temp_master", ErrBadQuery},
		{"internal table", "SELECT * FROM v0.meta", ErrBadQuery},
		{"vocabulary", "SELECT * FROM v0.words_vocab", ErrBadQuery},
		{"load extension", "SELECT load_extension('/tmp/x.so')", ErrBadQuery},
		{"randomblob", "SELECT randomblob(10)", ErrBadQuery},
		{"zeroblob", "SELECT zeroblob(1000000000)", ErrBadQuery},
		{"sqlite functions", "SELECT sqlite_version()", ErrBadQuery},
		{"fts5 functions", "SELECT fts5_source_id()", ErrBadQuery},
		{"two statements", "SELECT 1; SELECT 2", ErrBadQuery},
		{"comment hiding a statement", "SELECT 1; /* nothing */ DELETE FROM v0.documents", ErrBadQuery},
		{"semicolon in a comment", "SELECT 1 -- ; \n; SELECT 2", ErrBadQuery},
		{"transaction", "BEGIN", ErrBadQuery},
		{"savepoint", "SAVEPOINT x", ErrBadQuery},
		{"vacuum into", "VACUUM INTO '/tmp/stolen.db'", ErrBadQuery},
		{"analyze", "ANALYZE", ErrBadQuery},
		{"reindex", "REINDEX", ErrBadQuery},
		{"huge string", "SELECT printf('%.*c', 200000, 'x')", ErrQueryTooLarge},
		{"growing string", "WITH RECURSIVE c(x, s) AS (SELECT 1, printf('%.*c', 90000, 'a') UNION ALL SELECT x + 1, s || x FROM c WHERE x < 20000) SELECT x, length(s) FROM c ORDER BY s DESC", ErrQueryTooLarge},
		{"vacuum temp into", "VACUUM temp INTO '/tmp/stolen.db'", ErrBadQuery},
		{"explain", "EXPLAIN SELECT 1", ErrBadQuery},
		{"pragma data_version", "PRAGMA data_version", ErrBadQuery},
		{"quote after a semicolon", "SELECT 1; 'x'", ErrBadQuery},
		{"wide row", "SELECT " + strings.TrimSuffix(strings.Repeat("1, ", maxQueryColumns+1), ", "), ErrBadQuery},
		{"empty", "  -- nothing\n", ErrBadQuery},
		// A WITH may lead into a write; the authorizer and query_only stop those.
		{"with delete", "WITH x AS (SELECT 1) DELETE FROM v0.documents", ErrBadQuery},
		{"with insert", "WITH x AS (SELECT 1) INSERT INTO v0.tags SELECT 1, 'x' FROM x", ErrBadQuery},
		{"with update", "WITH x AS (SELECT 1) UPDATE v0.documents SET title = 'x'", ErrBadQuery},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := runQuery(t, sb, sources, tt.sql)
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
			if strings.Contains(err.Error(), t.TempDir()[:5]) && strings.Contains(err.Error(), "/") {
				t.Errorf("error shows a file path: %v", err)
			}
		})
	}

	rows, err := runQuery(t, sb, sources, "SELECT count(*) FROM documents")
	if err != nil || fmt.Sprint(rows.Rows) != "[[4]]" {
		t.Errorf("documents after the attempts: %v, %v", rows.Rows, err)
	}
}

func TestQueryTimeouts(t *testing.T) {
	sb, sources, _ := sandboxFixture(t)
	for name, q := range map[string]string{
		"endless recursion": "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n) SELECT count(*) FROM n",
		"cross join":        "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 100000) SELECT count(*) FROM n a, n b",
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			start := time.Now()
			_, err := sb.Query(ctx, sources, q)
			if !errors.Is(err, ErrQueryTimeout) {
				t.Errorf("error = %v", err)
			}
			if time.Since(start) > 2*time.Second {
				t.Errorf("took %s", time.Since(start))
			}
		})
	}
}

func TestQueryLimitsAndErrors(t *testing.T) {
	sb, sources, _ := sandboxFixture(t)

	rows, err := runQuery(t, sb, sources, "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 600) SELECT i FROM n")
	if err != nil || len(rows.Rows) != maxQueryRows || !rows.Truncated {
		t.Errorf("row limit: %d rows, truncated %v, %v", len(rows.Rows), rows.Truncated, err)
	}
	rows, err = runQuery(t, sb, sources, "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 300) SELECT printf('%.*c', 10000, 'x') FROM n")
	if err != nil || len(rows.Rows) >= 300 || !rows.Truncated {
		t.Errorf("size limit: %d rows, truncated %v, %v", len(rows.Rows), rows.Truncated, err)
	}

	_, err = runQuery(t, sb, sources, "SELECT nope FROM v1.documents")
	if !errors.Is(err, ErrBadQuery) || !strings.Contains(err.Error(), "nope") {
		t.Errorf("syntax error = %v", err)
	}
	_, err = runQuery(t, sb, sources, "SELEC 1")
	if !errors.Is(err, ErrBadQuery) {
		t.Errorf("syntax error = %v", err)
	}
	_, err = runQuery(t, sb, sources, "SELECT * FROM v2.documents") // only two vaults attached
	if !errors.Is(err, ErrBadQuery) {
		t.Errorf("a vault that isn't attached = %v", err)
	}

	many := make([]Source, MaxQuerySources+1)
	for i := range many {
		many[i] = sources[0]
	}
	_, err = runQuery(t, sb, many, "SELECT 1")
	if !errors.Is(err, ErrBadQuery) {
		t.Errorf("too many vaults = %v", err)
	}

	rows, err = runQuery(t, sb, nil, "SELECT count(*) FROM documents")
	if err != nil || fmt.Sprint(rows.Rows) != "[[0]]" {
		t.Errorf("no vaults: %v, %v", rows.Rows, err)
	}
}

// Every query gets a fresh connection: nothing one query sets up is there for the next.
func TestQueriesInParallel(t *testing.T) {
	sb, sources, _ := sandboxFixture(t)
	var wg sync.WaitGroup
	errs := make(chan error, 2*sandboxConns)
	for range 2 * sandboxConns {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(t.Context(), QueryTimeout)
			defer cancel()
			_, err := sb.Query(ctx, sources, "SELECT path FROM fts WHERE body MATCH 'cache'")
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

// The hardening hook turns on DEFENSIVE for sandbox connections, which refuses schema edits even with
// writable_schema, and leaves index files alone.
func TestSandboxConnectionsAreDefensive(t *testing.T) {
	sb, _, _ := sandboxFixture(t)
	conn, err := sb.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	for _, stmt := range []string{"PRAGMA writable_schema = ON", "CREATE TABLE t (a)"} {
		_, err := conn.ExecContext(t.Context(), stmt)
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = conn.ExecContext(t.Context(), "UPDATE sqlite_schema SET sql = 'CREATE TABLE t (b)' WHERE name = 't'")
	if err == nil {
		t.Error("a sandbox connection could edit its schema")
	}

	file, err := sql.Open("sqlite3", "file:"+t.TempDir()+"/x.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	_, err = file.ExecContext(t.Context(), "PRAGMA journal_mode = OFF")
	if err != nil {
		t.Errorf("an index file connection was hardened too: %v", err)
	}
}

// A query reads an index while Apply writes to it (the file is in WAL mode), and sees a consistent
// snapshot either side of each change.
func TestQueryWhileApplying(t *testing.T) {
	sb, sources, _ := sandboxFixture(t)
	kb := sources[0].Index
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			content := fmt.Sprintf("# Note %d\n\nWritten while querying.\n", i)
			err := kb.Apply(t.Context(), fmt.Sprintf("c%d", i+2), []Change{{Path: "notes/live.md", Version: version(content), Content: []byte(content)}})
			if err != nil {
				t.Error(err)
				return
			}
		}
	})
	for range 20 {
		rows, err := runQuery(t, sb, sources[:1], "SELECT count(*) FROM documents WHERE path = 'notes/live.md'")
		if err != nil {
			t.Fatal(err)
		}
		if n := fmt.Sprint(rows.Rows); n != "[[0]]" && n != "[[1]]" {
			t.Errorf("count = %s", n)
		}
	}
	close(stop)
	wg.Wait()
}

func TestQueryBrokenDocuments(t *testing.T) {
	sb, sources, _ := sandboxFixture(t)
	bad := "# Deploy\n\nNew steps \xff\n"
	err := sources[0].Index.Apply(t.Context(), "c2", []Change{{Path: "runbooks/deploy.md", Version: version(bad), Content: []byte(bad)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ sql, want string }{
		{
			sql:  `SELECT vault, path, broken, body, version FROM documents WHERE status = 'broken'`,
			want: "[[ours/kb runbooks/deploy.md unusable document: not UTF-8  " + version(bad) + "]]",
		},
		{
			sql:  `SELECT count(*) FROM documents WHERE broken IS NULL`,
			want: "[[3]]",
		},
		{
			sql:  `SELECT path FROM fts WHERE body MATCH 'steps'`,
			want: "[]",
		},
	} {
		rows, err := runQuery(t, sb, sources, tt.sql)
		if err != nil {
			t.Fatalf("%s: %v", tt.sql, err)
		}
		if got := fmt.Sprint(rows.Rows); got != tt.want {
			t.Errorf("%s = %s, want %s", tt.sql, got, tt.want)
		}
	}
}

// A query that needs more memory than SQLite may take fails as too large instead of growing the process.
func TestQueryMemoryLimit(t *testing.T) {
	sb, sources, _ := sandboxFixture(t)
	setHardHeapLimit(64 << 20)
	defer setHardHeapLimit(hardHeapLimit)
	_, err := runQuery(t, sb, sources, "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 5000) SELECT x, printf('%.*c', 90000, 'a') AS s FROM c ORDER BY s, x DESC")
	if !errors.Is(err, ErrQueryTooLarge) {
		t.Errorf("error = %v", err)
	}
}

// A heavy query is stopped at the sandbox budget, below the hard limit, so searches keep getting memory.
func TestQueryMemoryBudget(t *testing.T) {
	sb, sources, _ := sandboxFixture(t)
	setQueryMemoryBudget(32 << 20)
	defer setQueryMemoryBudget(queryMemoryBudget)
	done := make(chan error, 1)
	go func() {
		_, err := runQuery(t, sb, sources, "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 5000) SELECT x, printf('%.*c', 90000, 'a') AS s FROM c ORDER BY s, x DESC")
		done <- err
	}()
	for {
		_, err := Search(t.Context(), sources, "cache", Options{})
		if err != nil {
			t.Errorf("search during the query: %v", err)
		}
		select {
		case err := <-done:
			if !errors.Is(err, ErrQueryTooLarge) || !strings.Contains(err.Error(), "memory") {
				t.Errorf("error = %v", err)
			}
			return
		default:
		}
	}
}

// A query doesn't start while SQLite is already past the budget.
func TestQueryShortOfMemory(t *testing.T) {
	sb, sources, _ := sandboxFixture(t)
	setQueryMemoryBudget(1)
	defer setQueryMemoryBudget(queryMemoryBudget)
	_, err := runQuery(t, sb, sources, "SELECT 1")
	if !errors.Is(err, ErrQueryBusy) {
		t.Errorf("error = %v", err)
	}
}

// Past sandboxConns running queries, the next one waits briefly, then gives up as busy.
func TestQueryBusy(t *testing.T) {
	sb, sources, _ := sandboxFixture(t)
	sb.wait = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), QueryTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for range sandboxConns {
		wg.Go(func() {
			_, _ = sb.Query(ctx, sources, "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n) SELECT count(*) FROM n")
		})
	}
	for len(sb.slots) < sandboxConns {
		time.Sleep(time.Millisecond)
	}
	_, err := runQuery(t, sb, sources, "SELECT 1")
	if !errors.Is(err, ErrQueryBusy) {
		t.Errorf("error = %v", err)
	}
	cancel()
	wg.Wait()
	_, err = runQuery(t, sb, sources, "SELECT 1")
	if err != nil {
		t.Errorf("after the others ended: %v", err)
	}
}

func TestQueryErrorNamesVaults(t *testing.T) {
	sb, sources, _ := sandboxFixture(t)
	_, err := runQuery(t, sb, sources, "SELECT * FROM v0.nosuch")
	if err == nil || !strings.Contains(err.Error(), "ours/kb.nosuch") {
		t.Errorf("error = %v", err)
	}
}

// The 1 MiB limit is on the result as it goes out, JSON-encoded: escapes for markdown's < > &, for
// invalid UTF-8 and for line separators, numbers in full.
func TestQueryLimitCountsEncodedSize(t *testing.T) {
	sb, sources, _ := sandboxFixture(t)
	for name, q := range map[string]string{
		"angle brackets":  "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 30) SELECT printf('%.*c', 90000, '<') FROM n",
		"invalid UTF-8":   "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 30) SELECT unhex(printf('%.*c', 98000, 'F')) FROM n",
		"line separators": "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 40) SELECT replace(printf('%.*c', 30000, 'x'), 'x', char(8232)) FROM n",
		"numbers":         "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 400) SELECT printf('%.*c', 2000, 'x'), " + strings.TrimSuffix(strings.Repeat("i * 0.1234567891234567, ", 45), ", ") + " FROM n",
	} {
		t.Run(name, func(t *testing.T) {
			rows, err := runQuery(t, sb, sources, q)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(map[string]any{"columns": rows.Columns, "rows": rows.Rows})
			if err != nil {
				t.Fatal(err)
			}
			if len(raw) > maxQueryBytes+64 {
				t.Errorf("%d bytes of JSON, truncated %v", len(raw), rows.Truncated)
			}
		})
	}
}

func TestQueryErrorsKeepTheirSlashes(t *testing.T) {
	sb, sources, _ := sandboxFixture(t)
	_, err := runQuery(t, sb, sources, "SELECT / 1")
	if err == nil || strings.Contains(err.Error(), "<file>") {
		t.Errorf("a lone slash read as a file path: %v", err)
	}
	for _, name := range []string{"x/y", "a/b/c"} {
		_, err = runQuery(t, sb, sources, "SELECT * FROM '"+name+"'")
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("a name with slashes read as a file path: %v", err)
		}
	}
	relative := []Source{{Name: "acme/kb", Index: &Index{dir: "data/100/index"}}}
	for msg, want := range map[string]string{
		"unable to open database: /data/vaults/x/index/3.db": "unable to open database: <file>",
		"cannot open file:/data/a.db?mode=ro":                "cannot open <file>",
		"cannot open file:///data/a/b.db?mode=ro":            "cannot open <file>",
		"bad [/x/y/z.db]":                                    "bad [<file>", // the bracket goes too
		"cannot open data/100/index/index-1.db":              "cannot open <file>",
		"cannot open file:data/100/index/index-1.db?mode=ro": "cannot open <file>",
		"no such table: a/b/c":                               "no such table: a/b/c",
	} {
		if got := scrubPaths(msg, relative); got != want {
			t.Errorf("scrubbed %q to %q, want %q", msg, got, want)
		}
	}
}

// The forms mcpserver/schema.md gives for aggregating matches work.
func TestMatchWithAggregates(t *testing.T) {
	sb, sources, _ := sandboxFixture(t)
	for _, q := range []string{
		"SELECT count(*) FROM (SELECT path FROM fts WHERE body MATCH 'cache' LIMIT 500)",
		"WITH m AS MATERIALIZED (SELECT vault, path FROM fts WHERE body MATCH 'cache') SELECT vault, count(*) FROM m GROUP BY vault",
	} {
		if _, err := runQuery(t, sb, sources, q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
}

func TestCancelledQueryIsNotATimeout(t *testing.T) {
	sb, sources, _ := sandboxFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := sb.Query(ctx, sources, "SELECT 1")
	if errors.Is(err, ErrQueryTimeout) || !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v", err)
	}
}
