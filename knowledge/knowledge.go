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

// githubAPI is what knowledge asks GitHub, as the user.
type githubAPI interface {
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
	github  githubAPI
	sandbox querier
	notices notifier
	logger  *slog.Logger
	now     func() time.Time
	writes  keyedLock // one write at a time per vault
	queries keyedLock // one query at a time per user
}

// New returns a Base.
func New(vaults Vaults, github githubAPI, sandbox querier, notices notifier, logger *slog.Logger) *Base {
	return &Base{
		vaults:  vaults,
		github:  github,
		sandbox: sandbox,
		notices: notices,
		logger:  logger,
		now:     time.Now,
	}
}

// keyedLock is a lock per key that a caller can stop waiting for. The zero value is ready to use.
type keyedLock struct {
	mu    sync.Mutex
	locks map[int64]chan struct{}
}

// lock takes key's lock, or gives up when ctx ends, and returns its unlock.
func (k *keyedLock) lock(ctx context.Context, key int64) (func(), error) {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[int64]chan struct{}{}
	}
	l, ok := k.locks[key]
	if !ok {
		l = make(chan struct{}, 1)
		k.locks[key] = l
	}
	k.mu.Unlock()
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

// sources returns the indexes to search or query: the named vaults, each once however often it's named,
// or every vault the user may read when names is empty.
func (b *Base) sources(g *access.Grant, names []string) ([]index.Source, error) {
	var sources []index.Source
	if len(names) == 0 {
		for _, v := range g.Readable() {
			c, ok := b.vaults.Copy(v.RepoID)
			if ok {
				sources = append(sources, index.Source{Name: v.Name, Index: c.Index()})
			}
		}
		return sources, nil
	}
	seen := map[int64]bool{}
	for _, name := range names {
		v, c, err := b.open(g, name)
		if err != nil {
			return nil, err
		}
		if seen[v.RepoID] { // named twice: searched once, or every hit would come back twice
			continue
		}
		seen[v.RepoID] = true
		sources = append(sources, index.Source{Name: v.Name, Index: c.Index()})
	}
	return sources, nil
}

func noVault(name string) *Error {
	return errorf(CodeNotFound, "there's no vault named %q that you can read. Call list without arguments to see your vaults", name)
}

// indexError turns an index that closed under a request (its vault was just removed) into a not_found.
func indexError(name string, err error) error {
	if errors.Is(err, index.ErrClosed) {
		return noVault(name)
	}
	return fmt.Errorf("%s: %w", name, err)
}
