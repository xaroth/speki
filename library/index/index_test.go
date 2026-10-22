package index_test

import (
	"errors"
	"fmt"
	"maps"
	"math"
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

func TestSearch(t *testing.T) {
	t.Parallel()
	src := kb(newIndex(t, corpus()))

	testCases := []struct {
		name      string
		query     string
		opt       index.Options
		want      []string // in order
		corrected map[string]string
		relaxed   bool
	}{
		{name: "stemming", query: "configured", want: []string{"services/oauth.md", "ops/fleet-roles.md", "ops/fleet-doctrine.md", "old/nomad.md"}},
		{name: "typo corrected", query: "kubrenetes", want: []string{"ops/fleet-doctrine.md"}, corrected: map[string]string{"kubrenetes": "kubernetes"}},
		{name: "typo with the same stem", query: "kubernets", want: []string{"ops/fleet-doctrine.md"}},
		{name: "last word as prefix", query: "doctr", want: []string{"ops/fleet-doctrine.md", "ops/fleet-roles.md"}},
		{name: "substring of a name", query: "auth", want: []string{"services/oauth.md"}},
		{name: "phrase", query: `"cache invalidation"`, want: []string{"notes/cache.md"}},
		// fleet-roles links to fleet-doctrine, so "doctrine" is in its body too.
		{name: "exclusions", query: "fleet -nomad -doctrine", want: []string{"README.md"}},
		{name: "all words", query: "fleet tools", want: []string{"ops/fleet-doctrine.md"}},
		{name: "relaxed to any word", query: "nomad kubernetes", want: []string{"old/nomad.md", "ops/fleet-doctrine.md"}, relaxed: true},
		{name: "ranking", query: "fleet", want: []string{"ops/fleet-roles.md", "ops/fleet-doctrine.md", "README.md", "old/nomad.md"}},
		{name: "tag filter", query: "fleet", opt: index.Options{Tags: []string{"doctrine"}}, want: []string{"ops/fleet-doctrine.md"}},
		{name: "path prefix", query: "fleet", opt: index.Options{PathPrefix: "ops"}, want: []string{"ops/fleet-roles.md", "ops/fleet-doctrine.md"}},
		{name: "exclude deprecated", query: "nomad", opt: index.Options{ExcludeDeprecated: true}, want: nil},
		{name: "limit", query: "fleet", opt: index.Options{Limit: 1}, want: []string{"ops/fleet-roles.md"}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			res := search(t, src, testCase.query, testCase.opt)
			if diff := cmp.Diff(testCase.want, hitPaths(res), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("hits (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(testCase.corrected, res.Corrected, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("corrected (-want +got):\n%s", diff)
			}
			if res.Relaxed != testCase.relaxed {
				t.Errorf("relaxed: got %v, want %v", res.Relaxed, testCase.relaxed)
			}
		})
	}
}

func TestSearchDetails(t *testing.T) {
	t.Parallel()
	src := kb(newIndex(t, corpus()))

	res := search(t, src, "invalidation", index.Options{})
	want := []index.Hit{{
		Source:  "kb",
		Path:    "notes/cache.md",
		Title:   "Cache invalidation",
		Tags:    []string{},
		Status:  "active",
		Version: version(fleet["notes/cache.md"]),
		Snippet: "Hard problems: cache **invalidation** and naming things.",
	}}
	if diff := cmp.Diff(want, res.Hits, cmpopts.IgnoreFields(index.Hit{}, "Score"), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("hits (-want +got):\n%s", diff)
	}
	if len(res.Hits) == 1 && res.Hits[0].Score <= 0 {
		t.Errorf("score: got %v, want > 0", res.Hits[0].Score)
	}

	nomad := search(t, src, "nomad", index.Options{})
	if nomad.Hits[0].Score >= res.Hits[0].Score*2 {
		t.Errorf("deprecated score: got %v, want below %v", nomad.Hits[0].Score, res.Hits[0].Score*2)
	}

	_, err := index.Search(t.Context(), src, "-only -exclusions", index.Options{})
	if !errors.Is(err, index.ErrEmptyQuery) {
		t.Errorf("only exclusions: got %v, want %v", err, index.ErrEmptyQuery)
	}
}

func TestSearchAcrossSources(t *testing.T) {
	t.Parallel()
	withFiller := func(files map[string]string) map[string]string {
		for k, v := range corpus() {
			if strings.HasPrefix(k, "misc/") {
				files[k] = v
			}
		}
		return files
	}
	a := newIndex(t, withFiller(map[string]string{"x.md": "# Kubernetes\n\nkubernetes kubernetes kubernetes\n"}))
	b := newIndex(t, withFiller(map[string]string{"y.md": "# Other\n\nmentions kubernetes once in a longer text about other things\n"}))
	c := newIndex(t, withFiller(map[string]string{"z.md": "# Typo\n\nreplcation is how this vault spells it\n"}))
	d := newIndex(t, withFiller(map[string]string{"w.md": "# Spelled right\n\nreplication done properly\n"}))

	testCases := []struct {
		name      string
		sources   []index.Source
		query     string
		want      []string
		corrected map[string]string
	}{
		{
			name:    "hits merged by score",
			sources: []index.Source{{Name: "a", Index: a}, {Name: "b", Index: b}},
			query:   "kubernetes",
			want:    []string{"a:x.md", "b:y.md"},
		},
		{
			name:    "a word known in any source isn't corrected",
			sources: []index.Source{{Name: "c", Index: c}, {Name: "d", Index: d}},
			query:   "replcation",
			want:    []string{"c:z.md"},
		},
		{
			name:      "only the sources searched correct",
			sources:   []index.Source{{Name: "d", Index: d}},
			query:     "replcation",
			want:      []string{"d:w.md"},
			corrected: map[string]string{"replcation": "replication"},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			res := search(t, testCase.sources, testCase.query, index.Options{})
			var got []string
			for _, h := range res.Hits {
				got = append(got, h.Source+":"+h.Path)
			}
			if diff := cmp.Diff(testCase.want, got); diff != "" {
				t.Errorf("hits (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(testCase.corrected, res.Corrected, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("corrected (-want +got):\n%s", diff)
			}
		})
	}
}

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

// The examples in docs/functional/search-and-query.md that TestSearch doesn't cover.
func TestSearchExamples(t *testing.T) {
	t.Parallel()
	files := corpus()
	maps.Copy(files, map[string]string{
		"phrase/adjacent.md": "# Adjacent\n\nOur caching invalidations are slow.\n",
		"phrase/apart.md":    "# Apart\n\nThe cache is warm, and invalidation happens nightly.\n",
		"weights/title.md":   "---\ntitle: Zeppelin hangar\n---\nWhere the big ones park.\n",
		"weights/tags.md":    "---\ntitle: Parking\ntags: [zeppelin]\n---\nWhere the big ones park.\n",
		"weights/body.md":    "---\ntitle: Parking lots\n---\nWhere the big ones park, a zeppelin among them.\n",
		"status/active.md":   "---\ntitle: Dirigible notes\n---\nAll about the dirigible fleet.\n",
		"status/draft.md":    "---\ntitle: Dirigible notes\nstatus: draft\n---\nAll about the dirigible fleet.\n",
		"status/old.md":      "---\ntitle: Dirigible notes\nstatus: deprecated\n---\nAll about the dirigible fleet.\n",
		"snippet/long.md":    "# Long\n\n" + strings.Repeat("filler words go here ", 40) + "the blimp is here " + strings.Repeat("more filler text now ", 40) + "\n",
		"snippet/title.md":   "---\ntitle: Airship\ndescription: Lighter than air.\n---\nSome **bold** text about nothing in particular.\n",
	})
	src := kb(newIndex(t, files))
	score := func(tb testing.TB, res index.Results, path string) float64 {
		tb.Helper()
		for _, h := range res.Hits {
			if h.Path == path {
				return h.Score
			}
		}
		tb.Fatalf("hits: got %v, want %s", hitPaths(res), path)
		return 0
	}

	t.Run("quoted phrases are adjacent, after stemming", func(t *testing.T) {
		t.Parallel()
		got := hitPaths(search(t, src, `"cache invalidation"`, index.Options{}))
		if !slices.Contains(got, "phrase/adjacent.md") || slices.Contains(got, "phrase/apart.md") {
			t.Errorf("hits: got %v, want phrase/adjacent.md without phrase/apart.md", got)
		}
	})
	t.Run("title outranks tags outranks body", func(t *testing.T) {
		t.Parallel()
		got := hitPaths(search(t, src, "zeppelin", index.Options{}))
		if diff := cmp.Diff([]string{"weights/title.md", "weights/tags.md", "weights/body.md"}, got); diff != "" {
			t.Errorf("hits (-want +got):\n%s", diff)
		}
	})
	t.Run("deprecated is halved, draft keeps its score", func(t *testing.T) {
		t.Parallel()
		res := search(t, src, "dirigible", index.Options{})
		active, draft, old := score(t, res, "status/active.md"), score(t, res, "status/draft.md"), score(t, res, "status/old.md")
		if draft != active {
			t.Errorf("draft score: got %v, want %v", draft, active)
		}
		if math.Abs(old-active/2) > 1e-9 {
			t.Errorf("deprecated score: got %v, want %v", old, active/2)
		}
	})
	t.Run("snippet of at most 32 words", func(t *testing.T) {
		t.Parallel()
		res := search(t, src, "blimp", index.Options{})
		snippet := res.Hits[0].Snippet
		if n := len(strings.Fields(snippet)); n > 32+2 { // the ellipses
			t.Errorf("snippet words: got %d, want at most 34: %q", n, snippet)
		}
		if !strings.Contains(snippet, "**blimp**") {
			t.Errorf("snippet: got %q, want **blimp** in it", snippet)
		}
	})
	t.Run("a match only in the title shows the description", func(t *testing.T) {
		t.Parallel()
		res := search(t, src, "airship", index.Options{})
		if got := res.Hits[0].Snippet; got != "Lighter than air." {
			t.Errorf("snippet: got %q, want %q", got, "Lighter than air.")
		}
	})
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

func TestSearchRankingAndInput(t *testing.T) {
	t.Parallel()
	files := corpus()
	for i := range 40 { // deprecated, matching a little better than the active one, but not twice as well
		files[fmt.Sprintf("old/airship-%02d.md", i)] = "---\ntitle: Airship\nstatus: deprecated\n---\nAirship notes.\n"
	}
	for i := range 100 { // so that "airship" is a rare word, and BM25 tells the documents apart
		files[fmt.Sprintf("filler/%03d.md", i)] = fmt.Sprintf("# Filler %d\n\nNothing of interest here.\n", i)
	}
	maps.Copy(files, map[string]string{
		"new/airship.md":  "---\ntitle: Airship guide\n---\nNotes on the hangar.\n",
		"body/zep.md":     "# Notes\n\nA zeppelin.\n",
		"words/run.md":    "# Jobs\n\nWe run the job nightly.\n",
		"words/rune-1.md": "# Rune one\n\nA rune.\n",
		"words/rune-2.md": "# Rune two\n\nA rune.\n",
		"words/rune-3.md": "# Rune three\n\nA rune.\n",
		"words/rune-4.md": "# Rune four\n\nA rune.\n",
		"words/rune-5.md": "# Rune five\n\nA rune.\n",
		"names/micro.md":  "---\ntitle: Microservices\n---\nSmall things.\n",
		"names/cat.md":    "# Cat\n\nThe cat sat.\n",
	})
	src := kb(newIndex(t, files))

	t.Run("deprecated documents don't crowd out an active one", func(t *testing.T) {
		t.Parallel()
		got := hitPaths(search(t, src, "airship", index.Options{}))
		if len(got) == 0 || got[0] != "new/airship.md" {
			t.Errorf("hits: got %v, want new/airship.md first", got)
		}
	})
	t.Run("a word found by its stem isn't corrected", func(t *testing.T) {
		t.Parallel()
		res := search(t, src, "runs", index.Options{})
		if len(res.Corrected) != 0 || !slices.Contains(hitPaths(res), "words/run.md") {
			t.Errorf("got hits %v, corrected %v; want words/run.md, no corrections", hitPaths(res), res.Corrected)
		}
	})
	t.Run("substring matches rank below full-text matches", func(t *testing.T) {
		t.Parallel()
		// "service" is in most documents, so BM25 gives full-text matches only a tiny score.
		common := map[string]string{
			"names/micro.md":   "---\ntitle: Microservices\n---\nSmall things.\n",
			"names/service.md": "---\ntitle: Service\n---\nAbout the service.\n",
		}
		for i := range 20 {
			common[fmt.Sprintf("common/%02d.md", i)] = fmt.Sprintf("# Note %d\n\nThe service runs.\n", i)
		}
		got := hitPaths(search(t, kb(newIndex(t, common)), "service", index.Options{Limit: 50}))
		if len(got) == 0 || got[len(got)-1] != "names/micro.md" {
			t.Errorf("hits: got %v, want names/micro.md last", got)
		}
	})
	t.Run("folder names aren't matched as names", func(t *testing.T) {
		t.Parallel()
		got := hitPaths(search(t, src, "words", index.Options{}))
		for _, p := range got {
			if strings.HasPrefix(p, "words/") {
				t.Errorf("hits: got %v, want none under words/", got)
				break
			}
		}
	})
	t.Run("a quoted word matches as written", func(t *testing.T) {
		t.Parallel()
		if got := hitPaths(search(t, src, "zeppel", index.Options{})); !slices.Contains(got, "body/zep.md") {
			t.Fatalf("unquoted, as a prefix: got %v, want body/zep.md", got)
		}
		res := search(t, src, `"zeppel"`, index.Options{})
		if len(res.Hits) != 0 || len(res.Corrected) != 0 {
			t.Errorf("quoted: got hits %v, corrected %v; want none", hitPaths(res), res.Corrected)
		}
	})
	t.Run("three-letter typos", func(t *testing.T) {
		t.Parallel()
		res := search(t, src, "cot sat", index.Options{})
		if got := res.Corrected["cot"]; got != "cat" {
			t.Errorf("correction of cot: got %q, want cat", got)
		}
	})

	many := make([]string, 0, index.MaxTerms+1)
	for i := range cap(many) {
		many = append(many, fmt.Sprintf("w%d", i))
	}
	exclusions := "fleet"
	for i := range index.MaxTerms - 1 {
		exclusions += fmt.Sprintf(" -x%d", i)
	}
	sizeCases := []struct {
		name  string
		query string
		want  error
	}{
		{name: "too many words", query: strings.Join(many, " "), want: index.ErrTooManyWords},
		{name: "one word many times", query: strings.Repeat("fleet ", 500)},
		{name: "many exclusions", query: exclusions},
	}
	for _, testCase := range sizeCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := index.Search(t.Context(), src, testCase.query, index.Options{})
			if !errors.Is(err, testCase.want) {
				t.Errorf("got error %v, want %v", err, testCase.want)
			}
		})
	}
}

