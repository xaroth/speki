// Package access decides what a signed-in user may do: whether they get in at all, and which vaults they
// may read and write. GitHub is the source of truth, narrowed by each vault's readers and writers; answers
// are cached for five minutes, in a cache every instance shares.
package access

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/eve-online-tools/speki/gh"
	"github.com/eve-online-tools/speki/vault"
)

// ErrUnauthorized means GitHub rejected the user's token: it expired or was revoked.
var ErrUnauthorized = errors.New("GitHub no longer accepts this sign-in")

// ErrNotMember means the user isn't an active member of any required organisation.
var ErrNotMember = errors.New("you aren't a member of an organisation this speki requires")

// ErrNoVaults means the user can't read any vault.
var ErrNoVaults = errors.New("you can't read any vault on this speki")

// ErrNotReady means the user can't read any vault being served yet, but may read one speki is still
// cloning and indexing.
var ErrNotReady = errors.New("speki is still loading your vaults; try again shortly")

// ErrVaultsFailed means the only vaults the user may read failed to open. Speki retries them with backoff,
// so asking again in a few minutes can succeed.
var ErrVaultsFailed = errors.New("speki couldn't open your vaults and keeps retrying; try again in a few minutes")

// errLoading means a resolve found only vaults being built that the user may read: worth waiting for.
var errLoading = errors.New("readable vaults are still being built")

// User is a signed-in user, from their speki token.
type User struct {
	ID     int64
	Login  string
	Token  string // the user's GitHub token
	Client string // the MCP client they signed in with
	Write  bool   // signed in with write access
}

// Grant is what one user may do.
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

