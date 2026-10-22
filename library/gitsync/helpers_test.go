package gitsync_test

import (
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

	"github.com/xaroth/speki/library/gitsync"
)

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
