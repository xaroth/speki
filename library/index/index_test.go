// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index_test

import (
	"errors"
	"fmt"
	"maps"
	"sync"
	"testing"

	"github.com/xaroth/speki/library/index"
)

var (
	newIndex = index.NewTestIndex
	version  = index.Version
	changes  = index.Changes
)

var fleet = map[string]string{
	"ops/fleet-doctrine.md": "---\ntitle: Fleet doctrine\ndescription: Which fittings we fly.\ntags: [fleet, doctrine]\n---\n" +
		"# Fleet doctrine\n\nWe are configuring kubernetes clusters for the fleet tools.\n\nSee [roles](fleet-roles.md).\n",
	"ops/fleet-roles.md": "---\ntitle: Fleet roles\ntags: [fleet]\n---\nWho does what. Logistics configured by [[fleet-doctrine]].\n",
	"services/oauth.md":  "---\ntitle: OAuthProvider\ndescription: Sign-in service.\n---\nThe configuration of the provider.\n",
	"old/nomad.md":       "---\ntitle: Nomad setup\nstatus: deprecated\ntags: [fleet]\n---\nThe old configuration with nomad and fleet.\n",
	"notes/cache.md":     "---\ntitle: Cache invalidation\n---\nHard problems: cache invalidation and naming things.\n",
	"README.md":          "# About\n\nKnowledge about our fleet.\n",
}

// corpus is fleet plus unrelated documents, so the words the tests search for are rare, as they are in a
// real vault, and BM25 has something to work with.
func corpus() map[string]string {
	files := maps.Clone(fleet)
	topics := []string{"mining yields", "market orders", "industry jobs", "planetary interaction", "wormhole space",
		"skill training", "corporation taxes", "structure fuel", "jump bridges", "moon drilling", "contracts", "insurance"}
	for i, topic := range topics {
		files[fmt.Sprintf("misc/topic-%02d.md", i)] = fmt.Sprintf("# About %s\n\nNotes on %s and related procedures.\n", topic, topic)
	}
	return files
}

func search(tb testing.TB, sources []index.Source, q string, opt index.Options) index.Results {
	tb.Helper()
	res, err := index.Search(tb.Context(), sources, q, opt)
	if err != nil {
		tb.Fatalf("Search(%q): got error %v, want nil", q, err)
	}
	return res
}

func hitPaths(res index.Results) []string {
	var out []string
	for _, h := range res.Hits {
		out = append(out, h.Path)
	}
	return out
}

func kb(x *index.Index) []index.Source { return []index.Source{{Name: "kb", Index: x}} }

func TestClosedIndex(t *testing.T) {
	t.Parallel()
	x := newIndex(t, fleet)
	x.Close()
	x.Close() // twice is fine

	_, err := index.Search(t.Context(), kb(x), "fleet", index.Options{})
	if !errors.Is(err, index.ErrClosed) {
		t.Errorf("Search: got %v, want %v", err, index.ErrClosed)
	}
	_, err = x.Document(t.Context(), "README.md")
	if !errors.Is(err, index.ErrClosed) {
		t.Errorf("Document: got %v, want %v", err, index.ErrClosed)
	}
	if got := x.Commit(); got != "" {
		t.Errorf("Commit: got %q, want empty", got)
	}
}

// Close can race a build and applies: the index stays consistent and doesn't start work after closing.
func TestCloseRacesWork(t *testing.T) {
	t.Parallel()
	for i := range 10 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			x := newIndex(t, fleet)
			var wg sync.WaitGroup
			wg.Go(func() { _ = x.Build(t.Context(), "c2", changes(fleet)) })
			wg.Go(func() { _ = x.Apply(t.Context(), "c3", nil) })
			wg.Go(x.Close)
			wg.Wait()
			if err := x.Build(t.Context(), "c4", changes(fleet)); !errors.Is(err, index.ErrClosed) {
				t.Fatalf("Build after Close: got %v, want %v", err, index.ErrClosed)
			}
		})
	}
}