// Vault returns a readable vault by name. Names are matched without regard to letter case, as GitHub
// matches repository names.
func (g *Grant) Vault(name string) (vault.Vault, bool) {
	for _, v := range g.readable {
		if asciiEqualFold(v.Name, name) {
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
	TeamMember(ctx context.Context, token, org, slug, login string) (bool, error)
}

// cache holds GitHub's answers, shared by every instance when there are several.
type cache interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

// library is what the resolver needs to know about speki's vaults.
type library interface {
	// Vaults returns every vault by state, each in exactly one list.
	Vaults() (serving, building, failed []vault.Vault)
	// Changed returns a channel closed when a vault is added, changed or removed, or changes state.
	Changed() <-chan struct{}
}

// Resolver turns users into grants.
type Resolver struct {
	github       githubAPI
	library      library
	requiredOrgs []string
	cache        cache // by GitHub token, never by user: tokens can differ in scope or be revoked
	ttl          time.Duration
	wait         time.Duration // how long Resolve waits for a readable vault that's being built
	now          func() time.Time
	flight       singleflight.Group // one lookup per key at a time, shared by its callers
	stopping     chan struct{}      // closed by Stop
	stopOnce     sync.Once
}

// cached is one answer as the cache holds it. Expires is checked as well as the cache's own TTL, so a
// clock that tests move counts.
type cached struct {
	Repos   []gh.RepoAccess `json:"repos,omitempty"`
	Member  bool            `json:"member,omitempty"`
	Expires time.Time       `json:"expires"`
}

// NewResolver returns a resolver. requiredOrgs may be empty.
func NewResolver(github githubAPI, lib library, requiredOrgs []string, c cache) *Resolver {
	return &Resolver{
		github:       github,
		library:      lib,
		requiredOrgs: requiredOrgs,
		cache:        c,
		ttl:          5 * time.Minute,
		wait:         30 * time.Second,
		now:          time.Now,
		stopping:     make(chan struct{}),
	}
}

// Stop ends every wait for vaults being built, now and from now on, with ErrNotReady, so a stopping
// service isn't held up by requests that would wait.
func (r *Resolver) Stop() { r.stopOnce.Do(func() { close(r.stopping) }) }

// Resolve returns what u may do. It errs with ErrUnauthorized, ErrNotMember or ErrNoVaults when u
// doesn't get in, and with ErrVaultsFailed when the only vaults u may read failed to open. When u can read
// no vault being served but one being built, it waits for the library to serve one, and errs with
// ErrNotReady if it doesn't in time, ctx ends or the resolver stops first.
func (r *Resolver) Resolve(ctx context.Context, u User) (*Grant, error) {
	repos, err := r.lookup(ctx, u.Token)
	if err != nil {
		return nil, err
	}
	res := &resolution{r: r, u: u, repos: repos, seen: make(map[int64]readDecision)}
	changed := r.library.Changed() // before resolving, so a change while it runs isn't missed
	g, err := res.grant(ctx)
	if !errors.Is(err, errLoading) {
		return g, err
	}
	timeout := time.NewTimer(r.wait)
	defer timeout.Stop()
	for {
		last := false
		select {
		case <-changed:
		case <-timeout.C:
			last = true // one more look: a vault served just now still counts
		case <-r.stopping:
			return nil, ErrNotReady
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: %w", ErrNotReady, ctx.Err())
		}
		changed = r.library.Changed()
		g, err = res.grant(ctx)
		if !errors.Is(err, errLoading) {
			return g, err
		}
		if last {
			return nil, ErrNotReady
		}
	}
}

// resolution is one Resolve call: the user, what GitHub says they can see, and the read decisions made so
// far, so a waiter that wakes up only looks at vaults that are new or whose read settings changed.
type resolution struct {
	r     *Resolver
	u     User
	repos map[int64]gh.RepoAccess
	seen  map[int64]readDecision
}

// readDecision is whether the user may read a vault with the read settings it was decided under.
type readDecision struct {
	role    gh.Role
	readers []vault.Principal
	ok      bool
}

// grant returns the grant over the vaults being served. With none readable it errs with errLoading,
// ErrVaultsFailed or ErrNoVaults, by what the user may read among the vaults that aren't served.
func (res *resolution) grant(ctx context.Context) (*Grant, error) {
	serving, building, failed := res.r.library.Vaults()
	readable, err := res.readable(ctx, serving)
	if err != nil {
		return nil, err
	}
	if len(readable) == 0 {
		return nil, res.unserved(ctx, building, failed)
	}
	var writable []int64
	for _, v := range readable {
		if res.repos[v.RepoID].Role < gh.RoleWrite || !res.u.Write {
			continue
		}
		writer, err := res.r.listed(ctx, res.u, v.Config.Writers)
		if err != nil {
			return nil, err
		}
		if writer {
			writable = append(writable, v.RepoID)
		}
	}
	return NewGrant(res.u, readable, writable), nil
}

// unserved says why the user gets no vault: errLoading for one being built, ErrVaultsFailed for one that
// failed to open, ErrNoVaults for none. A failed vault isn't waited for: its next retry is minutes away.
func (res *resolution) unserved(ctx context.Context, building, failed []vault.Vault) error {
	readable, err := res.readable(ctx, building)
	if err != nil {
		return err
	}
	if len(readable) > 0 {
		return errLoading
	}
	readable, err = res.readable(ctx, failed)
	if err != nil {
		return err
	}
	if len(readable) > 0 {
		return ErrVaultsFailed
	}
	return ErrNoVaults
}

// readable returns the vaults among vs that the user may read.
func (res *resolution) readable(ctx context.Context, vs []vault.Vault) ([]vault.Vault, error) {
	var out []vault.Vault
	for _, v := range vs {
		ok, err := res.canRead(ctx, v)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, v)
		}
	}
	return out, nil
}

func (res *resolution) canRead(ctx context.Context, v vault.Vault) (bool, error) {
	c := v.Config
	d, ok := res.seen[v.RepoID]
	// nil readers means everyone, an empty list no one: Equal alone doesn't tell them apart.
	if ok && d.role == c.ReadRole && (d.readers == nil) == (c.Readers == nil) && slices.Equal(d.readers, c.Readers) {
		return d.ok, nil
	}
	a, ok := res.repos[v.RepoID]
	can := ok && a.Role >= max(c.ReadRole, gh.RoleRead)
	if can {
		var err error
		can, err = res.r.listed(ctx, res.u, c.Readers)
		if err != nil {
			return false, err
		}
	}
	res.seen[v.RepoID] = readDecision{role: c.ReadRole, readers: c.Readers, ok: can}
	return can, nil
}

