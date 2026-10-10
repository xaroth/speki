package index

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/mattn/go-sqlite3"
	"golang.org/x/text/unicode/norm"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func version(content string) string {
	sum := sha1.Sum([]byte(content))
	return hex.EncodeToString(sum[:])
}

func changes(files map[string]string) iter.Seq2[Change, error] {
	return func(yield func(Change, error) bool) {
		for _, p := range slices.Sorted(maps.Keys(files)) {
			if !yield(Change{Path: p, Version: version(files[p]), Content: []byte(files[p])}, nil) {
				return
			}
		}
	}
}

// newIndex builds an index of files and waits for its vocabulary.
func newIndex(t *testing.T, files map[string]string) *Index {
	t.Helper()
	x, err := Open(t.Context(), t.TempDir(), discard())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(x.Close)
	err = x.Build(t.Context(), "c1", changes(files))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	x.background.Wait()
	return x
}

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

func search(t *testing.T, sources []Source, q string, opt Options) Results {
	t.Helper()
	res, err := Search(t.Context(), sources, q, opt)
	if err != nil {
		t.Fatalf("Search(%q): %v", q, err)
	}
	return res
}

func hitPaths(res Results) []string {
	var out []string
	for _, h := range res.Hits {
		out = append(out, h.Path)
	}
	return out
}

func TestSearch(t *testing.T) {
	x := newIndex(t, corpus())
	src := []Source{{Name: "kb", Index: x}}

	tests := []struct {
		query     string
		opt       Options
		want      []string // in order
		corrected map[string]string
		relaxed   bool
	}{
		// Stemming: configuring, configured and configuration all match.
		{query: "configured", want: []string{"services/oauth.md", "ops/fleet-roles.md", "ops/fleet-doctrine.md", "old/nomad.md"}},
		{query: "kubrenetes", want: []string{"ops/fleet-doctrine.md"}, corrected: map[string]string{"kubrenetes": "kubernetes"}},
		// Same stem as "kubernetes": found by stemming, nothing to correct.
		{query: "kubernets", want: []string{"ops/fleet-doctrine.md"}},
		{query: "doctr", want: []string{"ops/fleet-doctrine.md", "ops/fleet-roles.md"}},
		{query: "auth", want: []string{"services/oauth.md"}},
		{query: `"cache invalidation"`, want: []string{"notes/cache.md"}},
		// fleet-roles links to fleet-doctrine, so "doctrine" is in its body too.
		{query: "fleet -nomad -doctrine", want: []string{"README.md"}},
		{query: "fleet tools", want: []string{"ops/fleet-doctrine.md"}},
		{query: "nomad kubernetes", want: []string{"old/nomad.md", "ops/fleet-doctrine.md"}, relaxed: true},
		{query: "fleet", want: []string{"ops/fleet-roles.md", "ops/fleet-doctrine.md", "README.md", "old/nomad.md"}},
		{query: "fleet", opt: Options{Tags: []string{"doctrine"}}, want: []string{"ops/fleet-doctrine.md"}},
		{query: "fleet", opt: Options{PathPrefix: "ops"}, want: []string{"ops/fleet-roles.md", "ops/fleet-doctrine.md"}},
		{query: "nomad", opt: Options{ExcludeDeprecated: true}, want: nil},
		{query: "fleet", opt: Options{Limit: 1}, want: []string{"ops/fleet-roles.md"}},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			res := search(t, src, tt.query, tt.opt)
			if diff := cmp.Diff(tt.want, hitPaths(res), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("hits (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.corrected, res.Corrected, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("corrected (-want +got):\n%s", diff)
			}
			if res.Relaxed != tt.relaxed {
				t.Errorf("relaxed = %v, want %v", res.Relaxed, tt.relaxed)
			}
		})
	}
}

