package gitsync_test

import (
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/go-cmp/cmp"
	"golang.org/x/text/unicode/norm"

	"github.com/eve-online-tools/speki/gitsync"
)

const lfsPointer = "version https://git-lfs.github.com/spec/v1\noid sha256:abc\nsize 12\n"

var errToken = errors.New("no token")

type failingToken struct{}

func (failingToken) Token(context.Context) (string, error) { return "", errToken }

// source is a regular repository standing in for GitHub.
type source struct {
	tb   testing.TB
	dir  string
	repo *git.Repository
}

func newSource(tb testing.TB) *source {
	tb.Helper()
	dir := tb.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		tb.Fatal(err)
	}
	// go-git names the first branch master; speki is told the branch name, so either works.
	return &source{tb: tb, dir: dir, repo: repo}
}

// commit writes files (nil content deletes, a "->target" content makes a symlink) and commits them.
func (s *source) commit(files map[string]*string) string {
	s.tb.Helper()
	wt, err := s.repo.Worktree()
	if err != nil {
		s.tb.Fatal(err)
	}
	for name, content := range files {
		full := filepath.Join(s.dir, name)
		if content == nil {
			_, err = wt.Remove(name)
			if err != nil {
				s.tb.Fatal(err)
			}
			continue
		}
		err = os.MkdirAll(filepath.Dir(full), 0o755)
		if err != nil {
			s.tb.Fatal(err)
		}
		_ = os.Remove(full)
		if target, ok := strings.CutPrefix(*content, "->"); ok {
			err = os.Symlink(target, full)
		} else {
			err = os.WriteFile(full, []byte(*content), 0o644)
		}
		if err != nil {
			s.tb.Fatal(err)
		}
		_, err = wt.Add(name)
		if err != nil {
			s.tb.Fatal(err)
		}
	}
	h, err := wt.Commit("change", &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()},
	})
	if err != nil {
		s.tb.Fatal(err)
	}
	return h.String()
}

// makeExecutable sets the executable bit on a file and stages it for the next commit.
func (s *source) makeExecutable(name string) {
	s.tb.Helper()
	err := os.Chmod(filepath.Join(s.dir, name), 0o755)
	if err != nil {
		s.tb.Fatal(err)
	}
	wt, err := s.repo.Worktree()
	if err != nil {
		s.tb.Fatal(err)
	}
	_, err = wt.Add(name)
	if err != nil {
		s.tb.Fatal(err)
	}
}

// rawCommit commits files by writing the objects directly, for names go-git's worktree refuses but git
// allows. Each commit holds exactly the files given, on top of the branch's head.
func (s *source) rawCommit(files map[string]string) string {
	s.tb.Helper()
	store := s.repo.Storer
	put := func(o interface {
		Encode(plumbing.EncodedObject) error
	}) plumbing.Hash {
		obj := store.NewEncodedObject()
		err := o.Encode(obj)
		if err != nil {
			s.tb.Fatal(err)
		}
		h, err := store.SetEncodedObject(obj)
		if err != nil {
			s.tb.Fatal(err)
		}
		return h
	}
	var tree func(files map[string]string) plumbing.Hash
	tree = func(files map[string]string) plumbing.Hash {
		var entries []object.TreeEntry
		folders := map[string]map[string]string{}
		for name, content := range files {
			if folder, rest, ok := strings.Cut(name, "/"); ok {
				if folders[folder] == nil {
					folders[folder] = map[string]string{}
				}
				folders[folder][rest] = content
				continue
			}
			blob := store.NewEncodedObject()
			blob.SetType(plumbing.BlobObject)
			w, _ := blob.Writer()
			_, _ = w.Write([]byte(content))
			_ = w.Close()
			h, err := store.SetEncodedObject(blob)
			if err != nil {
				s.tb.Fatal(err)
			}
			entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: h})
		}
		for name, inner := range folders {
			entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: tree(inner)})
		}
		key := func(e object.TreeEntry) string { // git's order: a folder sorts as its name and a slash
			if e.Mode == filemode.Dir {
				return e.Name + "/"
			}
			return e.Name
		}
		slices.SortFunc(entries, func(a, b object.TreeEntry) int { return strings.Compare(key(a), key(b)) })
		return put(&object.Tree{Entries: entries})
	}
	sig := object.Signature{Name: "t", Email: "t@x", When: time.Now()}
	commit := &object.Commit{Author: sig, Committer: sig, Message: "raw", TreeHash: tree(files)}
	ref, err := s.repo.Reference(plumbing.NewBranchReferenceName("master"), true)
	if err == nil {
		commit.ParentHashes = []plumbing.Hash{ref.Hash()}
	}
	h := put(commit)
	err = store.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("master"), h))
	if err != nil {
		s.tb.Fatal(err)
	}
	return h.String()
}