// Filters apply before the limit: documents that rank above every filtered match don't push those out.
func TestSearchFiltersBelowManyBetterMatches(t *testing.T) {
	t.Parallel()
	// Long bodies with few matches rank low.
	padding := func(n int) string { return strings.Repeat("words ", n) }
	files := map[string]string{
		"keep/a.md":  "---\ntitle: First\ntags: [keep]\n---\nGondola gondola " + padding(40) + "\n",
		"keep/b.md":  "---\ntitle: Second\ntags: [keep]\n---\nGondola " + padding(40) + "\n",
		"keep/c.md":  "---\ntitle: Third\ntags: [keep]\n---\nGondola " + padding(80) + "\n",
		"other/d.md": "# Fourth\n\nGondola " + padding(160) + "\n",
	}
	for i := range 30 { // deprecated, so halved, but matching in the title they still rank above all of the above
		files[fmt.Sprintf("popular/%02d.md", i)] = "---\ntitle: Gondola\nstatus: deprecated\n---\nGondola.\n"
	}
	for i := range 100 { // so that "gondola" is a rare word, and BM25 tells the documents apart
		files[fmt.Sprintf("filler/%03d.md", i)] = fmt.Sprintf("# Filler %d\n\nNothing of interest here.\n", i)
	}
	src := kb(newIndex(t, files))

	top := hitPaths(search(t, src, "gondola", index.Options{Limit: 30}))
	for _, p := range top {
		if !strings.HasPrefix(p, "popular/") {
			t.Fatalf("unfiltered hits: got %v, want only popular/ documents", top)
		}
	}

	testCases := []struct {
		name string
		opt  index.Options
		want []string // in order
	}{
		{name: "tag", opt: index.Options{Tags: []string{"keep"}}, want: []string{"keep/a.md", "keep/b.md", "keep/c.md"}},
		{name: "tag with a limit", opt: index.Options{Tags: []string{"keep"}, Limit: 2}, want: []string{"keep/a.md", "keep/b.md"}},
		{name: "exclude deprecated", opt: index.Options{ExcludeDeprecated: true}, want: []string{"keep/a.md", "keep/b.md", "keep/c.md", "other/d.md"}},
		{name: "tag and exclude deprecated", opt: index.Options{Tags: []string{"keep"}, ExcludeDeprecated: true}, want: []string{"keep/a.md", "keep/b.md", "keep/c.md"}},
		{name: "path prefix", opt: index.Options{PathPrefix: "keep"}, want: []string{"keep/a.md", "keep/b.md", "keep/c.md"}},
		{name: "path prefix of a longer folder name", opt: index.Options{PathPrefix: "kee"}, want: nil},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got := hitPaths(search(t, src, "gondola", testCase.opt))
			if diff := cmp.Diff(testCase.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("hits (-want +got):\n%s", diff)
			}
		})
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

// benchIndex is corpus with enough filler that BM25 and the vocabulary have work to do.
func benchIndex(b *testing.B) []index.Source {
	b.Helper()
	files := corpus()
	for i := range 1000 {
		files[fmt.Sprintf("filler/%04d.md", i)] = fmt.Sprintf("# Filler %d\n\nNotes on configuration number %d for the fleet tools.\n", i, i)
	}
	return kb(newIndex(b, files))
}

func BenchmarkSearch(b *testing.B) {
	src := benchIndex(b)
	for _, q := range []string{"fleet", "kubrenetes", "fleet tools -nomad"} {
		b.Run(q, func(b *testing.B) {
			for b.Loop() {
				_, err := index.Search(b.Context(), src, q, index.Options{})
				if err != nil {
					b.Fatalf("Search(%q): got error %v, want nil", q, err)
				}
			}
		})
	}
}
