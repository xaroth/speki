// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/xaroth/speki/document"
	"github.com/xaroth/speki/library/index"
)

// Answers carry the start of a long title or description, not all of it.
func TestLongTitlesAreCut(t *testing.T) {
	t.Parallel()
	title, desc := strings.Repeat("é", document.MaxTitle), strings.Repeat("quokka ", document.MaxDescription)
	x := newIndex(t, map[string]string{"long.md": "---\ntitle: " + title + "\ndescription: " + desc + "\n---\nBody.\n"})
	check := func(tb testing.TB, title, desc string) {
		tb.Helper()
		if len(title) > document.MaxTitle || !utf8.ValidString(title) {
			tb.Errorf("title: got %d bytes, valid UTF-8 %v; want at most %d, valid", len(title), utf8.ValidString(title), document.MaxTitle)
		}
		if len(desc) > document.MaxDescription || !strings.HasSuffix(desc, "…") {
			tb.Errorf("description: got %d bytes, want at most %d ending in …", len(desc), document.MaxDescription)
		}
	}
	t.Run("document", func(t *testing.T) {
		t.Parallel()
		d, err := x.Document(t.Context(), "long.md")
		if err != nil {
			t.Fatalf("Document: got error %v, want nil", err)
		}
		check(t, d.Title, d.Description)
	})
	t.Run("list", func(t *testing.T) {
		t.Parallel()
		entries, err := x.List(t.Context(), "", true, "", 10)
		if err != nil || len(entries) != 1 {
			t.Fatalf("List: got %d entries, %v; want 1, nil", len(entries), err)
		}
		check(t, entries[0].Title, entries[0].Description)
	})
	t.Run("search", func(t *testing.T) {
		t.Parallel()
		res := search(t, kb(x), "quokka", index.Options{})
		if len(res.Hits) != 1 {
			t.Fatalf("hits: got %v, want long.md", hitPaths(res))
		}
		check(t, res.Hits[0].Title, res.Hits[0].Description)
		if n := len(res.Hits[0].Snippet); n > document.MaxDescription {
			t.Errorf("snippet: got %d bytes, want at most %d", n, document.MaxDescription)
		}
	})
}
