package index_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/xaroth/speki/library/index"
)

func newSandbox(tb testing.TB) *index.Sandbox {
	tb.Helper()
	sb, err := index.NewSandbox()
	if err != nil {
		tb.Fatalf("NewSandbox: got error %v, want nil", err)
	}
	tb.Cleanup(func() { _ = sb.Close() })
	return sb
}

// sandboxFixture has two vaults the user may query, and a third index that must stay out of reach.
func sandboxFixture(tb testing.TB) (*index.Sandbox, []index.Source, *index.Index) {
	tb.Helper()
	ours := newIndex(tb, map[string]string{
		"runbooks/deploy.md":    "---\ntitle: Deploy\ntags: [runbook]\nupdated: 2025-03-01T00:00:00Z\nowner: ops\n---\n# Deploy\n\n## Steps\n\nClear the cache before invalidation of the proxy.\n",
		"runbooks/restart.md":   "---\ntitle: Restart\ntags: [runbook]\nupdated: 2026-05-01T00:00:00Z\n---\n# Restart\n\nSee [deploy](deploy.md) and [gone](nowhere.md) and [ESI](../services/esi-proxy.md).\n",
		"services/esi-proxy.md": "---\ntitle: ESI proxy\nstatus: active\nupdated: 2026-09-01T00:00:00Z\n---\n# ESI proxy\n\nThe cache and its invalidation rules.\n",
	})
	other := newIndex(tb, map[string]string{
		"notes.md": "---\ntitle: Notes\ntags: [runbook]\n---\nNothing about caches.\n",
	})
	secret := newIndex(tb, map[string]string{"secret.md": "# Secret\n\nThe launch codes.\n"})
	return newSandbox(tb), []index.Source{{Name: "ours/kb", Index: ours}, {Name: "ours/other", Index: other}}, secret
}

func runQuery(tb testing.TB, sb *index.Sandbox, sources []index.Source, q string) (index.Rows, error) {
	tb.Helper()
	ctx, cancel := context.WithTimeout(tb.Context(), index.QueryTimeout)
	defer cancel()
	return sb.Query(ctx, sources, q)
}

// query runs q in a sandbox of its own and fails the test on an error.
func query(tb testing.TB, sources []index.Source, q string) index.Rows {
	tb.Helper()
	rows, err := runQuery(tb, newSandbox(tb), sources, q)
	if err != nil {
		tb.Fatalf("Query(%q): got error %v, want nil", q, err)
	}
	return rows
}

