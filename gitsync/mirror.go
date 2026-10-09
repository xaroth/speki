// Package gitsync keeps a shallow, bare copy of one git repository and reads files from it. It knows
// nothing about vaults: it clones, fetches, diffs trees and reads blobs.
package gitsync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// MaxFileSize is the largest file speki reads from a repository.
const MaxFileSize = 1 << 20

// lfsPointer starts every git LFS pointer file. A clone has only the pointer, not the content.
const lfsPointer = "version https://git-lfs.github.com/spec/v1"

// TokenSource hands out credentials for the remote. It may be nil for remotes that need none.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// Mirror is a shallow, bare copy of one branch of one repository. Reads may run concurrently with each
// other, but not with Fetch: the caller serialises them.
type Mirror struct {
	dir    string
	branch plumbing.ReferenceName
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

// healthy reports whether the copy has the branch and can walk its tree.
func healthy(repo *git.Repository, branch plumbing.ReferenceName) bool {
	ref, err := repo.Reference(branch, true)
	if err != nil {
		return false
	}
	tree, err := commitTree(repo, ref.Hash())
	if err != nil {
		return false
	}
	return tree.Files().ForEach(func(*object.File) error { return nil }) == nil
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

// Head returns the commit the local copy of the branch is at.
func (m *Mirror) Head() (string, error) {
	ref, err := m.repo.Reference(m.branch, true)
	if err != nil {
		return "", fmt.Errorf("local head: %w", err)
	}
	return ref.Hash().String(), nil
}

// Fetch updates the branch from the remote, depth 1, and returns the commit it's at now. progress, which
// may be nil, receives git's progress messages.
func (m *Mirror) Fetch(ctx context.Context, progress io.Writer) (string, error) {
	httpAuth, err := m.basicAuth(ctx)
	if err != nil {
		return "", err
	}
	spec := config.RefSpec(fmt.Sprintf("+%s:%s", m.branch, m.branch))
	err = m.repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: git.DefaultRemoteName,
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
	walker := object.NewTreeWalker(tree, true, nil)
	defer walker.Close()
	for {
		name, entry, err := walker.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", commit, err)
		}
		ok, err := m.eligible(name, entry)
		if err != nil {
			return nil, err
		}
		if ok {
			files = append(files, File{Path: name, Blob: entry.Hash.String()})
		}
	}
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
	diffs, err := object.DiffTree(fromTree, toTree)
	if err != nil {
		return nil, fmt.Errorf("diff %s..%s: %w", from, to, err)
	}

	var changes []Change
	for _, d := range diffs {
		after, err := m.eligible(d.To.Name, d.To.TreeEntry)
		if err != nil {
			return nil, err
		}
		if after {
			changes = append(changes, Change{Path: d.To.Name, Blob: d.To.TreeEntry.Hash.String()})
			continue
		}
		before, err := m.eligible(d.From.Name, d.From.TreeEntry)
		if err != nil {
			return nil, err
		}
		if before { // deleted, or no longer a file speki reads
			changes = append(changes, Change{Path: d.From.Name})
		}
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
		return nil, fmt.Errorf("blob %s: %w", sha, err)
	}
	if blob.Size > MaxFileSize {
		return nil, fmt.Errorf("blob %s: %d bytes, more than %d", sha, blob.Size, MaxFileSize)
	}
	r, err := blob.Reader()
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w", sha, err)
	}
	defer r.Close()
	return io.ReadAll(r)
}

// eligible reports whether a tree entry is a file speki reads. A zero entry (the missing side of a diff)
// isn't.
func (m *Mirror) eligible(name string, e object.TreeEntry) (bool, error) {
	if e.Mode != filemode.Regular && e.Mode != filemode.Executable {
		return false, nil
	}
	if !strings.HasSuffix(name, ".md") || hasDotSegment(name) {
		return false, nil
	}
	blob, err := m.repo.BlobObject(e.Hash)
	if err != nil {
		return false, fmt.Errorf("blob of %s: %w", name, err)
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
		return nil, fmt.Errorf("commit %s: %w", h, err)
	}
	t, err := c.Tree()
	if err != nil {
		return nil, fmt.Errorf("tree of %s: %w", h, err)
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
