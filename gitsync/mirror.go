// Package gitsync keeps a shallow, bare copy of one git repository and reads files from it. It knows
// nothing about vaults: it clones, fetches, diffs trees and reads blobs.
package gitsync

import (
	"bytes"
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
	"unicode"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/filesystem/dotgit"

	"github.com/eve-online-tools/speki/document"
)

// MaxFileSize is the largest file speki reads from a repository: the largest document. (gitsync may
// import document; document must never import gitsync.)
const MaxFileSize = document.MaxSize

// lfsPointer starts every git LFS pointer file. A clone has only the pointer, not the content.
const lfsPointer = "version https://git-lfs.github.com/spec/v1"

// TokenSource hands out credentials for the remote. It may be nil for remotes that need none.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// Mirror is a shallow, bare copy of one branch of one repository. Reads may run concurrently with each
// other, but not with Fetch or SetSource: the caller serialises them.
type Mirror struct {
	dir    string
	branch plumbing.ReferenceName
	url    string // where Fetch fetches from: never the remote stored in the copy
	auth   TokenSource
	repo   *git.Repository
}

// File is a markdown file speki reads.
type File struct {
	Path string
	Blob string // SHA
}

// Change is a markdown file that changed between two commits. Blob is empty when it was deleted, or
// stopped being a file speki reads.
type Change struct {
	Path string
	Blob string
}

// Source says where a copy comes from.
type Source struct {
	URL    string // a URL git understands
	Branch string
	Auth   TokenSource // may be nil
	// Progress receives git's progress messages while cloning. May be nil.
	Progress io.Writer
}

// Open opens the copy in dir, or clones src into it when dir is empty or the copy there is broken.
func Open(ctx context.Context, dir string, src Source) (*Mirror, error) {
	m := &Mirror{
		dir:    dir,
		branch: plumbing.NewBranchReferenceName(src.Branch),
		url:    src.URL,
		auth:   src.Auth,
	}

	repo, err := git.PlainOpen(dir)
	switch {
	case err == nil && healthy(repo, m.branch):
		m.repo = repo
		return m, nil
	case err == nil, errors.Is(err, git.ErrRepositoryNotExists):
		// Nothing there yet, or something that isn't a usable copy: start over.
		err = os.RemoveAll(dir)
		if err != nil {
			return nil, fmt.Errorf("remove broken copy: %w", err)
		}
	default:
		return nil, fmt.Errorf("open %s: %w", dir, err) // maybe temporary; don't throw the copy away
	}

	httpAuth, err := m.basicAuth(ctx)
	if err != nil {
		return nil, err
	}
	repo, err = git.PlainCloneContext(ctx, dir, true, &git.CloneOptions{
		URL:           src.URL,
		Progress:      src.Progress,
		Auth:          httpAuth,
		ReferenceName: m.branch,
		SingleBranch:  true,
		Depth:         1,
		Tags:          git.NoTags,
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("clone: %w", err)
	}
	m.repo = repo
	return m, nil
}

// Usable reports whether dir holds a copy of branch that can be used without the network.
func Usable(dir, branch string) bool {
	repo, err := git.PlainOpen(dir)
	return err == nil && healthy(repo, plumbing.NewBranchReferenceName(branch))
}

func healthy(repo *git.Repository, branch plumbing.ReferenceName) bool {
	ref, err := repo.Reference(branch, true)
	if err != nil {
		return false
	}
	tree, err := commitTree(repo, ref.Hash())
	if err != nil {
		return false
	}
	return walk(repo, tree, "", func(string, object.TreeEntry) error { return nil }) == nil
}

// ErrBroken means the copy is missing or has a corrupt object: it has to be cloned again.
var ErrBroken = errors.New("the git copy is broken")

// broken marks errors about missing or corrupt objects with ErrBroken.
func broken(err error) error {
	if errors.Is(err, plumbing.ErrObjectNotFound) || errors.Is(err, plumbing.ErrInvalidType) ||
		errors.Is(err, dotgit.ErrPackfileNotFound) || errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, zlib.ErrHeader) || errors.Is(err, zlib.ErrChecksum) {
		return fmt.Errorf("%w: %w", ErrBroken, err)
	}
	return err
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

func (m *Mirror) basicAuth(ctx context.Context) (transport.AuthMethod, error) {
	if m.auth == nil {
		return nil, nil
	}
	token, err := m.auth.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("git credentials: %w", err)
	}
	return &githttp.BasicAuth{Username: "x-access-token", Password: token}, nil
}

