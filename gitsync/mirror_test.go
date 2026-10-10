package gitsync

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"golang.org/x/text/unicode/norm"
)

// source is a regular repository standing in for GitHub.
type source struct {
	t    *testing.T
	dir  string
	repo *git.Repository
}

func newSource(t *testing.T) *source {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	// go-git names the first branch master; speki is told the branch name, so either works.
	return &source{t: t, dir: dir, repo: repo}
}

// commit writes files (nil content deletes, a "->target" content makes a symlink) and commits them.
func (s *source) commit(files map[string]*string) string {
	s.t.Helper()
	wt, err := s.repo.Worktree()
	if err != nil {
		s.t.Fatal(err)
	}
	for name, content := range files {
		full := filepath.Join(s.dir, name)
		if content == nil {
			_, err = wt.Remove(name)
			if err != nil {
				s.t.Fatal(err)
			}
			continue
		}
		err = os.MkdirAll(filepath.Dir(full), 0o755)
		if err != nil {
			s.t.Fatal(err)
		}
		_ = os.Remove(full)
		if target, ok := strings.CutPrefix(*content, "->"); ok {
			err = os.Symlink(target, full)
		} else {
			err = os.WriteFile(full, []byte(*content), 0o644)
		}
		if err != nil {
			s.t.Fatal(err)
		}
		_, err = wt.Add(name)
		if err != nil {
			s.t.Fatal(err)
		}
	}
	h, err := wt.Commit("change", &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()},
	})
	if err != nil {
		s.t.Fatal(err)
	}
	return h.String()
}

func str(s string) *string { return &s }

func paths(files []File) []string {
	var out []string
	for _, f := range files {
		out = append(out, f.Path)
	}
	slices.Sort(out)
	return out
}

