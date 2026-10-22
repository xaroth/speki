// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index_test

import (
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"golang.org/x/text/unicode/norm"

	"github.com/xaroth/speki/document"
	"github.com/xaroth/speki/library/index"
)

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