// The examples from docs/functional/search-and-query.md.
func TestQueryExamples(t *testing.T) {
	t.Parallel()
	sb, sources, _ := sandboxFixture(t)
	testCases := []struct {
		name     string
		noVaults bool
		sql      string
		want     index.Rows
	}{
		{
			name: "runbooks nobody touched this year",
			sql: `SELECT d.vault, d.path, d.updated
				FROM documents d JOIN tags t USING (vault, path)
				WHERE t.tag = 'runbook' AND (d.updated IS NULL OR d.updated < '2026-01-01')
				ORDER BY d.updated;`,
			want: index.Rows{
				Columns: []string{"vault", "path", "updated"},
				Rows:    [][]any{{"ours/other", "notes.md", nil}, {"ours/kb", "runbooks/deploy.md", "2025-03-01T00:00:00Z"}},
			},
		},
		{
			name: "what links to a document",
			sql: `SELECT vault, path, text FROM links
				WHERE vault = 'ours/kb' AND target_path = 'services/esi-proxy.md';`,
			want: index.Rows{Columns: []string{"vault", "path", "text"}, Rows: [][]any{{"ours/kb", "runbooks/restart.md", "ESI"}}},
		},
		{
			name: "links that point at nothing",
			sql: `SELECT vault, path, target FROM links
				WHERE kind != 'related' AND target_path IS NULL AND target NOT LIKE 'http%';`,
			want: index.Rows{Columns: []string{"vault", "path", "target"}, Rows: [][]any{{"ours/kb", "runbooks/restart.md", "nowhere.md"}}},
		},
		{
			name: "unknown frontmatter keys",
			sql: `SELECT vault, path, json_extract(extra, '$.owner') AS owner
				FROM documents WHERE json_extract(extra, '$.owner') IS NOT NULL;`,
			want: index.Rows{Columns: []string{"vault", "path", "owner"}, Rows: [][]any{{"ours/kb", "runbooks/deploy.md", "ops"}}},
		},
		{
			name: "text search with metadata",
			sql: `SELECT f.vault, f.path, d.updated
				FROM fts f JOIN documents d USING (vault, path)
				WHERE f.body MATCH 'cache AND invalidation' AND d.status = 'active'
				ORDER BY d.updated DESC LIMIT 20;`,
			want: index.Rows{
				Columns: []string{"vault", "path", "updated"},
				Rows:    [][]any{{"ours/kb", "services/esi-proxy.md", "2026-09-01T00:00:00Z"}, {"ours/kb", "runbooks/deploy.md", "2025-03-01T00:00:00Z"}},
			},
		},
		{
			name: "headings and window functions",
			sql:  `SELECT path, text, row_number() OVER (PARTITION BY path ORDER BY line) AS n FROM headings WHERE path = 'runbooks/deploy.md'`,
			want: index.Rows{
				Columns: []string{"path", "text", "n"},
				Rows:    [][]any{{"runbooks/deploy.md", "Deploy", int64(1)}, {"runbooks/deploy.md", "Steps", int64(2)}},
			},
		},
		{
			name: "json_each over tags",
			sql:  `SELECT j.value, count(*) AS n FROM documents, json_each(documents.tags) j GROUP BY j.value ORDER BY j.value`,
			want: index.Rows{Columns: []string{"value", "n"}, Rows: [][]any{{"runbook", int64(3)}}},
		},
		{
			name: "infinity as text",
			sql:  `VALUES (1e999, -1e999)`,
			want: index.Rows{Columns: []string{"column1", "column2"}, Rows: [][]any{{"+Inf", "-Inf"}}},
		},
		{
			name: "recursive CTE",
			sql:  `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 3) SELECT group_concat(i) AS c FROM n`,
			want: index.Rows{Columns: []string{"c"}, Rows: [][]any{{"1,2,3"}}},
		},
		{
			name: "aggregate over a limited match",
			sql:  "SELECT count(*) AS n FROM (SELECT path FROM fts WHERE body MATCH 'cache' LIMIT 500)",
			want: index.Rows{Columns: []string{"n"}, Rows: [][]any{{int64(3)}}},
		},
		{
			name: "aggregate over a materialized match",
			sql:  "WITH m AS MATERIALIZED (SELECT vault, path FROM fts WHERE body MATCH 'cache') SELECT vault, count(*) AS n FROM m GROUP BY vault",
			want: index.Rows{Columns: []string{"vault", "n"}, Rows: [][]any{{"ours/kb", int64(2)}, {"ours/other", int64(1)}}},
		},
		{
			name:     "no vaults",
			noVaults: true,
			sql:      "SELECT count(*) AS n FROM documents",
			want:     index.Rows{Columns: []string{"n"}, Rows: [][]any{{int64(0)}}},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			src := sources
			if testCase.noVaults {
				src = nil
			}
			got, err := runQuery(t, sb, src, testCase.sql)
			if err != nil {
				t.Fatalf("Query: got error %v, want nil", err)
			}
			if diff := cmp.Diff(testCase.want, got); diff != "" {
				t.Errorf("rows (-want +got):\n%s", diff)
			}
		})
	}
}