// entryOf returns the tree entry at path in commit of the source.
func (s *source) entryOf(commit, path string) object.TreeEntry {
	s.tb.Helper()
	c, err := s.repo.CommitObject(plumbing.NewHash(commit))
	if err != nil {
		s.tb.Fatal(err)
	}
	root, err := c.Tree()
	if err != nil {
		s.tb.Fatal(err)
	}
	e, err := root.FindEntry(path)
	if err != nil {
		s.tb.Fatal(err)
	}
	return *e
}

// openCopy clones src into a new folder.
func openCopy(tb testing.TB, src *source) *gitsync.Mirror {
	tb.Helper()
	m, err := gitsync.Open(tb.Context(), filepath.Join(tb.TempDir(), "copy"), gitsync.Source{URL: src.dir, Branch: "master"})
	if err != nil {
		tb.Fatalf("Open: %v", err)
	}
	return m
}

// openWorkingCopy opens a copy of the source's own repository, loose objects included, so single objects
// can be removed from it.
func openWorkingCopy(tb testing.TB, src *source) (m *gitsync.Mirror, gitDir string) {
	tb.Helper()
	dir := filepath.Join(tb.TempDir(), "copy")
	copyDir(tb, src.dir, dir)
	m, err := gitsync.Open(tb.Context(), dir, gitsync.Source{URL: src.dir, Branch: "master"})
	if err != nil {
		tb.Fatalf("Open: %v", err)
	}
	return m, filepath.Join(dir, ".git")
}

func mustHead(tb testing.TB, m *gitsync.Mirror) string {
	tb.Helper()
	h, err := m.Head()
	if err != nil {
		tb.Fatal(err)
	}
	return h
}

func blobOf(content string) string {
	return plumbing.ComputeHash(plumbing.BlobObject, []byte(content)).String()
}

// copyDir copies a directory tree, for breaking a copy of a repository without touching the original.
func copyDir(tb testing.TB, from, to string) {
	tb.Helper()
	err := filepath.WalkDir(from, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		target := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o644)
	})
	if err != nil {
		tb.Fatal(err)
	}
}

// removeObject deletes a loose object from a repository's object store.
func removeObject(tb testing.TB, gitDir, hash string) {
	tb.Helper()
	err := os.Remove(filepath.Join(gitDir, "objects", hash[:2], hash[2:]))
	if err != nil {
		tb.Fatal(err)
	}
}

func TestOpen(t *testing.T) {
	t.Parallel()
	src := newSource(t)
	nfd := norm.NFD.String("menü/brötchen.md") // Paths keeps git's bytes: the write path compares new names with them
	head := src.commit(map[string]*string{
		"README.md":              new("# Vault\n"),
		"ops/fleet.md":           new("# Fleet\n"),
		nfd:                      new("# B\n"),
		"notes.txt":              new("not markdown"),
		".obsidian/workspace.md": new("dot folder"),
		"ops/.hidden.md":         new("dot file"),
		"big.md":                 new(strings.Repeat("x", gitsync.MaxFileSize+1)),
		"lfs.md":                 new(lfsPointer),
		"link.md":                new("->README.md"),
	})

	m := openCopy(t, src)
	got, err := m.Head()
	if err != nil || got != head {
		t.Fatalf("got Head %q, %v; want %q", got, err, head)
	}

	files, err := m.Files(head)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	want := []gitsync.File{
		{Path: "README.md", Blob: blobOf("# Vault\n")},
		{Path: nfd, Blob: blobOf("# B\n")},
		{Path: "ops/fleet.md", Blob: blobOf("# Fleet\n")},
	}
	if diff := cmp.Diff(want, files); diff != "" {
		t.Errorf("Files (-want +got):\n%s", diff)
	}

	paths, err := m.Paths(head)
	if err != nil {
		t.Fatalf("Paths: %v", err)
	}
	wantPaths := []string{".obsidian/workspace.md", "README.md", "big.md", "link.md", "lfs.md", nfd, "notes.txt",
		"ops/.hidden.md", "ops/fleet.md"}
	slices.Sort(paths)
	slices.Sort(wantPaths)
	if diff := cmp.Diff(wantPaths, paths); diff != "" {
		t.Errorf("Paths (-want +got):\n%s", diff)
	}

	content, err := m.Blob(blobOf("# Vault\n"))
	if err != nil || string(content) != "# Vault\n" {
		t.Errorf("got Blob %q, %v; want %q", content, err, "# Vault\n")
	}
	_, err = m.Blob(src.entryOf(head, "big.md").Hash.String())
	if err == nil {
		t.Error("got no error for a Blob over MaxFileSize, want one")
	}
	if got := m.Packs(); got != 1 {
		t.Errorf("got %d packs after a clone, want 1", got)
	}
}

