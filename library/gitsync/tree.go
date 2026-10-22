package gitsync

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// lfsPointer starts every git LFS pointer file. A clone has only the pointer, not the content.
const lfsPointer = "version https://git-lfs.github.com/spec/v1"

// File is a markdown file speki reads.
type File struct {
	Path string
	Blob string // SHA
}

// walk calls fn for every entry under tree that isn't a folder. It looks every subtree up itself, so a
// missing object is an error rather than the end of the walk (go-git's walker stops quietly), and file
// names git allows but go-git's walker rejects, such as ones with a tab, are passed on.
func walk(repo *git.Repository, tree *object.Tree, prefix string, fn func(name string, e object.TreeEntry) error) error {
	for _, e := range tree.Entries {
		name := prefix + e.Name
		if e.Mode != filemode.Dir {
			err := fn(name, e)
			if err != nil {
				return err
			}
			continue
		}
		sub, err := object.GetTree(repo.Storer, e.Hash)
		if err != nil {
			return broken(fmt.Errorf("%s: %w", name, err))
		}
		err = walk(repo, sub, name+"/", fn)
		if err != nil {
			return err
		}
	}
	return nil
}

// diffTrees calls fn for every file that differs between two trees, with a zero entry for the side it's
// missing from. Subtrees with the same hash are skipped. Like walk, it looks subtrees up itself: go-git's
// DiffTree treats a missing subtree as empty, and refuses folder names with control characters. Names
// starting with a dot, which speki never reads, are skipped. Each folder's names are taken in sorted order.
func diffTrees(repo *git.Repository, from, to *object.Tree, prefix string, fn func(name string, before, after object.TreeEntry) error) error {
	var names []string
	entries := map[string][2]object.TreeEntry{}
	for side, t := range []*object.Tree{from, to} {
		if t == nil {
			continue
		}
		for _, e := range t.Entries {
			if strings.HasPrefix(e.Name, ".") {
				continue
			}
			pair, seen := entries[e.Name]
			if !seen {
				names = append(names, e.Name)
			}
			pair[side] = e
			entries[e.Name] = pair
		}
	}
	slices.Sort(names)
	for _, n := range names {
		pair := entries[n]
		before, after := pair[0], pair[1]
		if before.Hash == after.Hash && before.Mode == after.Mode {
			continue
		}
		name := prefix + n
		var beforeDir, afterDir *object.Tree
		var err error
		if before.Mode == filemode.Dir {
			beforeDir, err = object.GetTree(repo.Storer, before.Hash)
			if err != nil {
				return broken(fmt.Errorf("%s: %w", name, err))
			}
			before = object.TreeEntry{}
		}
		if after.Mode == filemode.Dir {
			afterDir, err = object.GetTree(repo.Storer, after.Hash)
			if err != nil {
				return broken(fmt.Errorf("%s: %w", name, err))
			}
			after = object.TreeEntry{}
		}
		if beforeDir != nil || afterDir != nil {
			err = diffTrees(repo, beforeDir, afterDir, name+"/", fn)
			if err != nil {
				return err
			}
		}
		if before != (object.TreeEntry{}) || after != (object.TreeEntry{}) {
			err = fn(name, before, after)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Files returns every file speki reads at commit: markdown, a regular file, not under a dot folder, not
// too large, not an LFS pointer.
func (m *Mirror) Files(commit string) ([]File, error) {
	tree, err := commitTree(m.repo, plumbing.NewHash(commit))
	if err != nil {
		return nil, err
	}
	var files []File
	err = walk(m.repo, tree, "", func(name string, e object.TreeEntry) error {
		ok, err := m.eligible(name, e)
		if ok {
			files = append(files, File{Path: name, Blob: e.Hash.String()})
		}
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("files at %s: %w", commit, err)
	}
	return files, nil
}

// Paths returns the path of every file in the tree at commit, of any kind: what a new document must not
// collide with. Folders are implied by the paths.
func (m *Mirror) Paths(commit string) ([]string, error) {
	tree, err := commitTree(m.repo, plumbing.NewHash(commit))
	if err != nil {
		return nil, err
	}
	var paths []string
	err = walk(m.repo, tree, "", func(name string, _ object.TreeEntry) error {
		paths = append(paths, name)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("paths at %s: %w", commit, err)
	}
	return paths, nil
}

// BlobAt returns the blob SHA of the file at path in the tree at commit, "" when there's no file there. It
// reads only the folders on the way, not the whole tree.
func (m *Mirror) BlobAt(commit, path string) (string, error) {
	tree, err := commitTree(m.repo, plumbing.NewHash(commit))
	if err != nil {
		return "", err
	}
	parts := strings.Split(path, "/")
	for _, dir := range parts[:len(parts)-1] {
		e, ok := entryNamed(tree, dir)
		if !ok || e.Mode != filemode.Dir {
			return "", nil
		}
		tree, err = object.GetTree(m.repo.Storer, e.Hash)
		if err != nil {
			return "", fmt.Errorf("%s at %s: %w", path, commit, broken(err))
		}
	}
	e, ok := entryNamed(tree, parts[len(parts)-1])
	if !ok || e.Mode == filemode.Dir {
		return "", nil
	}
	return e.Hash.String(), nil
}

// entryNamed finds an entry of tree by name. go-git's own lookup rejects some names git allows.
func entryNamed(tree *object.Tree, name string) (object.TreeEntry, bool) {
	for _, e := range tree.Entries {
		if e.Name == name {
			return e, true
		}
	}
	return object.TreeEntry{}, false
}

// Diff returns the markdown files that changed between two commits. Both must be in the copy. A file
// with an empty Blob was deleted, or stopped being a file speki reads.
func (m *Mirror) Diff(from, to string) ([]File, error) {
	fromTree, err := commitTree(m.repo, plumbing.NewHash(from))
	if err != nil {
		return nil, err
	}
	toTree, err := commitTree(m.repo, plumbing.NewHash(to))
	if err != nil {
		return nil, err
	}
	var changes []File
	err = diffTrees(m.repo, fromTree, toTree, "", func(name string, before, after object.TreeEntry) error {
		ok, err := m.eligible(name, after)
		if err != nil {
			return err
		}
		if ok {
			if before.Hash == after.Hash && isFile(before.Mode) {
				return nil // only the executable bit changed
			}
			changes = append(changes, File{Path: name, Blob: after.Hash.String()})
			return nil
		}
		ok, err = m.eligible(name, before)
		if errors.Is(err, ErrBroken) {
			// The old content is gone, but a deletion doesn't need it: removing a path the index
			// doesn't have is harmless.
			ok, err = true, nil
		}
		if err != nil {
			return err
		}
		if ok { // deleted, or no longer a file speki reads
			changes = append(changes, File{Path: name})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("diff %s..%s: %w", from, to, err)
	}
	return changes, nil
}

// eligible reports whether a tree entry is a file speki reads. A zero entry (the missing side of a diff)
// isn't.
func (m *Mirror) eligible(name string, e object.TreeEntry) (bool, error) {
	if !isFile(e.Mode) {
		return false, nil
	}
	if !strings.HasSuffix(name, ".md") || hasDotSegment(name) || strings.ContainsFunc(name, unicode.IsControl) {
		return false, nil // a control character in a name is allowed by git, but speki never reads it
	}
	blob, err := m.repo.BlobObject(e.Hash)
	if err != nil {
		return false, fmt.Errorf("blob of %s: %w", name, broken(err))
	}
	if blob.Size > MaxFileSize {
		return false, nil
	}
	if blob.Size >= int64(len(lfsPointer)) && blob.Size < 1024 {
		head, err := m.Blob(e.Hash.String())
		if err != nil {
			return false, err
		}
		if bytes.HasPrefix(head, []byte(lfsPointer)) {
			return false, nil
		}
	}
	return true, nil
}

func isFile(m filemode.FileMode) bool {
	return m == filemode.Regular || m == filemode.Executable
}

func hasDotSegment(name string) bool {
	for s := range strings.SplitSeq(name, "/") {
		if strings.HasPrefix(s, ".") {
			return true
		}
	}
	return false
}

func commitTree(repo *git.Repository, h plumbing.Hash) (*object.Tree, error) {
	c, err := repo.CommitObject(h)
	if err != nil {
		return nil, fmt.Errorf("commit %s: %w", h, broken(err))
	}
	t, err := c.Tree()
	if err != nil {
		return nil, fmt.Errorf("tree of %s: %w", h, broken(err))
	}
	return t, nil
}