// Every one of these must fail without reading or changing anything it shouldn't.
func TestQueryEscapes(t *testing.T) {
	t.Parallel()
	sb, sources, secret := sandboxFixture(t)
	secretPath := index.GenerationFile(secret)
	testCases := []struct {
		name string
		sql  string
		want error
	}{
		{"insert", "INSERT INTO documents (path) VALUES ('x')", index.ErrBadQuery},
		{"insert into an attached table", "INSERT INTO v0.tags VALUES (1, 'x')", index.ErrBadQuery},
		{"update", "UPDATE v0.documents SET title = 'x'", index.ErrBadQuery},
		{"delete", "DELETE FROM v0.documents", index.ErrBadQuery},
		{"create table", "CREATE TABLE x (a)", index.ErrBadQuery},
		{"create temp table", "CREATE TEMP TABLE x AS SELECT 1", index.ErrBadQuery},
		{"drop view", "DROP VIEW documents", index.ErrBadQuery},
		{"attach another vault", "ATTACH '" + secretPath + "' AS s", index.ErrBadQuery},
		{"attach as uri", "ATTACH 'file:" + secretPath + "?mode=ro' AS s", index.ErrBadQuery},
		{"detach", "DETACH v0", index.ErrBadQuery},
		{"pragma", "PRAGMA table_info(documents)", index.ErrBadQuery},
		{"pragma write", "PRAGMA query_only = 0", index.ErrBadQuery},
		{"writable schema", "PRAGMA writable_schema = 1", index.ErrBadQuery},
		{"pragma function", "SELECT * FROM pragma_table_info('documents')", index.ErrBadQuery},
		{"pragma database list", "SELECT * FROM pragma_database_list", index.ErrBadQuery},
		{"schema", "SELECT * FROM sqlite_master", index.ErrBadQuery},
		{"attached schema", "SELECT * FROM v0.sqlite_master", index.ErrBadQuery},
		{"temp schema", "SELECT * FROM sqlite_temp_master", index.ErrBadQuery},
		{"internal table", "SELECT * FROM v0.meta", index.ErrBadQuery},
		{"vocabulary", "SELECT * FROM v0.words_vocab", index.ErrBadQuery},
		{"load extension", "SELECT load_extension('/tmp/x.so')", index.ErrBadQuery},
		{"randomblob", "SELECT randomblob(10)", index.ErrBadQuery},
		{"zeroblob", "SELECT zeroblob(1000000000)", index.ErrBadQuery},
		{"sqlite functions", "SELECT sqlite_version()", index.ErrBadQuery},
		{"fts5 functions", "SELECT fts5_source_id()", index.ErrBadQuery},
		{"two statements", "SELECT 1; SELECT 2", index.ErrBadQuery},
		{"comment hiding a statement", "SELECT 1; /* nothing */ DELETE FROM v0.documents", index.ErrBadQuery},
		{"semicolon in a comment", "SELECT 1 -- ; \n; SELECT 2", index.ErrBadQuery},
		{"transaction", "BEGIN", index.ErrBadQuery},
		{"savepoint", "SAVEPOINT x", index.ErrBadQuery},
		{"vacuum into", "VACUUM INTO '/tmp/stolen.db'", index.ErrBadQuery},
		{"vacuum temp into", "VACUUM temp INTO '/tmp/stolen.db'", index.ErrBadQuery},
		{"analyze", "ANALYZE", index.ErrBadQuery},
		{"reindex", "REINDEX", index.ErrBadQuery},
		{"explain", "EXPLAIN SELECT 1", index.ErrBadQuery},
		{"pragma data_version", "PRAGMA data_version", index.ErrBadQuery},
		{"quote after a semicolon", "SELECT 1; 'x'", index.ErrBadQuery},
		{"wide row", "SELECT " + strings.TrimSuffix(strings.Repeat("1, ", index.MaxQueryColumns+1), ", "), index.ErrBadQuery},
		{"empty", "  -- nothing\n", index.ErrBadQuery},
		// A WITH may lead into a write; the authorizer and query_only stop those.
		{"with delete", "WITH x AS (SELECT 1) DELETE FROM v0.documents", index.ErrBadQuery},
		{"with insert", "WITH x AS (SELECT 1) INSERT INTO v0.tags SELECT 1, 'x' FROM x", index.ErrBadQuery},
		{"with update", "WITH x AS (SELECT 1) UPDATE v0.documents SET title = 'x'", index.ErrBadQuery},
		{"huge string", "SELECT printf('%.*c', 200000, 'x')", index.ErrQueryTooLarge},
		{"growing string", "WITH RECURSIVE c(x, s) AS (SELECT 1, printf('%.*c', 90000, 'a') UNION ALL SELECT x + 1, s || x FROM c WHERE x < 20000) SELECT x, length(s) FROM c ORDER BY s DESC", index.ErrQueryTooLarge},
	}
	// The group returns once all its parallel subtests are done, for the check after it.
	t.Run("group", func(t *testing.T) {
		for _, testCase := range testCases {
			t.Run(testCase.name, func(t *testing.T) {
				t.Parallel()
				_, err := runQuery(t, sb, sources, testCase.sql)
				if !errors.Is(err, testCase.want) {
					t.Fatalf("got error %v, want %v", err, testCase.want)
				}
				if strings.Contains(err.Error(), os.TempDir()) {
					t.Errorf("got error %v, want no file path in it", err)
				}
			})
		}
	})

	rows, err := runQuery(t, sb, sources, "SELECT count(*) FROM documents")
	if err != nil {
		t.Fatalf("Query: got error %v, want nil", err)
	}
	if diff := cmp.Diff([][]any{{int64(4)}}, rows.Rows); diff != "" {
		t.Errorf("documents after the attempts (-want +got):\n%s", diff)
	}
}