func TestSearchDetails(t *testing.T) {
	x := newIndex(t, corpus())
	src := []Source{{Name: "kb", Index: x}}

	res := search(t, src, "invalidation", Options{})
	if len(res.Hits) != 1 {
		t.Fatalf("hits = %v", hitPaths(res))
	}
	h := res.Hits[0]
	if h.Source != "kb" || h.Title != "Cache invalidation" || h.Version != version(fleet["notes/cache.md"]) || h.Score <= 0 {
		t.Errorf("hit = %+v", h)
	}
	if h.Snippet != "Hard problems: cache **invalidation** and naming things." {
		t.Errorf("snippet = %q", h.Snippet)
	}

	res = search(t, src, "doctrine", Options{})
	if res.Hits[0].Snippet == "" || res.Hits[0].Tags[1] != "doctrine" {
		t.Errorf("hit = %+v", res.Hits[0])
	}

	nomad := search(t, src, "nomad", Options{})
	plain := search(t, src, "invalidation", Options{})
	if nomad.Hits[0].Score >= plain.Hits[0].Score*2 {
		t.Errorf("deprecated document isn't ranked lower: %v vs %v", nomad.Hits[0].Score, plain.Hits[0].Score)
	}

	_, err := Search(t.Context(), src, "-only -exclusions", Options{})
	if !errors.Is(err, ErrEmptyQuery) {
		t.Errorf("err = %v, want ErrEmptyQuery", err)
	}
}

func TestSearchAcrossSources(t *testing.T) {
	withFiller := func(files map[string]string) map[string]string {
		for k, v := range corpus() {
			if k[:5] == "misc/" {
				files[k] = v
			}
		}
		return files
	}
	a := newIndex(t, withFiller(map[string]string{"x.md": "# Kubernetes\n\nkubernetes kubernetes kubernetes\n"}))
	b := newIndex(t, withFiller(map[string]string{"y.md": "# Other\n\nmentions kubernetes once in a longer text about other things\n"}))
	c := newIndex(t, withFiller(map[string]string{"z.md": "# Typo\n\nreplcation is how this vault spells it\n"}))
	d := newIndex(t, withFiller(map[string]string{"w.md": "# Spelled right\n\nreplication done properly\n"}))

	res := search(t, []Source{{"a", a}, {"b", b}}, "kubernetes", Options{})
	got := []string{}
	for _, h := range res.Hits {
		got = append(got, h.Source+":"+h.Path)
	}
	if diff := cmp.Diff([]string{"a:x.md", "b:y.md"}, got); diff != "" {
		t.Errorf("merged hits (-want +got):\n%s", diff)
	}

	// A word known in any searched source isn't corrected.
	res = search(t, []Source{{"c", c}, {"d", d}}, "replcation", Options{})
	if res.Corrected != nil || len(res.Hits) != 1 || res.Hits[0].Source != "c" {
		t.Errorf("results = %+v", res)
	}
	// Only the vaults searched are used for corrections.
	res = search(t, []Source{{"d", d}}, "replcation", Options{})
	if res.Corrected["replcation"] != "replication" || len(res.Hits) != 1 {
		t.Errorf("results = %+v", res)
	}
}

