// Package knowledge is what speki's tools do: list, search, query, read, history, write, edit, move and
// delete over the vaults a user may see. It knows nothing about MCP. Every function that touches a vault takes
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
	Info() vault.Vault // the vault's current name and branch
	Index() *index.Index
	Blob(sha string) ([]byte, error)
	Paths(commit string) ([]string, error) // every file in the git tree at commit
}

// Vaults hands out the local copies of the vaults speki serves.
type Vaults interface {
	Copy(repoID int64) (Copy, bool)
	// Sync brings a vault's copy up to date with GitHub by applying changes, and returns when it's
	// done. It stops waiting when ctx ends, and leaves anything bigger, such as a full rebuild, to the
	// poller.
	Sync(ctx context.Context, repoID int64) error
}

// gitHub is what knowledge asks GitHub, as the user.
type gitHub interface {
	Commits(ctx context.Context, token, repo, branch, path string, limit int) ([]gh.Commit, error)
	Commit(ctx context.Context, token string, req gh.CommitRequest) (string, error)
}

// querier runs agents' SQL.
type querier interface {
	Query(ctx context.Context, sources []index.Source, sql string) (index.Rows, error)
}

// notifier tells other instances that a vault changed, so they sync before their next poll.
type notifier interface {
	Publish(ctx context.Context, repoID int64, commit string) error
}

// Base answers the tools' questions.
type Base struct {
	vaults  Vaults
	github  gitHub
	sandbox querier
	notices notifier
	logger  *slog.Logger
	now     func() time.Time

	mu      sync.Mutex
	locks   map[int64]chan struct{} // one write at a time per vault: a slot taken while writing
	queries map[int64]chan struct{} // one query at a time per user
}

// New returns a Base.
func New(vaults Vaults, github gitHub, sandbox querier, notices notifier, logger *slog.Logger) *Base {
	return &Base{
		vaults:  vaults,
		github:  github,
		sandbox: sandbox,
		notices: notices,
		logger:  logger,
		now:     time.Now,
		locks:   map[int64]chan struct{}{},
		queries: map[int64]chan struct{}{},
	}
}

// lock takes a vault's write lock, or gives up when ctx ends, and returns its unlock.
func (b *Base) lock(ctx context.Context, repoID int64) (func(), error) {
	b.mu.Lock()
	l, ok := b.locks[repoID]
	if !ok {
		l = make(chan struct{}, 1)
		b.locks[repoID] = l
	}
	b.mu.Unlock()
	select {
	case l <- struct{}{}:
		return func() { <-l }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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