func TestQueryTimeout(t *testing.T) {
	t.Parallel()
	sb, sources, _ := sandboxFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := sb.Query(ctx, sources, "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n) SELECT count(*) FROM n")
	if !errors.Is(err, index.ErrQueryTimeout) {
		t.Errorf("got error %v, want %v", err, index.ErrQueryTimeout)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %s, want the query interrupted at its deadline", took)
	}
}

func TestCancelledQueryIsNotATimeout(t *testing.T) {
	t.Parallel()
	sb := newSandbox(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := sb.Query(ctx, nil, "SELECT 1")
	if errors.Is(err, index.ErrQueryTimeout) || !errors.Is(err, context.Canceled) {
		t.Errorf("got error %v, want %v", err, context.Canceled)
	}
}

func TestQueryLimits(t *testing.T) {
	t.Parallel()
	sb, sources, _ := sandboxFixture(t)
	testCases := []struct {
		name     string
		sql      string
		wantRows int // at most, for the size limit
	}{
		{name: "rows", sql: "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 600) SELECT i FROM n", wantRows: index.MaxQueryRows},
		{name: "size", sql: "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 300) SELECT printf('%.*c', 10000, 'x') FROM n", wantRows: index.MaxQueryBytes / 10000},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			rows, err := runQuery(t, sb, sources, testCase.sql)
			if err != nil {
				t.Fatalf("got error %v, want nil", err)
			}
			if len(rows.Rows) > testCase.wantRows || !rows.Truncated {
				t.Errorf("got %d rows, truncated %v; want at most %d, truncated", len(rows.Rows), rows.Truncated, testCase.wantRows)
			}
		})
	}

	// The 1 MiB limit is on the result as it goes out, JSON-encoded, escapes and numbers in full.
	encoded := map[string]string{
		"escaped characters": "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 30) SELECT printf('%.*c', 90000, '<') FROM n",
		"numbers":            "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 400) SELECT printf('%.*c', 2000, 'x'), " + strings.TrimSuffix(strings.Repeat("i * 0.1234567891234567, ", 45), ", ") + " FROM n",
	}
	for name, q := range encoded {
		t.Run("encoded "+name, func(t *testing.T) {
			t.Parallel()
			rows, err := runQuery(t, sb, sources, q)
			if err != nil {
				t.Fatalf("got error %v, want nil", err)
			}
			raw, err := json.Marshal(map[string]any{"columns": rows.Columns, "rows": rows.Rows})
			if err != nil {
				t.Fatalf("Marshal: got error %v, want nil", err)
			}
			if len(raw) > index.MaxQueryBytes+64 || !rows.Truncated {
				t.Errorf("got %d bytes of JSON, truncated %v; want at most %d, truncated", len(raw), rows.Truncated, index.MaxQueryBytes+64)
			}
		})
	}
}

func TestQueryErrors(t *testing.T) {
	t.Parallel()
	sb, sources, _ := sandboxFixture(t)
	many := make([]index.Source, index.MaxQuerySources+1)
	for i := range many {
		many[i] = sources[0]
	}
	testCases := []struct {
		name    string
		sources []index.Source
		sql     string
		want    error
		message string // in the error
	}{
		{name: "unknown column", sql: "SELECT nope FROM v1.documents", want: index.ErrBadQuery, message: "nope"},
		{name: "syntax", sql: "SELEC 1", want: index.ErrBadQuery},
		{name: "vault not attached", sql: "SELECT * FROM v2.documents", want: index.ErrBadQuery},
		{name: "too many vaults", sources: many, sql: "SELECT 1", want: index.ErrBadQuery},
		{name: "vault named", sql: "SELECT * FROM v0.nosuch", want: index.ErrBadQuery, message: "ours/kb.nosuch"},
		{name: "a lone slash isn't a path", sql: "SELECT / 1", want: index.ErrBadQuery, message: `"/"`},
		{name: "a name with a slash isn't a path", sql: "SELECT * FROM 'x/y'", want: index.ErrBadQuery, message: "x/y"},
		{name: "a name with slashes isn't a path", sql: "SELECT * FROM 'a/b/c'", want: index.ErrBadQuery, message: "a/b/c"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			src := sources
			if testCase.sources != nil {
				src = testCase.sources
			}
			_, err := runQuery(t, sb, src, testCase.sql)
			if !errors.Is(err, testCase.want) {
				t.Fatalf("got error %v, want %v", err, testCase.want)
			}
			if !strings.Contains(err.Error(), testCase.message) {
				t.Errorf("got error %v, want %q in it", err, testCase.message)
			}
		})
	}
}