func TestOpenClonesEligibleFiles(t *testing.T) {
	src := newSource(t)
	head := src.commit(map[string]*string{
		"README.md":                  str("# Vault\n"),
		"ops/fleet.md":               str("# Fleet\n"),
		"notes.txt":                  str("not markdown"),
		".obsidian/workspace.md":     str("dot folder"),
		"ops/.hidden.md":             str("dot file"),
		"big.md":                     str(strings.Repeat("x", MaxFileSize+1)),
		"lfs.md":                     str(lfsPointer + "\noid sha256:abc\nsize 12\n"),
		"link.md":                    str("->README.md"),
		"speki.yaml":                 str("title: x\n"),
		"deep/er/still/fine/file.md": str("ok"),
	})

	m, err := Open(t.Context(), filepath.Join(t.TempDir(), "copy"), Source{URL: src.dir, Branch: "master"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := m.Head()
	if err != nil || got != head {
		t.Fatalf("Head = %q, %v; want %q", got, err, head)
	}

	files, err := m.Files(head)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	want := []string{"README.md", "deep/er/still/fine/file.md", "ops/fleet.md"}
	if diff := cmp.Diff(want, paths(files)); diff != "" {
		t.Errorf("files (-want +got):\n%s", diff)
	}

	all, err := m.Paths(head)
	if err != nil {
		t.Fatalf("Paths: %v", err)
	}
	wantAll := []string{".obsidian/workspace.md", "README.md", "big.md", "deep/er/still/fine/file.md", "link.md", "lfs.md",
		"notes.txt", "ops/.hidden.md", "ops/fleet.md", "speki.yaml"}
	if diff := cmp.Diff(wantAll, all, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
		t.Errorf("paths (-want +got):\n%s", diff)
	}

	content, err := m.Blob(files[0].Blob)
	if err != nil {
		t.Fatalf("Blob: %v", err)
	}
	if len(content) == 0 {
		t.Error("empty blob")
	}
}

func TestFetchAndDiff(t *testing.T) {
	src := newSource(t)
	first := src.commit(map[string]*string{
		"keep.md":    str("same"),
		"edit.md":    str("before"),
		"delete.md":  str("bye"),
		"rename.md":  str("moving"),
		"shrink.md":  str(strings.Repeat("x", MaxFileSize+1)),
		"README.txt": str("ignored"),
	})
	m, err := Open(t.Context(), filepath.Join(t.TempDir(), "copy"), Source{URL: src.dir, Branch: "master"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// A few pushes, so the copy has to fetch on top of a shallow history.
	src.commit(map[string]*string{"edit.md": str("middle")})
	second := src.commit(map[string]*string{
		"edit.md":      str("after"),
		"delete.md":    nil,
		"rename.md":    nil,
		"renamed.md":   str("moving"),
		"shrink.md":    str("small now"),
		"new.md":       str("hello"),
		"keep.md":      str("same"),
		"README.txt":   str("still ignored"),
		".hidden/x.md": str("ignored"),
	})

	head, err := m.Fetch(t.Context(), nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if head != second {
		t.Fatalf("Fetch = %s, want %s", head, second)
	}
	if !m.Has(first) {
		t.Fatal("the previous commit is gone after a shallow fetch")
	}

	changes, err := m.Diff(first, second)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	got := map[string]bool{} // path -> deleted
	for _, c := range changes {
		got[c.Path] = c.Blob == ""
	}
	want := map[string]bool{
		"edit.md":    false,
		"delete.md":  true,
		"rename.md":  true,
		"renamed.md": false,
		"shrink.md":  false,
		"new.md":     false,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("changes, path -> deleted (-want +got):\n%s", diff)
	}

	again, err := m.Fetch(t.Context(), nil)
	if err != nil || again != second {
		t.Errorf("fetch with nothing new = %s, %v", again, err)
	}
}

func TestOpenReusesAndRepairs(t *testing.T) {
	src := newSource(t)
	head := src.commit(map[string]*string{"a.md": str("a")})
	dir := filepath.Join(t.TempDir(), "copy")

	_, err := Open(t.Context(), dir, Source{URL: src.dir, Branch: "master"})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	src.commit(map[string]*string{"b.md": str("b")})

	m, err := Open(t.Context(), dir, Source{URL: src.dir, Branch: "master"})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, _ := m.Head()
	if got != head {
		t.Errorf("reopened copy is at %s, want the old head %s (no fetch yet)", got, head)
	}

	// Break the copy: remove its objects.
	err = os.RemoveAll(filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	m, err = Open(t.Context(), dir, Source{URL: src.dir, Branch: "master"})
	if err != nil {
		t.Fatalf("Open broken copy: %v", err)
	}
	files, err := m.Files(mustHead(t, m))
	if err != nil || len(files) != 2 {
		t.Errorf("repaired copy files = %v, %v", paths(files), err)
	}
}

func TestDiffEligibilityTransitions(t *testing.T) {
	src := newSource(t)
	first := src.commit(map[string]*string{
		"to-symlink.md": str("text"),
		"grows.md":      str("small"),
		"to-lfs.md":     str("real content"),
		"target.md":     str("target"),
		"run.md":        str("exec"),
		"with space.md": str("spaced"),
	})
	err := os.Chmod(filepath.Join(src.dir, "run.md"), 0o755)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Open(t.Context(), filepath.Join(t.TempDir(), "copy"), Source{URL: src.dir, Branch: "master"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	second := src.commit(map[string]*string{
		"to-symlink.md": str("->target.md"),
		"grows.md":      str(strings.Repeat("x", MaxFileSize+1)),
		"to-lfs.md":     str(lfsPointer + "\noid sha256:abc\nsize 12\n"),
		"run.md":        str("exec, changed"),
		"with space.md": str("spaced, changed"),
	})
	_, err = m.Fetch(t.Context(), nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	changes, err := m.Diff(first, second)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	got := map[string]bool{}
	for _, c := range changes {
		got[c.Path] = c.Blob == ""
	}
	want := map[string]bool{
		"to-symlink.md": true,
		"grows.md":      true,
		"to-lfs.md":     true,
		"run.md":        false,
		"with space.md": false,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("changes, path -> deleted (-want +got):\n%s", diff)
	}
}

func TestOpenReplacesNonRepository(t *testing.T) {
	src := newSource(t)
	src.commit(map[string]*string{"a.md": str("a")})
	dir := filepath.Join(t.TempDir(), "copy")
	err := os.MkdirAll(dir, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, "junk"), []byte("not git"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	m, err := Open(t.Context(), dir, Source{URL: src.dir, Branch: "master"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	files, err := m.Files(mustHead(t, m))
	if err != nil || len(files) != 1 {
		t.Errorf("files = %v, %v", paths(files), err)
	}
}

func TestOpenFailsCleanly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "copy")
	_, err := Open(t.Context(), dir, Source{URL: filepath.Join(t.TempDir(), "nothing"), Branch: "master"})
	if err == nil {
		t.Fatal("Open of a missing remote succeeded")
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Errorf("failed clone left %s behind", dir)
	}
}

func mustHead(t *testing.T, m *Mirror) string {
	t.Helper()
	h, err := m.Head()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// The copy fetches from the source it's given now, not from the remote stored when it was cloned: on
// SetSource, and when an existing copy is opened with another URL.
func TestFetchFollowsTheSource(t *testing.T) {
	first, second := newSource(t), newSource(t)
	first.commit(map[string]*string{"a.md": str("# First\n")})
	want := second.commit(map[string]*string{"b.md": str("# Second\n")})
	dir := filepath.Join(t.TempDir(), "copy")

	m, err := Open(t.Context(), dir, Source{URL: first.dir, Branch: "master"})
	if err != nil {
		t.Fatal(err)
	}
	m.SetSource(second.dir, nil)
	got, err := m.Fetch(t.Context(), nil)
	if err != nil || got != want {
		t.Errorf("Fetch after SetSource = %s, %v; want %s", got, err, want)
	}

	want = second.commit(map[string]*string{"c.md": str("# More\n")})
	m, err = Open(t.Context(), dir, Source{URL: second.dir, Branch: "master"})
	if err != nil {
		t.Fatal(err)
	}
	got, err = m.Fetch(t.Context(), nil)
	if err != nil || got != want {
		t.Errorf("Fetch after reopening = %s, %v; want %s", got, err, want)
	}
}

// Paths reports names as git stores them, NFD included: the write path compares new names with them.
func TestPathsKeepGitsBytes(t *testing.T) {
	src := newSource(t)
	name := norm.NFD.String("menü/brötchen.md")
	head := src.commit(map[string]*string{name: str("# B\n")})
	m, err := Open(t.Context(), filepath.Join(t.TempDir(), "copy"), Source{URL: src.dir, Branch: "master"})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := m.Paths(head)
	if err != nil || len(paths) != 1 || paths[0] != name {
		t.Errorf("Paths = %q, %v; want the NFD bytes %q", paths, err, name)
	}
}
