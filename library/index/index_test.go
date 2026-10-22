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

func TestApply(t *testing.T) {
	t.Parallel()
	x := newIndex(t, fleet)
	src := kb(x)

	err := x.Apply(t.Context(), "c2", []index.Change{
		{Path: "notes/cache.md"}, // deleted
		{Path: "ops/fleet-roles.md", Version: version("x"), Content: []byte("# Fleet roles\n\nNow about zeppelins.\n")},
		{Path: "new.md", Version: version("y"), Content: []byte("# New\n\nFreshly written quokka notes.\n")},
		{Path: "bad.md", Version: version("z"), Content: []byte("not utf-8 \xff")},
	})
	if err != nil {
		t.Fatalf("Apply: got error %v, want nil", err)
	}
	if got := x.Commit(); got != "c2" {
		t.Errorf("commit: got %q, want c2", got)
	}

	testCases := []struct {
		name      string
		query     string
		want      []string
		corrected map[string]string
	}{
		// Known straight away, before the vocabulary reload finishes, so not corrected.
		{name: "added", query: "quokka", want: []string{"new.md"}},
		{name: "deleted", query: "invalidation", want: nil},
		{name: "changed", query: "zeppelins", want: []string{"ops/fleet-roles.md"}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			res := search(t, src, testCase.query, index.Options{})
			if diff := cmp.Diff(testCase.want, hitPaths(res), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("hits (-want +got):\n%s", diff)
			}
			if len(res.Corrected) != 0 {
				t.Errorf("corrected: got %v, want none", res.Corrected)
			}
		})
	}

	d, err := x.Document(t.Context(), "bad.md")
	if err != nil {
		t.Fatalf("Document: got error %v, want nil", err)
	}
	if d.Status != index.StatusBroken || d.Version != version("z") {
		t.Errorf("unusable file: got status %q version %q, want %q %q", d.Status, d.Version, index.StatusBroken, version("z"))
	}
}

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

func TestInterruptedBuildIsDiscarded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	x, err := index.Open(t.Context(), dir, index.Discard())
	if err != nil {
		t.Fatalf("Open: got error %v, want nil", err)
	}
	err = x.Build(t.Context(), "good", changes(fleet))
	if err != nil {
		t.Fatalf("Build: got error %v, want nil", err)
	}
	// A build that fails half way, as if speki were killed while the transaction was open.
	failing := func(yield func(index.Change, error) bool) {
		if !yield(index.Change{Path: "a.md", Version: "v", Content: []byte("# A\n")}, nil) {
			return
		}
		yield(index.Change{}, errors.New("killed"))
	}
	err = x.Build(t.Context(), "half", failing)
	if err == nil {
		t.Fatal("failing build: got nil error, want one")
	}
	if got := x.Commit(); got != "good" {
		t.Errorf("commit after failed build: got %q, want good", got)
	}
	x.Close()

	x, err = index.Open(t.Context(), dir, index.Discard())
	if err != nil {
		t.Fatalf("reopen: got error %v, want nil", err)
	}
	t.Cleanup(x.Close)
	if got := x.Commit(); got != "good" {
		t.Errorf("reopened commit: got %q, want good", got)
	}
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

// A file that becomes unusable is kept as broken at its new version, never at the old one: a write
// checked against the old version would overwrite a change nobody saw.
func TestBrokenDocuments(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	x := newIndex(t, fleet)
	src := kb(x)
	bad := "---\ntitle: About\n---\n# About\n\nKnowledge about our fleet. \xff\n"
	err := x.Apply(ctx, "c2", []index.Change{{Path: "README.md", Version: version(bad), Content: []byte(bad)}})
	if err != nil {
		t.Fatalf("Apply: got error %v, want nil", err)
	}
	if got := x.Commit(); got != "c2" {
		t.Errorf("commit: got %q, want c2", got)
	}
	applied, err := x.Document(ctx, "README.md")
	if err != nil {
		t.Fatalf("Document: got error %v, want nil", err)
	}
	want := index.Document{
		Path:     "README.md",
		Title:    "README.md",
		Status:   index.StatusBroken,
		Tags:     []string{},
		Aliases:  []string{},
		Related:  []string{},
		Extra:    map[string]any{},
		Bytes:    len(bad),
		Lines:    7,
		Version:  version(bad),
		Warnings: []string{},
		Broken:   "unusable document: not UTF-8",
	}
	if diff := cmp.Diff(want, applied); diff != "" {
		t.Errorf("after Apply (-want +got):\n%s", diff)
	}
	for _, q := range []string{"knowledge", "about", "readme"} {
		if got := hitPaths(search(t, src, q, index.Options{})); slices.Contains(got, "README.md") {
			t.Errorf("search %q: got %v, want no README.md", q, got)
		}
	}
	root, err := x.List(ctx, "", false, "", 1)
	if err != nil {
		t.Fatalf("List: got error %v, want nil", err)
	}
	if len(root) != 1 || root[0].Status != index.StatusBroken {
		t.Errorf("list: got %+v, want README.md as broken", root)
	}

	files := maps.Clone(fleet)
	files["README.md"] = bad
	built, err := newIndex(t, files).Document(ctx, "README.md")
	if err != nil {
		t.Fatalf("Document: got error %v, want nil", err)
	}
	if diff := cmp.Diff(applied, built); diff != "" {
		t.Errorf("Build and Apply disagree (-apply +build):\n%s", diff)
	}

	good := "# About\n\nFixed.\n"
	err = x.Apply(ctx, "c3", []index.Change{{Path: "README.md", Version: version(good), Content: []byte(good)}})
	if err != nil {
		t.Fatalf("Apply: got error %v, want nil", err)
	}
	d, err := x.Document(ctx, "README.md")
	if err != nil {
		t.Fatalf("Document: got error %v, want nil", err)
	}
	if d.Status != "active" || d.Broken != "" || d.Version != version(good) {
		t.Errorf("after the fix: got %+v, want active at %s", d, version(good))
	}
	if got := hitPaths(search(t, src, "fixed", index.Options{})); !slices.Contains(got, "README.md") {
		t.Errorf("search fixed: got %v, want README.md", got)
	}
}

// A build stops reading files once an insert fails: the build fails anyway.
func TestBuildStopsAtAFailedInsert(t *testing.T) {
	t.Parallel()
	x, err := index.Open(t.Context(), t.TempDir(), index.Discard())
	if err != nil {
		t.Fatalf("Open: got error %v, want nil", err)
	}
	t.Cleanup(x.Close)
	const total = 5000
	read := 0
	files := func(yield func(index.Change, error) bool) {
		for i := range total {
			read++
			path := fmt.Sprintf("d%d.md", i)
			if i == 1 {
				path = "d0.md" // the same path twice fails its insert
			}
			content := fmt.Sprintf("# D%d\n", i)
			if !yield(index.Change{Path: path, Version: version(content), Content: []byte(content)}, nil) {
				return
			}
		}
	}
	err = x.Build(t.Context(), "c1", files)
	if err == nil || !strings.Contains(err.Error(), "d0.md: insert document") {
		t.Fatalf("Build: got %v, want a d0.md insert error", err)
	}
	if read > total/2 {
		t.Errorf("files read after the insert failed: got %d, want at most %d", read, total/2)
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