func TestApply(t *testing.T) {
	x := newIndex(t, fleet)
	src := []Source{{Name: "kb", Index: x}}

	err := x.Apply(t.Context(), "c2", []Change{
		{Path: "notes/cache.md"}, // deleted
		{Path: "ops/fleet-roles.md", Version: version("x"), Content: []byte("# Fleet roles\n\nNow about zeppelins.\n")},
		{Path: "new.md", Version: version("y"), Content: []byte("# New\n\nFreshly written quokka notes.\n")},
		{Path: "bad.md", Version: version("z"), Content: []byte("not utf-8 \xff")},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if x.Commit() != "c2" {
		t.Errorf("commit = %q", x.Commit())
	}

	// The new word counts as known straight away, before the vocabulary reload finishes.
	res := search(t, src, "quokka", Options{})
	if res.Corrected != nil || len(res.Hits) != 1 {
		t.Errorf("new word: %+v", res)
	}
	if res := search(t, src, "invalidation", Options{}); len(res.Hits) != 0 {
		t.Errorf("deleted document still found: %v", hitPaths(res))
	}
	if res := search(t, src, "zeppelins", Options{}); len(hitPaths(res)) != 1 {
		t.Errorf("changed document not found")
	}
	_, err = x.Document(t.Context(), "bad.md")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("unusable file was indexed: %v", err)
	}
}

func TestDocumentAndLinks(t *testing.T) {
	x := newIndex(t, fleet)

	d, err := x.Document(t.Context(), "ops/fleet-doctrine.md")
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if d.Title != "Fleet doctrine" || d.Status != "active" || d.Description != "Which fittings we fly." ||
		!slices.Equal(d.Tags, []string{"fleet", "doctrine"}) || d.Version != version(fleet["ops/fleet-doctrine.md"]) {
		t.Errorf("document = %+v", d)
	}
	_, err = x.Document(t.Context(), "missing.md")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("missing document: %v", err)
	}

	from, err := x.LinkedFrom(t.Context(), "ops/fleet-roles.md")
	if err != nil || !slices.Equal(from, []string{"ops/fleet-doctrine.md"}) {
		t.Errorf("linked from roles = %v, %v", from, err)
	}
	from, err = x.LinkedFrom(t.Context(), "ops/fleet-doctrine.md")
	if err != nil || !slices.Equal(from, []string{"ops/fleet-roles.md"}) {
		t.Errorf("linked from doctrine (wikilink) = %v, %v", from, err)
	}
}

