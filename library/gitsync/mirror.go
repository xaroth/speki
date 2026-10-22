// Package gitsync keeps a shallow, bare copy of one git repository and reads files from it. It knows
// nothing about vaults: it clones, fetches, diffs trees and reads blobs.
package gitsync

import (
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/filesystem/dotgit"

	"github.com/xaroth/speki/document"
)

// MaxFileSize is the largest file speki reads from a repository: the largest document. (gitsync may
// import document; document must never import gitsync.)
const MaxFileSize = document.MaxSize

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
	m, err := OpenLocal(dir, src)
	switch {
	case err == nil:
		return m, nil
	case errors.Is(err, ErrBroken):
		// Nothing there yet, or something that isn't a usable copy: start over.
		err = os.RemoveAll(dir)
		if err != nil {
			return nil, fmt.Errorf("remove broken copy: %w", err)
		}
	default:
		return nil, err // maybe temporary; don't throw the copy away
	}

	m = newMirror(dir, src)
	httpAuth, err := m.basicAuth(ctx)
	if err != nil {
		return nil, err
	}
	repo, err := git.PlainCloneContext(ctx, dir, true, &git.CloneOptions{
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

// OpenLocal opens the copy in dir without the network. Unlike Open, it never removes or clones: a missing
// or unusable copy is an error wrapping ErrBroken.
func OpenLocal(dir string, src Source) (*Mirror, error) {
	m := newMirror(dir, src)
	repo, err := git.PlainOpen(dir)
	switch {
	case errors.Is(err, git.ErrRepositoryNotExists):
		return nil, fmt.Errorf("open %s: %w: %w", dir, ErrBroken, err)
	case err != nil:
		return nil, fmt.Errorf("open %s: %w", dir, err)
	case !healthy(repo, m.branch):
		return nil, fmt.Errorf("open %s: %w", dir, ErrBroken)
	}
	m.repo = repo
	return m, nil
}

func newMirror(dir string, src Source) *Mirror {
	return &Mirror{
		dir:    dir,
		branch: plumbing.NewBranchReferenceName(src.Branch),
		url:    src.URL,
		auth:   src.Auth,
	}
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

// Packs returns how many pack files the copy has. Every fetch adds one; too many slow reads down.
func (m *Mirror) Packs() int {
	packs, err := filepath.Glob(filepath.Join(m.dir, "objects", "pack", "*.pack"))
	if err != nil {
		return 0
	}
	return len(packs)
}
