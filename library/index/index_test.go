// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index_test

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"golang.org/x/text/unicode/norm"

	"github.com/xaroth/speki/document"
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

func TestDocument(t *testing.T) {
	t.Parallel()
	x := newIndex(t, fleet)

	got, err := x.Document(t.Context(), "ops/fleet-doctrine.md")
	if err != nil {
		t.Fatalf("Document: got error %v, want nil", err)
	}
	content := fleet["ops/fleet-doctrine.md"]
	want := index.Document{
		Path:        "ops/fleet-doctrine.md",
		Title:       "Fleet doctrine",
		Description: "Which fittings we fly.",
		Status:      "active",
		Tags:        []string{"fleet", "doctrine"},
		Aliases:     []string{},
		Related:     []string{},
		Extra:       map[string]any{},
		Bytes:       len(content),
		Lines:       11,
		Version:     version(content),
		Warnings:    []string{},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("document (-want +got):\n%s", diff)
	}

	_, err = x.Document(t.Context(), "missing.md")
	if !errors.Is(err, index.ErrNotFound) {
		t.Errorf("missing document: got %v, want %v", err, index.ErrNotFound)
	}
}

func TestLinkedFrom(t *testing.T) {
	t.Parallel()
	x := newIndex(t, fleet)
	testCases := []struct {
		name string
		path string
		want []string
	}{
		{name: "markdown link", path: "ops/fleet-roles.md", want: []string{"ops/fleet-doctrine.md"}},
		{name: "wikilink", path: "ops/fleet-doctrine.md", want: []string{"ops/fleet-roles.md"}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, err := x.LinkedFrom(t.Context(), testCase.path)
			if err != nil {
				t.Fatalf("LinkedFrom: got error %v, want nil", err)
			}
			if diff := cmp.Diff(testCase.want, got); diff != "" {
				t.Errorf("linked from (-want +got):\n%s", diff)
			}
		})
	}
}

func TestResolves(t *testing.T) {
	t.Parallel()
	x := newIndex(t, fleet)
	testCases := []struct {
		name string
		link document.Link
		want bool
	}{
		{name: "path", link: document.Link{Target: "fleet-roles.md", Candidate: "ops/fleet-roles.md", Kind: document.LinkRelated}, want: true},
		{name: "missing path", link: document.Link{Target: "nope.md", Candidate: "ops/nope.md", Kind: document.LinkRelated}},
		{name: "wikilink", link: document.Link{Target: "[[fleet-roles]]", Candidate: "fleet-roles", Kind: document.LinkWiki}, want: true},
		{name: "missing wikilink", link: document.Link{Target: "[[nope]]", Candidate: "nope", Kind: document.LinkWiki}},
		{name: "outside the vault", link: document.Link{Target: "https://example.com", Kind: document.LinkRelated}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, err := x.Resolves(t.Context(), testCase.link)
			if err != nil {
				t.Fatalf("Resolves: got error %v, want nil", err)
			}
			if got != testCase.want {
				t.Errorf("got %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestList(t *testing.T) {
	t.Parallel()
	x := newIndex(t, fleet)
	root := []string{"README.md", "notes/", "old/", "ops/", "services/"}

	testCases := []struct {
		name      string
		folder    string
		recursive bool
		after     string
		want      []string
	}{
		{name: "root", want: root},
		{name: "folder", folder: "ops/", want: []string{"ops/fleet-doctrine.md", "ops/fleet-roles.md"}},
		{name: "recursive page", folder: "ops", recursive: true, after: "ops/fleet-doctrine.md", want: []string{"ops/fleet-roles.md"}},
		{name: "recursive", recursive: true, want: slices.Sorted(maps.Keys(fleet))},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			entries, err := x.List(t.Context(), testCase.folder, testCase.recursive, testCase.after, 100)
			if err != nil {
				t.Fatalf("List: got error %v, want nil", err)
			}
			var got []string
			for _, e := range entries {
				got = append(got, e.Path)
			}
			if diff := cmp.Diff(testCase.want, got); diff != "" {
				t.Errorf("paths (-want +got):\n%s", diff)
			}
		})
	}

	t.Run("pages", func(t *testing.T) {
		t.Parallel()
		var got []string
		after := ""
		for {
			page, err := x.List(t.Context(), "", false, after, 2)
			if err != nil {
				t.Fatalf("List: got error %v, want nil", err)
			}
			if len(page) == 0 {
				break
			}
			for _, e := range page {
				got = append(got, e.Path)
			}
			after = page[len(page)-1].Path
		}
		if diff := cmp.Diff(root, got); diff != "" {
			t.Errorf("paged (-want +got):\n%s", diff)
		}
	})

	t.Run("entry", func(t *testing.T) {
		t.Parallel()
		entries, err := x.List(t.Context(), "ops/", false, "", 1)
		if err != nil {
			t.Fatalf("List: got error %v, want nil", err)
		}
		want := []index.Entry{{
			Path:        "ops/fleet-doctrine.md",
			Title:       "Fleet doctrine",
			Description: "Which fittings we fly.",
			Status:      "active",
			Version:     version(fleet["ops/fleet-doctrine.md"]),
		}}
		if diff := cmp.Diff(want, entries); diff != "" {
			t.Errorf("entries (-want +got):\n%s", diff)
		}
	})
}

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

func TestSearchWhileBuilding(t *testing.T) {
	t.Parallel()
	x := newIndex(t, corpus())
	errs := make(chan error, 6) // the builder's errors, reported from the test's goroutine
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 3 {
			errs <- x.Build(t.Context(), fmt.Sprint("c", i), changes(corpus()))
			errs <- x.Apply(t.Context(), fmt.Sprint("a", i), []index.Change{{Path: "notes/cache.md"}})
		}
	}()
	for searching := true; searching; {
		select {
		case <-done:
			searching = false
		default:
			_, err := index.Search(t.Context(), kb(x), "fleet", index.Options{})
			if err != nil {
				<-done
				t.Fatalf("Search: got error %v, want nil", err)
			}
		}
	}
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("build or apply: got error %v, want nil", err)
		}
	}
}

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