// An existing copy is reused when it's healthy and cloned again when it isn't.
func TestOpenExistingCopy(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name string
		// prepare puts something in dir, then pushes newer to the source.
		prepare func(tb testing.TB, src *source, dir string)
		reused  bool
	}{
		{
			name:    "healthy copy",
			prepare: func(tb testing.TB, src *source, dir string) { open(tb, src, dir) },
			reused:  true,
		},
		{
			name: "copy without objects",
			prepare: func(tb testing.TB, src *source, dir string) {
				open(tb, src, dir)
				err := os.RemoveAll(filepath.Join(dir, "objects"))
				if err != nil {
					tb.Fatal(err)
				}
			},
		},
		{
			name: "copy missing a tree",
			prepare: func(tb testing.TB, src *source, dir string) {
				copyDir(tb, src.dir, dir)
				removeObject(tb, filepath.Join(dir, ".git"), src.entryOf(mustSourceHead(tb, src), "d").Hash.String())
			},
		},
		{
			name: "not a repository",
			prepare: func(tb testing.TB, _ *source, dir string) {
				err := os.MkdirAll(dir, 0o755)
				if err != nil {
					tb.Fatal(err)
				}
				err = os.WriteFile(filepath.Join(dir, "junk"), []byte("not git"), 0o644)
				if err != nil {
					tb.Fatal(err)
				}
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			src := newSource(t)
			old := src.commit(map[string]*string{"d/x.md": new("# X\n")})
			dir := filepath.Join(t.TempDir(), "copy")
			testCase.prepare(t, src, dir)
			newer := src.commit(map[string]*string{"top.md": new("# Top\n")})
			want := newer
			if testCase.reused {
				want = old
			}

			m, err := gitsync.Open(t.Context(), dir, gitsync.Source{URL: src.dir, Branch: "master"})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if got := mustHead(t, m); got != want {
				t.Errorf("got head %s, want %s (reused %v)", got, want, testCase.reused)
			}
			_, err = m.Files(want)
			if err != nil {
				t.Errorf("Files: %v", err)
			}
		})
	}
}

func open(tb testing.TB, src *source, dir string) {
	tb.Helper()
	_, err := gitsync.Open(tb.Context(), dir, gitsync.Source{URL: src.dir, Branch: "master"})
	if err != nil {
		tb.Fatalf("Open: %v", err)
	}
}

func mustSourceHead(tb testing.TB, src *source) string {
	tb.Helper()
	ref, err := src.repo.Head()
	if err != nil {
		tb.Fatal(err)
	}
	return ref.Hash().String()
}

func TestOpenFails(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		auth    gitsync.TokenSource
		missing bool
		wantErr error
	}{
		{name: "missing remote", missing: true},
		{name: "no token", auth: failingToken{}, wantErr: errToken},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			src := newSource(t)
			src.commit(map[string]*string{"a.md": new("a")})
			url := src.dir
			if testCase.missing {
				url = filepath.Join(t.TempDir(), "nothing")
			}
			dir := filepath.Join(t.TempDir(), "copy")
			_, err := gitsync.Open(t.Context(), dir, gitsync.Source{URL: url, Branch: "master", Auth: testCase.auth})
			if err == nil || !errors.Is(err, testCase.wantErr) && testCase.wantErr != nil {
				t.Errorf("got error %v, want %v", err, testCase.wantErr)
			}
			_, statErr := os.Stat(dir)
			if !os.IsNotExist(statErr) {
				t.Errorf("got %s left behind after a failed Open, want nothing", dir)
			}
		})
	}
}

