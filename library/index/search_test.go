// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index_test

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/xaroth/speki/library/index"
)

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
	if len(res.Hits) == 1 && res.Hits[0].Score != 1 {
		t.Errorf("score: got %v, want 1 for the best match", res.Hits[0].Score)
	}

	// The vault's best match, but deprecated.
	nomad := search(t, src, "nomad", index.Options{})
	if nomad.Hits[0].Score != 0.5 {
		t.Errorf("deprecated score: got %v, want 0.5", nomad.Hits[0].Score)
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
	largeFiles := withFiller(map[string]string{})
	for i := range 3 {
		largeFiles[fmt.Sprintf("zep-%d.md", i)] = fmt.Sprintf("# Zeppelin %d\n\nzeppelin%s\n", i, strings.Repeat(" filler", 10*i))
	}
	large := newIndex(t, largeFiles)
	small := newIndex(t, map[string]string{
		"zep.md":   "# Zeppelin\n\nzeppelin zeppelin\n",
		"other.md": "# Other\n\nmentions a zeppelin in a longer text about other things\n",
	})

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
			// "zeppelin" is in every document of the small vault, so bm25 scores it near zero there.
			name:    "the best of each source first, whatever its size",
			sources: []index.Source{{Name: "large", Index: large}, {Name: "small", Index: small}},
			query:   "zeppelin",
			want:    []string{"large:zep-0.md", "small:zep.md", "large:zep-1.md", "large:zep-2.md", "small:other.md"},
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