// CheckMember reports whether the token's user passes the organisation gate, for sign-in.
func (r *Resolver) CheckMember(ctx context.Context, token string) error {
	_, err := r.lookup(ctx, token)
	return err
}

// lookup returns the repositories the token's user can see, after the organisation gate, from the cache
// when it's fresh.
func (r *Resolver) lookup(ctx context.Context, token string) (map[int64]gh.RepoAccess, error) {
	c, err := r.cached(ctx, "access:repos:"+tokenKey(token), func(ctx context.Context) (cached, error) {
		err := r.checkOrgs(ctx, token)
		if err != nil {
			return cached{}, err
		}
		list, err := r.github.UserRepositories(ctx, token)
		if err != nil {
			return cached{}, githubError(err)
		}
		return cached{Repos: list}, nil
	})
	if err != nil {
		return nil, err
	}
	repos := make(map[int64]gh.RepoAccess, len(c.Repos))
	for _, a := range c.Repos {
		repos[a.RepoID] = a
	}
	return repos, nil
}

// cached returns the answer under key, asking GitHub with ask when the cache has none. Callers asking for
// the same key share one ask. A cache that fails counts as empty: GitHub still answers.
func (r *Resolver) cached(ctx context.Context, key string, ask func(context.Context) (cached, error)) (cached, error) {
	now := r.now()
	raw, ok, err := r.cache.Get(ctx, key)
	if err == nil && ok {
		var c cached
		if json.Unmarshal(raw, &c) == nil && now.Before(c.Expires) {
			return c, nil
		}
	}

	// The ask is the resolver's, not the request's: a caller that leaves mustn't fail it for the others.
	ch := r.flight.DoChan(key, func() (any, error) {
		actx := context.WithoutCancel(ctx)
		c, err := ask(actx)
		if err != nil {
			return cached{}, err
		}
		c.Expires = r.now().Add(r.ttl)
		raw, err := json.Marshal(c)
		if err == nil {
			_ = r.cache.Set(actx, key, raw, r.ttl)
		}
		return c, nil
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return cached{}, res.Err
		}
		return res.Val.(cached), nil
	case <-ctx.Done():
		return cached{}, ctx.Err()
	}
}

// tokenKey names a token in cache keys without revealing it.
func tokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// listed reports whether u is one of ps, directly or through a team. A nil list lists everyone.
func (r *Resolver) listed(ctx context.Context, u User, ps []vault.Principal) (bool, error) {
	if ps == nil {
		return true, nil
	}
	for _, p := range ps {
		if p.Team == "" && asciiEqualFold(p.Login, u.Login) {
			return true, nil
		}
	}
	for _, p := range ps {
		if p.Team == "" {
			continue
		}
		member, err := r.teamMember(ctx, u, p)
		if err != nil || member {
			return member, err
		}
	}
	return false, nil
}

// teamMember asks GitHub whether u is in team, once per token until the cached answer expires.
func (r *Resolver) teamMember(ctx context.Context, u User, team vault.Principal) (bool, error) {
	key := "access:team:" + tokenKey(u.Token) + ":" + strings.ToLower(team.String())
	c, err := r.cached(ctx, key, func(ctx context.Context) (cached, error) {
		member, err := r.github.TeamMember(ctx, u.Token, team.Org, team.Team, u.Login)
		if err != nil {
			return cached{}, githubError(err)
		}
		return cached{Member: member}, nil
	})
	return c.Member, err
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

// asciiEqualFold reports whether a and b are equal, ignoring the case of ASCII letters only: GitHub names
// are ASCII, and a Unicode fold would let a Kelvin sign stand for a k.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