func TestUsable(t *testing.T) {
	t.Parallel()
	src := newSource(t)
	src.commit(map[string]*string{"a.md": new("a")})
	dir := filepath.Join(t.TempDir(), "copy")
	open(t, src, dir)
	testCases := []struct {
		name   string
		dir    string
		branch string
		want   bool
	}{
		{name: "healthy copy", dir: dir, branch: "master", want: true},
		{name: "other branch", dir: dir, branch: "main"},
		{name: "empty folder", dir: t.TempDir(), branch: "master"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := gitsync.Usable(testCase.dir, testCase.branch); got != testCase.want {
				t.Errorf("got Usable %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestFetchAndDiff(t *testing.T) {
	t.Parallel()
	src := newSource(t)
	first := src.commit(map[string]*string{
		"keep.md":       new("same"),
		"edit.md":       new("before"),
		"delete.md":     new("bye"),
		"README.txt":    new("ignored"),
		"to-symlink.md": new("text"),
		"grows.md":      new("small"),
		"to-lfs.md":     new("real content"),
		"target.md":     new("target"),
		"run.md":        new("exec"),
	})
	m := openCopy(t, src)

	// A few pushes, so the copy has to fetch on top of a shallow history.
	src.commit(map[string]*string{"edit.md": new("middle")})
	src.makeExecutable("run.md")
	second := src.commit(map[string]*string{
		"edit.md":       new("after"),
		"delete.md":     nil,
		"new.md":        new("hello"),
		"README.txt":    new("still ignored"),
		".hidden/x.md":  new("ignored"),
		"to-symlink.md": new("->target.md"),
		"grows.md":      new(strings.Repeat("x", gitsync.MaxFileSize+1)),
		"to-lfs.md":     new(lfsPointer),
		"run.md":        new("exec, changed"),
	})

	head, err := m.Fetch(t.Context(), nil)
	if err != nil || head != second {
		t.Fatalf("got Fetch %s, %v; want %s", head, err, second)
	}
	if !m.Has(first) {
		t.Error("got Has false for the previous commit after a shallow fetch, want true")
	}
	if got := m.Packs(); got != 2 {
		t.Errorf("got %d packs after a fetch, want 2", got)
	}

	changes, err := m.Diff(first, second)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	want := []gitsync.File{
		{Path: "delete.md"},
		{Path: "edit.md", Blob: blobOf("after")},
		{Path: "grows.md"},
		{Path: "new.md", Blob: blobOf("hello")},
		{Path: "run.md", Blob: blobOf("exec, changed")},
		{Path: "to-lfs.md"},
		{Path: "to-symlink.md"},
	}
	if diff := cmp.Diff(want, changes); diff != "" {
		t.Errorf("Diff (-want +got):\n%s", diff)
	}

	again, err := m.Fetch(t.Context(), nil)
	if err != nil || again != second {
		t.Errorf("got Fetch with nothing new %s, %v; want %s", again, err, second)
	}
}

// The copy fetches from the source it's given now, not from the remote stored when it was cloned.
func TestFetchFollowsTheSource(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name string
		move func(tb testing.TB, m *gitsync.Mirror, dir, url string) *gitsync.Mirror
	}{
		{
			name: "SetSource",
			move: func(_ testing.TB, m *gitsync.Mirror, _, url string) *gitsync.Mirror {
				m.SetSource(url, nil)
				return m
			},
		},
		{
			name: "reopened with another URL",
			move: func(tb testing.TB, _ *gitsync.Mirror, dir, url string) *gitsync.Mirror {
				m, err := gitsync.Open(tb.Context(), dir, gitsync.Source{URL: url, Branch: "master"})
				if err != nil {
					tb.Fatalf("Open: %v", err)
				}
				return m
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			first, second := newSource(t), newSource(t)
			first.commit(map[string]*string{"a.md": new("# First\n")})
			want := second.commit(map[string]*string{"b.md": new("# Second\n")})
			dir := filepath.Join(t.TempDir(), "copy")
			m, err := gitsync.Open(t.Context(), dir, gitsync.Source{URL: first.dir, Branch: "master"})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			m = testCase.move(t, m, dir, second.dir)
			got, err := m.Fetch(t.Context(), nil)
			if err != nil || got != want {
				t.Errorf("got Fetch %s, %v; want %s", got, err, want)
			}
		})
	}
}

func TestFetchFails(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		auth    gitsync.TokenSource
		missing bool
		wantErr error
	}{
		{name: "missing remote", missing: true},
		{name: "no token", auth: failingToken{}, wantErr: errToken},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			src := newSource(t)
			head := src.commit(map[string]*string{"a.md": new("a")})
			m := openCopy(t, src)
			url := src.dir
			if testCase.missing {
				url = filepath.Join(t.TempDir(), "nothing")
			}
			m.SetSource(url, testCase.auth)
			_, err := m.Fetch(t.Context(), nil)
			if err == nil || !errors.Is(err, testCase.wantErr) && testCase.wantErr != nil {
				t.Errorf("got error %v, want %v", err, testCase.wantErr)
			}
			if got := mustHead(t, m); got != head {
				t.Errorf("got head %s after a failed Fetch, want %s", got, head)
			}
		})
	}
}

