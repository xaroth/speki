// Package access decides what a signed-in user may do: whether they get in at all, and which vaults they
// may read and write. GitHub is the source of truth; answers are cached for a minute.
package access

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/eve-online-tools/speki/gh"
	"github.com/eve-online-tools/speki/vault"
)

// Reasons a user is turned away.
var (
	// ErrUnauthorized means GitHub rejected the user's token: it expired or was revoked.
	ErrUnauthorized = errors.New("GitHub no longer accepts this sign-in")
	// ErrNotMember means the user isn't an active member of any required organisation.
	ErrNotMember = errors.New("you aren't a member of an organisation this speki requires")
	// ErrNoVaults means the user can't read any vault.
	ErrNoVaults = errors.New("you can't read any vault on this speki")
)

// User is a signed-in user, from their speki token.
type User struct {
	ID     int64
	Login  string
	Token  string // the user's GitHub token
	Client string // the MCP client they signed in with
	Write  bool   // signed in with write access
}

// Grant is what one user may do right now.
type Grant struct {
	user     User
	readable []vault.Vault // sorted by name
	writable map[int64]bool
}

// NewGrant returns a grant for u to read readable and write the vaults in writable. Resolver makes the
// real ones; tests make their own.
func NewGrant(u User, readable []vault.Vault, writable []int64) *Grant {
	g := &Grant{user: u, readable: slices.Clone(readable), writable: make(map[int64]bool)}
	slices.SortFunc(g.readable, func(a, b vault.Vault) int { return cmp.Compare(a.Name, b.Name) })
	if u.Write {
		for _, id := range writable {
			g.writable[id] = true
		}
	}
	return g
}

// User returns who the grant is for, including their GitHub token: the one place speki acts as the user.
func (g *Grant) User() User { return g.user }

// Readable returns the vaults the user may read, sorted by name.
func (g *Grant) Readable() []vault.Vault { return slices.Clone(g.readable) }

// Vault returns a readable vault by name.
func (g *Grant) Vault(name string) (vault.Vault, bool) {
	for _, v := range g.readable {
		if v.Name == name {
			return v, true
		}
	}
	return vault.Vault{}, false
}

// CanWrite reports whether the user may write to a vault.
func (g *Grant) CanWrite(repoID int64) bool { return g.writable[repoID] }

// ReadOnlySession reports whether the user signed in without write access.
func (g *Grant) ReadOnlySession() bool { return !g.user.Write }

// githubAPI is what the resolver asks GitHub, with the user's own token.
type githubAPI interface {
	ActiveMember(ctx context.Context, token, org string) (bool, error)
	UserRepositories(ctx context.Context, token string) ([]gh.RepoAccess, error)
}

// Resolver turns users into grants.
type Resolver struct {
	github       githubAPI
	vaults       func() []vault.Vault // the vaults speki serves
	requiredOrgs []string
	ttl          time.Duration
	now          func() time.Time

	mu    sync.Mutex
	cache map[[sha256.Size]byte]answer // by GitHub token, never by user: tokens can differ in scope or be revoked
}

// answer is what GitHub said about one token.
type answer struct {
	repos   map[int64]gh.RepoAccess
	expires time.Time
}

// NewResolver returns a resolver. vaults returns the vaults speki currently serves; requiredOrgs may be
// empty.
func NewResolver(github githubAPI, vaults func() []vault.Vault, requiredOrgs []string) *Resolver {
	return &Resolver{
		github:       github,
		vaults:       vaults,
		requiredOrgs: requiredOrgs,
		ttl:          time.Minute,
		now:          time.Now,
		cache:        make(map[[sha256.Size]byte]answer),
	}
}

// Resolve returns what u may do. It errs with ErrUnauthorized, ErrNotMember or ErrNoVaults when u
// doesn't get in.
func (r *Resolver) Resolve(ctx context.Context, u User) (*Grant, error) {
	repos, err := r.lookup(ctx, u.Token)
	if err != nil {
		return nil, err
	}
	var readable []vault.Vault
	var writable []int64
	for _, v := range r.vaults() {
		a, ok := repos[v.RepoID]
		if !ok || !a.Pull {
			continue
		}
		readable = append(readable, v)
		if a.Push {
			writable = append(writable, v.RepoID)
		}
	}
	if len(readable) == 0 {
		return nil, ErrNoVaults
	}
	return NewGrant(u, readable, writable), nil
}

// CheckMember reports whether the token's user passes the organisation gate, for sign-in.
func (r *Resolver) CheckMember(ctx context.Context, token string) error {
	_, err := r.lookup(ctx, token)
	return err
}

// lookup returns the repositories the token's user can see, after the organisation gate, from the cache
// when it's fresh.
func (r *Resolver) lookup(ctx context.Context, token string) (map[int64]gh.RepoAccess, error) {
	key := sha256.Sum256([]byte(token))
	now := r.now()
	r.mu.Lock()
	a, ok := r.cache[key]
	r.mu.Unlock()
	if ok && now.Before(a.expires) {
		return a.repos, nil
	}

	err := r.checkOrgs(ctx, token)
	if err != nil {
		return nil, err
	}
	list, err := r.github.UserRepositories(ctx, token)
	if err != nil {
		return nil, githubError(err)
	}
	repos := make(map[int64]gh.RepoAccess, len(list))
	for _, a := range list {
		repos[a.RepoID] = a
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for k, old := range r.cache { // drop stale answers so the cache doesn't grow without bound
		if !now.Before(old.expires) {
			delete(r.cache, k)
		}
	}
	r.cache[key] = answer{repos: repos, expires: now.Add(r.ttl)}
	return repos, nil
}

func (r *Resolver) checkOrgs(ctx context.Context, token string) error {
	if len(r.requiredOrgs) == 0 {
		return nil
	}
	for _, org := range r.requiredOrgs {
		ok, err := r.github.ActiveMember(ctx, token, org)
		if err != nil {
			return githubError(err)
		}
		if ok {
			return nil
		}
	}
	return ErrNotMember
}

func githubError(err error) error {
	if errors.Is(err, gh.ErrUnauthorized) {
		return fmt.Errorf("%w: %w", ErrUnauthorized, err)
	}
	return err
}