// Every query gets a fresh connection: nothing one query sets up is there for the next.
func TestQueriesInParallel(t *testing.T) {
	t.Parallel()
	sb, sources, _ := sandboxFixture(t)
	var wg sync.WaitGroup
	errs := make(chan error, 2*index.SandboxConns)
	for range 2 * index.SandboxConns {
		wg.Go(func() {
			_, err := runQuery(t, sb, sources, "SELECT path FROM fts WHERE body MATCH 'cache'")
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("got error %v, want nil", err)
		}
	}
}

// A query reads an index while Apply writes to it (the file is in WAL mode), and sees a consistent
// snapshot either side of each change.
func TestQueryWhileApplying(t *testing.T) {
	t.Parallel()
	sb, sources, _ := sandboxFixture(t)
	ours := sources[0].Index
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
			err := ours.Apply(t.Context(), fmt.Sprintf("c%d", i+2), []index.Change{{Path: "notes/live.md", Version: version(content), Content: []byte(content)}})
			if err != nil {
				t.Errorf("Apply: got error %v, want nil", err)
				return
			}
		}
	})
	for range 20 {
		rows, err := runQuery(t, sb, sources[:1], "SELECT count(*) FROM documents WHERE path = 'notes/live.md'")
		if err != nil {
			t.Fatalf("Query: got error %v, want nil", err)
		}
		if n := rows.Rows[0][0]; n != int64(0) && n != int64(1) {
			t.Errorf("count: got %v, want 0 or 1", n)
		}
	}
	close(stop)
	wg.Wait()
}

func TestQueryBrokenDocuments(t *testing.T) {
	t.Parallel()
	sb, sources, _ := sandboxFixture(t)
	bad := "# Deploy\n\nNew steps \xff\n"
	err := sources[0].Index.Apply(t.Context(), "c2", []index.Change{{Path: "runbooks/deploy.md", Version: version(bad), Content: []byte(bad)}})
	if err != nil {
		t.Fatalf("Apply: got error %v, want nil", err)
	}
	testCases := []struct {
		name string
		sql  string
		want [][]any
	}{
		{
			name: "kept with its version",
			sql:  `SELECT vault, path, broken, body, version FROM documents WHERE status = 'broken'`,
			want: [][]any{{"ours/kb", "runbooks/deploy.md", "unusable document: not UTF-8", "", version(bad)}},
		},
		{name: "others", sql: `SELECT count(*) FROM documents WHERE broken IS NULL`, want: [][]any{{int64(3)}}},
		{name: "body not matched", sql: `SELECT path FROM fts WHERE body MATCH 'steps'`, want: [][]any{}},
		{name: "title not matched", sql: `SELECT path FROM fts WHERE title MATCH 'deploy'`, want: [][]any{}},
		{name: "not in fts", sql: `SELECT count(*) FROM fts`, want: [][]any{{int64(3)}}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			rows, err := runQuery(t, sb, sources, testCase.sql)
			if err != nil {
				t.Fatalf("Query: got error %v, want nil", err)
			}
			if diff := cmp.Diff(testCase.want, rows.Rows); diff != "" {
				t.Errorf("rows (-want +got):\n%s", diff)
			}
		})
	}
}

func BenchmarkQuery(b *testing.B) {
	sb := newSandbox(b)
	src := benchIndex(b)
	for b.Loop() {
		_, err := runQuery(b, sb, src, "SELECT f.path, d.title FROM fts f JOIN documents d USING (vault, path) WHERE f.body MATCH 'fleet' LIMIT 20")
		if err != nil {
			b.Fatalf("Query: got error %v, want nil", err)
		}
	}
}