// BlobAt finds names go-git's own lookup rejects, and tells an NFC name from its NFD twin: git compares bytes.
func TestBlobAt(t *testing.T) {
	t.Parallel()
	nfc, nfd := norm.NFC.String("café.md"), norm.NFD.String("café.md")
	src := newSource(t)
	head := src.rawCommit(map[string]string{
		"README.md":    "# Vault\n",
		"ops/fleet.md": "# Fleet\n",
		"a\tb.md":      "# Tab\n",
		"d\tx/a.md":    "# In a tab folder\n",
		nfc:            "# NFC\n",
		nfd:            "# NFD\n",
	})
	m := openCopy(t, src)
	testCases := []struct {
		name string
		path string
		want string
	}{
		{name: "top-level file", path: "README.md", want: blobOf("# Vault\n")},
		{name: "file in a folder", path: "ops/fleet.md", want: blobOf("# Fleet\n")},
		{name: "tab in the name", path: "a\tb.md", want: blobOf("# Tab\n")},
		{name: "tab in the folder name", path: "d\tx/a.md", want: blobOf("# In a tab folder\n")},
		{name: "NFC name", path: nfc, want: blobOf("# NFC\n")},
		{name: "NFD name", path: nfd, want: blobOf("# NFD\n")},
		{name: "missing file", path: "missing.md"},
		{name: "a folder", path: "ops"},
		{name: "missing file in a folder", path: "ops/missing.md"},
		{name: "file as a folder", path: "README.md/x.md"},
		{name: "missing folder", path: "nope/fleet.md"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, err := m.BlobAt(head, testCase.path)
			if err != nil || got != testCase.want {
				t.Errorf("got BlobAt(%q) %q, %v; want %q", testCase.path, got, err, testCase.want)
			}
		})
	}
}

// A control character in a file or folder name skips that file, not the vault, in a diff too.
func TestControlCharacterInAName(t *testing.T) {
	t.Parallel()
	src := newSource(t)
	first := src.rawCommit(map[string]string{"a\tb.md": "# Tab\n", "d\tx/a.md": "# A\n", "ok.md": "# OK\n"})
	m := openCopy(t, src)
	files, err := m.Files(first)
	if diff := cmp.Diff([]gitsync.File{{Path: "ok.md", Blob: blobOf("# OK\n")}}, files); err != nil || diff != "" {
		t.Errorf("Files, error %v (-want +got):\n%s", err, diff)
	}

	second := src.rawCommit(map[string]string{"a\tb.md": "# Tab2\n", "d\tx/a.md": "# A2\n", "d\tx/b.md": "# B\n", "ok.md": "# OK2\n"})
	head, err := m.Fetch(t.Context(), nil)
	if err != nil || head != second {
		t.Fatalf("got Fetch %s, %v; want %s", head, err, second)
	}
	changes, err := m.Diff(first, second)
	if diff := cmp.Diff([]gitsync.File{{Path: "ok.md", Blob: blobOf("# OK2\n")}}, changes); err != nil || diff != "" {
		t.Errorf("Diff, error %v (-want +got):\n%s", err, diff)
	}
}

