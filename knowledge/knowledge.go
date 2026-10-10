// Package knowledge is what speki's tools do: list, search, read, history, write, edit, move and delete
// over the vaults a user may see. It knows nothing about MCP. Every function that touches a vault takes
// the user's grant and checks it before anything else.
package knowledge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/eve-online-tools/speki/access"
	"github.com/eve-online-tools/speki/gh"
	"github.com/eve-online-tools/speki/index"
	"github.com/eve-online-tools/speki/vault"
)

// Copy is one vault's local copy.
type Copy interface {
	Index() *index.Index
	Blob(sha string) ([]byte, error)
	Paths(commit string) ([]string, error) // every file in the git tree at commit
}

// Vaults hands out the local copies of the vaults speki serves.
type Vaults interface {
	Copy(repoID int64) (Copy, bool)
	// Sync brings a vault's copy up to date with GitHub, and returns when it's done.
	Sync(ctx context.Context, repoID int64) error
}

// gitHub is what knowledge asks GitHub, as the user.
type gitHub interface {
	Commits(ctx context.Context, token, repo, branch, path string, limit int) ([]gh.Commit, error)
	Commit(ctx context.Context, token string, req gh.CommitRequest) (string, error)
}

// Base answers the tools' questions.
type Base struct {
	vaults Vaults
	github gitHub
	logger *slog.Logger
	now    func() time.Time

	mu    sync.Mutex
	locks map[int64]*sync.Mutex // one write at a time per vault
}

// New returns a Base.
func New(vaults Vaults, github gitHub, logger *slog.Logger) *Base {
	return &Base{vaults: vaults, github: github, logger: logger, now: time.Now, locks: map[int64]*sync.Mutex{}}
}

// lock takes a vault's write lock and returns its unlock.
func (b *Base) lock(repoID int64) func() {
	b.mu.Lock()
	l, ok := b.locks[repoID]
	if !ok {
		l = &sync.Mutex{}
		b.locks[repoID] = l
	}
	b.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// open returns a vault the user may read, by name, with its local copy.
func (b *Base) open(g *access.Grant, name string) (vault.Vault, Copy, error) {
	v, ok := g.Vault(name)
	if !ok {
		return vault.Vault{}, nil, noVault(name)
	}
	c, ok := b.vaults.Copy(v.RepoID)
	if !ok {
		return vault.Vault{}, nil, noVault(name)
	}
	return v, c, nil
}

func noVault(name string) *Error {
	return errorf(NotFound, "there's no vault named %q that you can read. Call list without arguments to see your vaults", name)
}

// indexError turns an index that closed under a request (its vault was just removed) into a not_found.
func indexError(name string, err error) error {
	if errors.Is(err, index.ErrClosed) {
		return noVault(name)
	}
	return fmt.Errorf("%s: %w", name, err)
}
