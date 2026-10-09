package index

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
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
		{query: "kubernets", want: []string{"ops/fleet-doctrine.md"}, corrected: map[string]string{"kubernets": "kubernetes"}},
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
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 3 {
			err := x.Build(t.Context(), fmt.Sprint("c", i), changes(corpus()))
			if err != nil {
				t.Error(err)
			}
			err = x.Apply(t.Context(), fmt.Sprint("a", i), []Change{{Path: "notes/cache.md"}})
			if err != nil {
				t.Error(err)
			}
		}
	}()
	for {
		select {
		case <-done:
			return
		default:
		}
		_, err := Search(t.Context(), src, "fleet", Options{})
		if err != nil {
			t.Fatal(err)
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