// A missing object is a broken copy, not an empty folder or file.
func TestBrokenCopy(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name string
		// missing names the object to remove, by commit ("first" or "second") and path.
		commit, missing string
		read            func(m *gitsync.Mirror, first, second string) error
	}{
		{
			name:   "Files with a missing subtree",
			commit: "second", missing: "e",
			read: func(m *gitsync.Mirror, _, second string) error { _, err := m.Files(second); return err },
		},
		{
			name:   "Paths with a missing subtree",
			commit: "second", missing: "e",
			read: func(m *gitsync.Mirror, _, second string) error { _, err := m.Paths(second); return err },
		},
		{
			name:   "BlobAt with a missing subtree",
			commit: "second", missing: "e",
			read: func(m *gitsync.Mirror, _, second string) error { _, err := m.BlobAt(second, "e/y.md"); return err },
		},
		{
			name:   "Diff with a missing old subtree",
			commit: "first", missing: "d",
			read: func(m *gitsync.Mirror, first, second string) error { _, err := m.Diff(first, second); return err },
		},
		{
			name:   "Diff with a missing new blob",
			commit: "second", missing: "e/y.md",
			read: func(m *gitsync.Mirror, first, second string) error { _, err := m.Diff(first, second); return err },
		},
		{
			name:   "Blob",
			commit: "second", missing: "e/y.md",
			read: func(m *gitsync.Mirror, _, _ string) error { _, err := m.Blob(blobOf("# Y2\n")); return err },
		},
		{
			name:   "Files with a missing commit",
			commit: "second", missing: "",
			read: func(m *gitsync.Mirror, _, second string) error { _, err := m.Files(second); return err },
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			src := newSource(t)
			first := src.commit(map[string]*string{"d/x.md": new("# X\n"), "e/y.md": new("# Y\n"), "top.md": new("# Top\n")})
			second := src.commit(map[string]*string{"d/x.md": nil, "e/y.md": new("# Y2\n")})
			m, gitDir := openWorkingCopy(t, src)
			commit := map[string]string{"first": first, "second": second}[testCase.commit]
			hash := commit
			if testCase.missing != "" {
				hash = src.entryOf(commit, testCase.missing).Hash.String()
			}
			removeObject(t, gitDir, hash)

			err := testCase.read(m, first, second)
			if !errors.Is(err, gitsync.ErrBroken) {
				t.Errorf("got error %v, want ErrBroken", err)
			}
		})
	}
}

// A diff needs neither the old content of a deleted file nor the folders speki doesn't read, and skips a
// change of the executable bit alone.
func TestDiffNeedsOnlyWhatItReads(t *testing.T) {
	t.Parallel()
	src := newSource(t)
	first := src.commit(map[string]*string{"gone.md": new("# Gone\n"), ".obsidian/x.md": new("x"), "run.md": new("# Run\n")})
	src.makeExecutable("run.md")
	second := src.commit(map[string]*string{"gone.md": nil, ".obsidian/x.md": new("y")})
	if mode := src.entryOf(second, "run.md").Mode; mode != filemode.Executable {
		t.Fatalf("got run.md mode %v in the second commit, want %v", mode, filemode.Executable)
	}
	m, gitDir := openWorkingCopy(t, src)
	removeObject(t, gitDir, src.entryOf(first, "gone.md").Hash.String())
	removeObject(t, gitDir, src.entryOf(first, ".obsidian").Hash.String())
	removeObject(t, gitDir, src.entryOf(second, ".obsidian").Hash.String())

	changes, err := m.Diff(first, second)
	if diff := cmp.Diff([]gitsync.File{{Path: "gone.md"}}, changes); err != nil || diff != "" {
		t.Errorf("Diff, error %v (-want +got):\n%s", err, diff)
	}
}

// Errors from damaged object stores count as a broken copy. Most can't be produced through a real copy.
func TestBrokenErrors(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "object not found", err: plumbing.ErrObjectNotFound, want: true},
		{name: "unexpected EOF", err: io.ErrUnexpectedEOF, want: true},
		{name: "zlib checksum", err: zlib.ErrChecksum, want: true},
		{name: "file not found", err: fs.ErrNotExist, want: true},
		{name: "other error", err: errors.New("network down")},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			err := gitsync.MarkBroken(fmt.Errorf("x: %w", testCase.err))
			if got := errors.Is(err, gitsync.ErrBroken); got != testCase.want {
				t.Errorf("got broken %v, want %v", got, testCase.want)
			}
			if !errors.Is(err, testCase.err) {
				t.Errorf("got %v, want it to wrap %v", err, testCase.err)
			}
		})
	}
}
