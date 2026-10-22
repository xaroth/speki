// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index_test

import (
	"maps"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/xaroth/speki/library/index"
)

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
