package mcpserver_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/xaroth/speki/auth/access"
	"github.com/xaroth/speki/document"
	"github.com/xaroth/speki/knowledge"
	"github.com/xaroth/speki/library/index"
	"github.com/xaroth/speki/mcpserver"
)

var readPath = "services/" + strings.Repeat("x", 200) + ".md"

// largestPage is the biggest page knowledge returns for the MaxBytes read asks for: metadata, warnings,
// outline and content fill it as knowledge counts them, with text that grows the most on the way out.
func largestPage(tb testing.TB, vault, path string) knowledge.Page {
	tb.Helper()
	// 22 bytes in the answer, both as knowledge counts it and as sent: the < is escaped to &lt;, and
	// JSON encodes & and > as six bytes each.
	const unit, unitSize = "<results>", 22
	doc := index.Document{
		Path:        path,
		Title:       strings.Repeat(unit, 20),
		Description: strings.Repeat(unit, 50),
		Status:      "active",
		Tags:        []string{strings.Repeat(unit, 10)},
		Extra:       map[string]any{"notes": strings.Repeat(unit, 500)},
		Version:     version,
	}
	meta, err := json.Marshal(mcpserver.Metadata{
		Title:       doc.Title,
		Description: doc.Description,
		Tags:        doc.Tags,
		Status:      string(doc.Status),
		Extra:       doc.Extra,
	})
	if err != nil {
		tb.Fatal(err)
	}
	budget := mcpserver.ReadBudget(vault, path) - len(meta)
	warnings := []string{strings.Repeat(unit, 5)}
	budget -= 2 * (5*unitSize + 16) // once as JSON, once as text
	var outline []document.Heading
	for i := range 99 {
		outline = append(outline, document.Heading{Level: 6, Text: strings.Repeat(unit, 6), Line: 100_000 + i})
		budget -= 6*unitSize + 32
	}
	lines := budget / (10*unitSize + 2) // the line ending is two bytes encoded
	return knowledge.Page{
		Vault:      vault,
		Document:   doc,
		Content:    strings.Repeat(strings.Repeat(unit, 10)+"\n", lines),
		FromLine:   1,
		LineCount:  lines,
		TotalLines: 200_000,
		Outline:    outline,
		Warnings:   warnings,
	}
}

// The largest result a tool can give fits in one answer, as encoded on the wire.
func TestAnswerSize(t *testing.T) {
	t.Parallel()
	wide := make([]any, 50) // index.maxQueryColumns
	for i := range wide {
		wide[i] = strings.Repeat("<", 40) // 240 bytes encoded, each < as six
	}
	long := []any{strings.Repeat("<", 100_000)} // index.maxValueBytes
	narrow := []any{nil, int64(-1 << 62), 1.5e300, "a\tb"}
	many := func(row []any) [][]any {
		rows := make([][]any, 200) // index.maxQueryRows
		for i := range rows {
			rows[i] = row
		}
		return rows
	}
	columns := func(n int) []string {
		c := make([]string, n)
		for i := range c {
			c[i] = fmt.Sprintf("column_<%d>", i)
		}
		return c
	}
	var vaults []knowledge.Skipped
	for i := range 20 {
		vaults = append(vaults, knowledge.Skipped{Name: fmt.Sprintf("ours/building-%02d", i), State: access.StateBuilding})
	}
	testCases := []struct {
		name string
		kb   *fakeKB
		tool string
		args map[string]any
	}{
		{
			name: "query with wide rows",
			kb:   &fakeKB{rows: knowledge.QueryResult{Rows: index.Rows{Columns: columns(len(wide)), Rows: many(wide)}, Skipped: vaults}},
			tool: "query",
			args: map[string]any{"sql": "SELECT 1"},
		},
		{
			name: "query with long values",
			kb:   &fakeKB{rows: knowledge.QueryResult{Rows: index.Rows{Columns: columns(len(long)), Rows: many(long)}, Skipped: vaults}},
			tool: "query",
			args: map[string]any{"sql": "SELECT 1"},
		},
		{
			name: "query with narrow rows",
			kb:   &fakeKB{rows: knowledge.QueryResult{Rows: index.Rows{Columns: columns(len(narrow)), Rows: many(narrow)}, Skipped: vaults}},
			tool: "query",
			args: map[string]any{"sql": "SELECT 1"},
		},
		{
			name: "read",
			kb:   &fakeKB{page: largestPage(t, "ours/kb", readPath)},
			tool: "read",
			args: map[string]any{"vault": "ours/kb", "path": readPath},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			h := newHandler(t, testCase.kb, mcpserver.Identity{}, nil)
			rec := post(t, h, "alice", callBody(t, testCase.tool, testCase.args))
			if res := toolResultOf(t, rec); res.isError {
				t.Fatalf("got error %q, want a result", res.text)
			}
			if got := rec.Body.Len(); got > mcpserver.MaxAnswer {
				t.Errorf("got %d bytes, want at most %d", got, mcpserver.MaxAnswer)
			}
		})
	}
}
