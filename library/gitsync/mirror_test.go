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

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/google/go-cmp/cmp"
	"golang.org/x/text/unicode/norm"

	"github.com/xaroth/speki/library/gitsync"
)

const lfsPointer = "version https://git-lfs.github.com/spec/v1\noid sha256:abc\nsize 12\n"

var errToken = errors.New("no token")

type failingToken struct{}

func (failingToken) Token(context.Context) (string, error) { return "", errToken }

func mustHead(tb testing.TB, m *gitsync.Mirror) string {
	tb.Helper()
	h, err := m.Head()
	if err != nil {
		tb.Fatal(err)
	}
	return h
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

// OpenLocal never clones and never removes what it can't use: callers decide.
func TestOpenLocal(t *testing.T) {
	t.Parallel()
	src := newSource(t)
	src.commit(map[string]*string{"a.md": new("a")})
	dir := filepath.Join(t.TempDir(), "copy")
	open(t, src, dir)
	testCases := []struct {
		name    string
		dir     string
		branch  string
		wantErr error
	}{
		{name: "healthy copy", dir: dir, branch: "master"},
		{name: "other branch", dir: dir, branch: "main", wantErr: gitsync.ErrBroken},
		{name: "empty folder", dir: t.TempDir(), branch: "master", wantErr: gitsync.ErrBroken},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			m, err := gitsync.OpenLocal(testCase.dir, gitsync.Source{URL: src.dir, Branch: testCase.branch})
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("got error %v, want %v", err, testCase.wantErr)
			}
			if err == nil {
				head, err := m.Head()
				if err != nil || head != mustSourceHead(t, src) {
					t.Errorf("got head %s, %v, want %s", head, err, mustSourceHead(t, src))
				}
			}
			_, err = os.Stat(testCase.dir)
			if err != nil {
				t.Errorf("got %v for %s, want it left in place", err, testCase.dir)
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
