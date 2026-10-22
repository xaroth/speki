// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index

// In-package: it clears the vocabulary, as if its reload after a change hadn't finished.

import "testing"

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