// Links typed in NFC find files named in NFD, as macOS names them, and the other way round.
func TestLinksAcrossNormalForms(t *testing.T) {
	t.Parallel()
	nfd := norm.NFD.String("café/menu.md")
	x := newIndex(t, map[string]string{
		nfd:              "# Menu\n",
		"index.md":       "# Index\n\n[menu](café/menu.md) and [[Menu]]\n",
		"résumé.md":      "# Résumé\n",
		"other/links.md": "# Links\n\n[cv](../" + norm.NFD.String("résumé.md") + ")\n",
		"İstanbul.md":    "# Istanbul\n",
		"trip.md":        "# Trip\n\n[[" + norm.NFD.String("İstanbul") + "]]\n",
	})

	rows := query(t, kb(x), "SELECT path, target_path FROM links ORDER BY path, kind")
	want := [][]any{{"index.md", nfd}, {"index.md", nfd}, {"other/links.md", "résumé.md"}, {"trip.md", "İstanbul.md"}}
	if diff := cmp.Diff(want, rows.Rows); diff != "" {
		t.Errorf("resolved links (-want +got):\n%s", diff)
	}

	for path, want := range map[string][]string{nfd: {"index.md"}, "résumé.md": {"other/links.md"}, "İstanbul.md": {"trip.md"}} {
		got, err := x.LinkedFrom(t.Context(), path)
		if err != nil {
			t.Fatalf("LinkedFrom(%q): got error %v, want nil", path, err)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("LinkedFrom(%q) (-want +got):\n%s", path, diff)
		}
	}
	if got := hitPaths(search(t, kb(x), "menu", index.Options{PathPrefix: "café"})); !slices.Equal(got, []string{nfd}) {
		t.Errorf("search under an NFC folder name: got %v, want [%s]", got, nfd)
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

// A link resolves to a document that isn't broken before a broken one, even when the broken one's path is
// shorter, and falls back to the broken one when it's the only match.
func TestLinksPreferDocumentsThatArentBroken(t *testing.T) {
	t.Parallel()
	bad := "# Deploy\n\nNot UTF-8 \xff\n"
	x := newIndex(t, map[string]string{
		"x/deploy.md":        bad,
		"runbooks/deploy.md": "# Deploy\n\nSteps.\n",
		"x/restart.md":       bad,
		"a.md":               "# A\n\nSee [[deploy]] and [[restart]].\n",
	})
	for path, want := range map[string][]string{
		"runbooks/deploy.md": {"a.md"},
		"x/deploy.md":        nil,
		"x/restart.md":       {"a.md"},
	} {
		got, err := x.LinkedFrom(t.Context(), path)
		if err != nil {
			t.Fatalf("LinkedFrom(%q): got error %v, want nil", path, err)
		}
		if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("LinkedFrom(%q) (-want +got):\n%s", path, diff)
		}
	}
	rows := query(t, kb(x), "SELECT target, target_path FROM links ORDER BY target")
	want := [][]any{{"[[deploy]]", "runbooks/deploy.md"}, {"[[restart]]", "x/restart.md"}}
	if diff := cmp.Diff(want, rows.Rows); diff != "" {
		t.Errorf("links (-want +got):\n%s", diff)
	}
}