// SetSource changes where the copy fetches from, and with which credentials, for a repository that was
// renamed or moved to another installation. The copy itself stays: it's the same repository.
func (m *Mirror) SetSource(url string, auth TokenSource) {
	m.url = url
	m.auth = auth
}

// Head returns the commit the local copy of the branch is at.
func (m *Mirror) Head() (string, error) {
	ref, err := m.repo.Reference(m.branch, true)
	if err != nil {
		return "", fmt.Errorf("local head: %w", err)
	}
	return ref.Hash().String(), nil
}

// Fetch updates the branch from the remote, depth 1, and returns the commit it is at. progress, which
// may be nil, receives git's progress messages.
func (m *Mirror) Fetch(ctx context.Context, progress io.Writer) (string, error) {
	httpAuth, err := m.basicAuth(ctx)
	if err != nil {
		return "", err
	}
	spec := config.RefSpec(fmt.Sprintf("+%s:%s", m.branch, m.branch))
	err = m.repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: git.DefaultRemoteName,
		RemoteURL:  m.url, // the URL the copy was cloned from may name another repository by now
		RefSpecs:   []config.RefSpec{spec},
		Depth:      1,
		Auth:       httpAuth,
		Tags:       git.NoTags,
		Force:      true,
		Progress:   progress,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return "", fmt.Errorf("fetch: %w", err)
	}
	return m.Head()
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

// Diff returns the markdown files that changed between two commits. Both must be in the copy.
func (m *Mirror) Diff(from, to string) ([]Change, error) {
	fromTree, err := commitTree(m.repo, plumbing.NewHash(from))
	if err != nil {
		return nil, err
	}
	toTree, err := commitTree(m.repo, plumbing.NewHash(to))
	if err != nil {
		return nil, err
	}
	var changes []Change
	err = diffTrees(m.repo, fromTree, toTree, "", func(name string, before, after object.TreeEntry) error {
		ok, err := m.eligible(name, after)
		if err != nil {
			return err
		}
		if ok {
			if before.Hash == after.Hash && isFile(before.Mode) {
				return nil // only the executable bit changed
			}
			changes = append(changes, Change{Path: name, Blob: after.Hash.String()})
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
			changes = append(changes, Change{Path: name})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("diff %s..%s: %w", from, to, err)
	}
	return changes, nil
}

// Has reports whether commit is in the copy, so it can be diffed against.
func (m *Mirror) Has(commit string) bool {
	_, err := commitTree(m.repo, plumbing.NewHash(commit))
	return err == nil
}

// Blob returns a file's content by SHA. Blobs larger than MaxFileSize are refused.
func (m *Mirror) Blob(sha string) ([]byte, error) {
	blob, err := m.repo.BlobObject(plumbing.NewHash(sha))
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w", sha, broken(err))
	}
	if blob.Size > MaxFileSize {
		return nil, fmt.Errorf("blob %s: %d bytes, more than %d", sha, blob.Size, MaxFileSize)
	}
	r, err := blob.Reader()
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w", sha, broken(err))
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w", sha, broken(err))
	}
	return data, nil
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

// Packs returns how many pack files the copy has. Every fetch adds one; too many slow reads down.
func (m *Mirror) Packs() int {
	packs, err := filepath.Glob(filepath.Join(m.dir, "objects", "pack", "*.pack"))
	if err != nil {
		return 0
	}
	return len(packs)
}