func TestList(t *testing.T) {
	x := newIndex(t, fleet)
	ctx := t.Context()

	paths := func(es []Entry) []string {
		var out []string
		for _, e := range es {
			out = append(out, e.Path)
		}
		return out
	}

	root, err := x.List(ctx, "", false, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"README.md", "notes/", "old/", "ops/", "services/"}
	if diff := cmp.Diff(want, paths(root)); diff != "" {
		t.Errorf("root (-want +got):\n%s", diff)
	}

	var paged []Entry
	after := ""
	for {
		page, err := x.List(ctx, "", false, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		paged = append(paged, page...)
		after = page[len(page)-1].Path
	}
	if diff := cmp.Diff(want, paths(paged)); diff != "" {
		t.Errorf("paged (-want +got):\n%s", diff)
	}
	recursivePage, _ := x.List(ctx, "ops", true, "ops/fleet-doctrine.md", 10)
	if diff := cmp.Diff([]string{"ops/fleet-roles.md"}, paths(recursivePage)); diff != "" {
		t.Errorf("recursive page (-want +got):\n%s", diff)
	}

	ops, _ := x.List(ctx, "ops/", false, "", 100)
	if diff := cmp.Diff([]string{"ops/fleet-doctrine.md", "ops/fleet-roles.md"}, paths(ops)); diff != "" {
		t.Errorf("ops (-want +got):\n%s", diff)
	}
	if ops[0].Title != "Fleet doctrine" {
		t.Errorf("entry = %+v", ops[0])
	}

	all, _ := x.List(ctx, "", true, "", 100)
	if len(all) != len(fleet) {
		t.Errorf("recursive listing has %d entries, want %d", len(all), len(fleet))
	}
}

func TestGenerations(t *testing.T) {
	dir := t.TempDir()
	x, err := Open(t.Context(), dir, discard())
	if err != nil {
		t.Fatal(err)
	}
	if x.Commit() != "" {
		t.Errorf("empty index commit = %q", x.Commit())
	}

	err = x.Build(t.Context(), "c1", changes(fleet))
	if err != nil {
		t.Fatal(err)
	}
	// A reader on the first build keeps working while a second build replaces it.
	old, err := x.acquire()
	if err != nil {
		t.Fatal(err)
	}
	err = x.Build(t.Context(), "c2", changes(map[string]string{"only.md": "# Only\n"}))
	if err != nil {
		t.Fatal(err)
	}
	var n int
	err = old.reader.QueryRow("SELECT count(*) FROM documents").Scan(&n)
	if err != nil || n != len(fleet) {
		t.Errorf("old generation during swap: %d documents, %v", n, err)
	}
	old.release()
	x.Close()

	files, _ := filepath.Glob(filepath.Join(dir, "index-*.db"))
	if len(files) != 1 {
		t.Errorf("generation files = %v, want one", files)
	}

	// Garbage next to it is cleaned up, and the newest usable generation is reopened.
	err = os.WriteFile(filepath.Join(dir, "index-99.db"), []byte("not sqlite"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	x, err = Open(t.Context(), dir, discard())
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	if x.Commit() != "c2" {
		t.Errorf("reopened commit = %q, want c2", x.Commit())
	}
	if _, err := os.Stat(filepath.Join(dir, "index-99.db")); !os.IsNotExist(err) {
		t.Error("unusable generation not removed")
	}
}

func TestInterruptedBuildIsDiscarded(t *testing.T) {
	dir := t.TempDir()
	x, err := Open(t.Context(), dir, discard())
	if err != nil {
		t.Fatal(err)
	}
	err = x.Build(t.Context(), "good", changes(fleet))
	if err != nil {
		t.Fatal(err)
	}
	// A build that fails half way, as if speki were killed while the transaction was open.
	failing := func(yield func(Change, error) bool) {
		if !yield(Change{Path: "a.md", Version: "v", Content: []byte("# A\n")}, nil) {
			return
		}
		yield(Change{}, errors.New("killed"))
	}
	err = x.Build(t.Context(), "half", failing)
	if err == nil {
		t.Fatal("failing build succeeded")
	}
	if x.Commit() != "good" {
		t.Errorf("commit after failed build = %q", x.Commit())
	}
	x.Close()

	x, err = Open(t.Context(), dir, discard())
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	if x.Commit() != "good" {
		t.Errorf("reopened commit = %q, want good", x.Commit())
	}
}

func TestClosedIndex(t *testing.T) {
	x := newIndex(t, fleet)
	x.Close()
	x.Close() // twice is fine

	_, err := Search(t.Context(), []Source{{"kb", x}}, "fleet", Options{})
	if !errors.Is(err, ErrClosed) {
		t.Errorf("Search on closed index: %v", err)
	}
	_, err = x.Document(t.Context(), "README.md")
	if !errors.Is(err, ErrClosed) {
		t.Errorf("Document on closed index: %v", err)
	}
	if x.Commit() != "" {
		t.Errorf("Commit on closed index = %q", x.Commit())
	}
}

func TestSearchWhileBuilding(t *testing.T) {
	x := newIndex(t, corpus())
	src := []Source{{"kb", x}}
	errs := make(chan error, 6) // the builder's errors, reported from the test's goroutine
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 3 {
			errs <- x.Build(t.Context(), fmt.Sprint("c", i), changes(corpus()))
			errs <- x.Apply(t.Context(), fmt.Sprint("a", i), []Change{{Path: "notes/cache.md"}})
		}
	}()
	searching := true
	for searching {
		select {
		case <-done:
			searching = false
		default:
			_, err := Search(t.Context(), src, "fleet", Options{})
			if err != nil {
				<-done
				t.Fatal(err)
			}
		}
	}
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

func TestApplyKeepsOldDocumentWhenNewOneFails(t *testing.T) {
	x := newIndex(t, fleet)
	err := x.Apply(t.Context(), "c2", []Change{{Path: "README.md", Version: "v2", Content: []byte("bad \xff")}})
	if err != nil {
		t.Fatal(err)
	}
	d, err := x.Document(t.Context(), "README.md")
	if err != nil || d.Version != version(fleet["README.md"]) {
		t.Errorf("README.md after a failed change: %+v, %v", d, err)
	}
}

func TestSearchDoesNotCorrectPartialWordsOrNumbers(t *testing.T) {
	x := newIndex(t, map[string]string{"a.md": "# Network\n\nnetwork notes for 2025\n"})
	x.current.Load().vocab.Store(nil) // as right after a change, before the vocabulary is reloaded
	src := []Source{{"kb", x}}

	res := search(t, src, "netw", Options{})
	if res.Corrected != nil || len(res.Hits) != 1 {
		t.Errorf("partial word: %+v", res)
	}
	res = search(t, src, "2026", Options{})
	if res.Corrected != nil {
		t.Errorf("number corrected: %v", res.Corrected)
	}
}

func TestVocabulary(t *testing.T) {
	v := &vocabulary{trigrams: map[string][]int32{}}
	for i, w := range []string{"doctrine", "kubectl", "kubernetes", "replication"} { // sorted
		v.words = append(v.words, w)
		v.docs = append(v.docs, 10-i)
		for _, tg := range trigramsOf(w) {
			v.trigrams[tg] = append(v.trigrams[tg], int32(i))
		}
	}

	tests := []struct {
		word string
		max  int
		want string
	}{
		{"kubernets", 2, "kubernetes"},
		{"replcation", 2, "replication"},
		{"doctirne", 2, "doctrine"}, //nolint:misspell // a transposition, on purpose
		{"zzzz", 2, ""},
	}
	for _, tt := range tests {
		got := ""
		if s := v.suggest(tt.word, tt.max); len(s) > 0 {
			got = slices.MinFunc(s, func(a, b suggestion) int { return a.distance - b.distance }).word
		}
		if got != tt.want {
			t.Errorf("suggest(%q) = %q, want %q", tt.word, got, tt.want)
		}
	}
	if d := distance("abc", "abc", 2); d != 0 {
		t.Errorf("distance = %d", d)
	}
	if d := distance("abcdef", "xyz", 2); d != 3 {
		t.Errorf("distance beyond limit = %d, want 3", d)
	}
}

// The examples in docs/functional/search-and-query.md that TestSearch doesn't cover.
func TestSearchExamples(t *testing.T) {
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
	src := []Source{{Name: "kb", Index: newIndex(t, files)}}
	score := func(res Results, path string) float64 {
		for _, h := range res.Hits {
			if h.Path == path {
				return h.Score
			}
		}
		t.Fatalf("%s not found in %v", path, hitPaths(res))
		return 0
	}

	t.Run("quoted phrases are adjacent, after stemming", func(t *testing.T) {
		res := search(t, src, `"cache invalidation"`, Options{})
		got := hitPaths(res)
		if !slices.Contains(got, "phrase/adjacent.md") || slices.Contains(got, "phrase/apart.md") {
			t.Errorf("hits = %v", got)
		}
	})
	t.Run("title outranks tags outranks body", func(t *testing.T) {
		got := hitPaths(search(t, src, "zeppelin", Options{}))
		if strings.Join(got, ",") != "weights/title.md,weights/tags.md,weights/body.md" {
			t.Errorf("order = %v", got)
		}
	})
	t.Run("deprecated is halved, draft keeps its score", func(t *testing.T) {
		res := search(t, src, "dirigible", Options{})
		active, draft, old := score(res, "status/active.md"), score(res, "status/draft.md"), score(res, "status/old.md")
		if draft != active {
			t.Errorf("draft %v, active %v", draft, active)
		}
		if math.Abs(old-active/2) > 1e-9 {
			t.Errorf("deprecated %v, active %v", old, active)
		}
	})
	t.Run("snippet of at most 32 words", func(t *testing.T) {
		res := search(t, src, "blimp", Options{})
		if n := len(strings.Fields(res.Hits[0].Snippet)); n > 32+2 { // the ellipses
			t.Errorf("snippet of %d words: %q", n, res.Hits[0].Snippet)
		}
		if !strings.Contains(res.Hits[0].Snippet, "**blimp**") {
			t.Errorf("snippet = %q", res.Hits[0].Snippet)
		}
	})
	t.Run("a match only in the title shows the description", func(t *testing.T) {
		res := search(t, src, "airship", Options{})
		if res.Hits[0].Snippet != "Lighter than air." {
			t.Errorf("snippet = %q", res.Hits[0].Snippet)
		}
	})
}

// A failure that isn't the file's (here a cancelled context) leaves the commit where it was, so the next
// sync tries again instead of losing the change.
func TestApplyFailureKeepsTheCommit(t *testing.T) {
	x := newIndex(t, fleet)
	g, err := x.acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer g.release()
	_, err = g.writer.ExecContext(t.Context(), `CREATE TRIGGER fail_headings BEFORE INSERT ON headings
		BEGIN SELECT RAISE(ABORT, 'disk full'); END`)
	if err != nil {
		t.Fatal(err)
	}
	content := "# README\n\nNew text.\n"
	changes := []Change{
		{Path: "bad.md", Version: version("z"), Content: []byte("not utf-8 \xff")},
		{Path: "README.md", Version: version(content), Content: []byte(content)},
	}
	err = x.Apply(t.Context(), "c2", changes)
	if err == nil {
		t.Fatal("Apply succeeded although inserts fail")
	}
	if x.Commit() != "c1" {
		t.Errorf("commit moved to %s", x.Commit())
	}

	_, err = g.writer.ExecContext(t.Context(), "DROP TRIGGER fail_headings")
	if err != nil {
		t.Fatal(err)
	}
	err = x.Apply(t.Context(), "c2", changes)
	if err != nil || x.Commit() != "c2" {
		t.Fatalf("retry: %v, commit %s", err, x.Commit())
	}
	if _, err := x.Document(t.Context(), "bad.md"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the bad file after the retry: %v", err)
	}
	if d, err := x.Document(t.Context(), "README.md"); err != nil || d.Version != version(content) {
		t.Errorf("README.md after the retry: %+v, %v", d, err)
	}
}

// A build stops reading files once an insert fails: the build fails anyway.
func TestBuildStopsAtAFailedInsert(t *testing.T) {
	x, err := Open(t.Context(), t.TempDir(), discard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(x.Close)
	const total = 5000
	read := 0
	files := func(yield func(Change, error) bool) {
		for i := range total {
			read++
			path := fmt.Sprintf("d%d.md", i)
			if i == 1 {
				path = "d0.md" // the same path twice fails its insert
			}
			content := fmt.Sprintf("# D%d\n", i)
			if !yield(Change{Path: path, Version: version(content), Content: []byte(content)}, nil) {
				return
			}
		}
	}
	err = x.Build(t.Context(), "c1", files)
	if err == nil || !strings.Contains(err.Error(), "insert d0.md") {
		t.Fatalf("Build: %v", err)
	}
	if read > total/2 {
		t.Errorf("read %d of %d files after the insert failed", read, total)
	}
}

// Answers carry the start of a long title or description, not all of it.
func TestLongTitlesAreCut(t *testing.T) {
	title, desc := strings.Repeat("é", maxTitle), strings.Repeat("quokka ", maxDescription)
	x := newIndex(t, map[string]string{"long.md": "---\ntitle: " + title + "\ndescription: " + desc + "\n---\nBody.\n"})
	check := func(where, title, desc string) {
		t.Helper()
		if len(title) > maxTitle || len(desc) > maxDescription || !utf8.ValidString(title) || !strings.HasSuffix(desc, "…") {
			t.Errorf("%s: title of %d bytes, description of %d", where, len(title), len(desc))
		}
	}
	d, err := x.Document(t.Context(), "long.md")
	if err != nil {
		t.Fatal(err)
	}
	check("document", d.Title, d.Description)
	entries, err := x.List(t.Context(), "", true, "", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("list: %v, %v", entries, err)
	}
	check("list", entries[0].Title, entries[0].Description)
	res := search(t, []Source{{Name: "kb", Index: x}}, "quokka", Options{})
	if len(res.Hits) != 1 {
		t.Fatalf("search: %v", hitPaths(res))
	}
	check("search", res.Hits[0].Title, res.Hits[0].Description)
	if len(res.Hits[0].Snippet) > maxDescription {
		t.Errorf("snippet of %d bytes", len(res.Hits[0].Snippet))
	}
}

// A snippet cut inside a match still closes the bold span, and stays within the limit.
func TestBoldMatches(t *testing.T) {
	for _, s := range []string{
		strings.Repeat("a", 490) + "\x01" + strings.Repeat("b", 30) + "\x02 c",
		"\x01" + strings.Repeat("é", 400) + "\x02",
		"short \x01match\x02 here",
	} {
		got := boldMatches(s)
		if len(got) > maxDescription || strings.Count(got, "**")%2 != 0 || !utf8.ValidString(got) {
			t.Errorf("boldMatches = %d bytes, %d markers: %q", len(got), strings.Count(got, "**"), got)
		}
	}
	if got := boldMatches("short \x01match\x02 here"); got != "short **match** here" {
		t.Errorf("short snippet: %q", got)
	}
}

// Links typed in NFC find files named in NFD, as macOS names them, and the other way round.
func TestLinksAcrossNormalForms(t *testing.T) {
	nfd := norm.NFD.String("café/menu.md")
	x := newIndex(t, map[string]string{
		nfd:              "# Menu\n",
		"index.md":       "# Index\n\n[menu](café/menu.md) and [[Menu]]\n",
		"résumé.md":      "# Résumé\n",
		"other/links.md": "# Links\n\n[cv](../" + norm.NFD.String("résumé.md") + ")\n",
	})
	g, err := x.acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer g.release()
	rows, err := g.reader.QueryContext(t.Context(), `SELECT d.path, l.target_path FROM resolved_links l
		JOIN documents d ON d.id = l.doc_id ORDER BY d.path, l.kind`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var from string
		var to sql.NullString
		if err := rows.Scan(&from, &to); err != nil {
			t.Fatal(err)
		}
		got = append(got, from+" -> "+to.String)
	}
	want := []string{"index.md -> " + nfd, "index.md -> " + nfd, "other/links.md -> résumé.md"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("resolved links (-want +got):\n%s", diff)
	}
	for path, want := range map[string][]string{nfd: {"index.md"}, "résumé.md": {"other/links.md"}} {
		from, err := x.LinkedFrom(t.Context(), path)
		if err != nil || !slices.Equal(from, want) {
			t.Errorf("LinkedFrom(%q) = %v, %v; want %v", path, from, err, want)
		}
	}
}

// Only errors that mean a generation is unusable for good get it discarded; anything else, such as a
// cancelled context or too many open files, keeps the file for the next try.
func TestUnusableErrors(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want bool
	}{
		{sql.ErrNoRows, true},
		{fmt.Errorf("%w: quick_check: row 3 missing", errUnusable), true},
		{sqlite3.Error{Code: sqlite3.ErrNotADB}, true},
		{sqlite3.Error{Code: sqlite3.ErrCorrupt}, true},
		{context.Canceled, false},
		{sqlite3.Error{Code: sqlite3.ErrCantOpen}, false},
		{sqlite3.Error{Code: sqlite3.ErrBusy}, false},
	} {
		if got := errors.Is(unusable(tt.err), errUnusable); got != tt.want {
			t.Errorf("unusable(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

func TestSearchRankingAndInput(t *testing.T) {
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
	src := []Source{{Name: "kb", Index: newIndex(t, files)}}

	t.Run("deprecated documents don't crowd out an active one", func(t *testing.T) {
		got := hitPaths(search(t, src, "airship", Options{}))
		if len(got) == 0 || got[0] != "new/airship.md" {
			t.Errorf("the active document isn't first: %v", got)
		}
	})
	t.Run("a word found by its stem isn't corrected", func(t *testing.T) {
		res := search(t, src, "runs", Options{})
		if len(res.Corrected) != 0 || !slices.Contains(hitPaths(res), "words/run.md") {
			t.Errorf("hits %v, corrected %v", hitPaths(res), res.Corrected)
		}
	})
	t.Run("substring matches rank below full-text matches", func(t *testing.T) {
		// "service" is in most documents, so BM25 gives full-text matches only a tiny score.
		common := map[string]string{
			"names/micro.md":   "---\ntitle: Microservices\n---\nSmall things.\n",
			"names/service.md": "---\ntitle: Service\n---\nAbout the service.\n",
		}
		for i := range 20 {
			common[fmt.Sprintf("common/%02d.md", i)] = fmt.Sprintf("# Note %d\n\nThe service runs.\n", i)
		}
		res := search(t, []Source{{Name: "kb", Index: newIndex(t, common)}}, "service", Options{Limit: 50})
		got := hitPaths(res)
		if len(got) == 0 || got[len(got)-1] != "names/micro.md" {
			t.Errorf("the substring match isn't last: %v", got)
		}
	})
	t.Run("folder names aren't matched as names", func(t *testing.T) {
		got := hitPaths(search(t, src, "words", Options{}))
		for _, p := range got {
			if strings.HasPrefix(p, "words/") {
				t.Errorf("a document matched by its folder name: %v", got)
				break
			}
		}
	})
	t.Run("a quoted word matches as written", func(t *testing.T) {
		if got := hitPaths(search(t, src, "zeppel", Options{})); !slices.Contains(got, "body/zep.md") {
			t.Fatalf("unquoted, the last word matches as a prefix: %v", got)
		}
		res := search(t, src, `"zeppel"`, Options{})
		if len(res.Hits) != 0 || len(res.Corrected) != 0 {
			t.Errorf("hits %v, corrected %v", hitPaths(res), res.Corrected)
		}
	})
	t.Run("three-letter typos", func(t *testing.T) {
		res := search(t, src, "cot sat", Options{})
		if res.Corrected["cot"] != "cat" {
			t.Errorf("corrected = %v", res.Corrected)
		}
	})
	t.Run("query size", func(t *testing.T) {
		var many []string
		for i := range maxTerms + 1 {
			many = append(many, fmt.Sprintf("w%d", i))
		}
		_, err := Search(t.Context(), src, strings.Join(many, " "), Options{})
		if !errors.Is(err, ErrTooManyWords) {
			t.Errorf("%d words: %v", len(many), err)
		}
		_, err = Search(t.Context(), src, strings.Repeat("fleet ", 500), Options{})
		if err != nil {
			t.Errorf("one word 500 times: %v", err)
		}
		excl := "fleet"
		for i := range maxTerms - 1 {
			excl += fmt.Sprintf(" -x%d", i)
		}
		if _, err := Search(t.Context(), src, excl, Options{}); err != nil {
			t.Errorf("many exclusions: %v", err)
		}
	})
}

// A build whose inserting fails keeps any other error next to it, so a broken git copy is noticed, but
// not the cancellation the failed insert caused.
func TestFillError(t *testing.T) {
	insertErr := errors.New("insert a.md: disk full")
	readErr := errors.New("blob missing")
	if err := fillError(t.Context(), insertErr, fmt.Errorf("x: %w", readErr)); !errors.Is(err, readErr) || !errors.Is(err, insertErr) {
		t.Errorf("with a read error: %v", err)
	}
	if err := fillError(t.Context(), insertErr, context.Canceled); err == nil || err.Error() != insertErr.Error() {
		t.Errorf("with the build's own cancellation: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := fillError(ctx, insertErr, context.Canceled); !errors.Is(err, context.Canceled) {
		t.Errorf("with the caller's cancellation: %v", err)
	}
	if err := fillError(t.Context(), nil, readErr); !errors.Is(err, readErr) {
		t.Errorf("with only a read error: %v", err)
	}
}
