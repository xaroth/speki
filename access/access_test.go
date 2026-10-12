package access

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/go-cmp/cmp"

	"github.com/eve-online-tools/speki/gh"
	"github.com/eve-online-tools/speki/store"
	"github.com/eve-online-tools/speki/vault"
)

type fakeGitHub struct {
	mu      sync.Mutex
	members map[string][]string // token -> orgs
	repos   map[string][]gh.RepoAccess
	revoked map[string]bool
	teams   map[string][]string // login -> org/slug
	asked   []string            // org/slug, per TeamMember call
	calls   int
}

func (f *fakeGitHub) TeamMember(_ context.Context, token, org, slug, login string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.asked = append(f.asked, org+"/"+slug)
	if f.revoked[token] {
		return false, gh.ErrUnauthorized
	}
	return slices.Contains(f.teams[login], org+"/"+slug), nil
}

func (f *fakeGitHub) ActiveMember(_ context.Context, token, org string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.revoked[token] {
		return false, gh.ErrUnauthorized
	}
	if slices.Contains(f.members[token], org) {
		return true, nil
	}
	return false, nil
}

func (f *fakeGitHub) UserRepositories(_ context.Context, token string) ([]gh.RepoAccess, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.revoked[token] {
		return nil, gh.ErrUnauthorized
	}
	return f.repos[token], nil
}

// fakeLibrary holds vaults in the states tests put them in.
type fakeLibrary struct {
	mu       sync.Mutex
	serving  []vault.Vault
	building []vault.Vault
	failed   []vault.Vault
	changed  chan struct{}
}

func served(vs ...vault.Vault) *fakeLibrary {
	return &fakeLibrary{serving: vs, changed: make(chan struct{})}
}

func (l *fakeLibrary) Vaults() (serving, building, failed []vault.Vault) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.serving), slices.Clone(l.building), slices.Clone(l.failed)
}

func (l *fakeLibrary) Changed() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.changed
}

// set moves the vaults to new states and wakes waiters.
func (l *fakeLibrary) set(serving, building, failed []vault.Vault) {
	l.setQuietly(serving, building, failed)
	l.mu.Lock()
	defer l.mu.Unlock()
	close(l.changed)
	l.changed = make(chan struct{})
}

// setQuietly moves the vaults to new states without waking waiters.
func (l *fakeLibrary) setQuietly(serving, building, failed []vault.Vault) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.serving, l.building, l.failed = serving, building, failed
}

var kb = vault.Vault{RepoID: 100, Name: "ours/kb"}
var infra = vault.Vault{RepoID: 101, Name: "ours/infra"}
var other = vault.Vault{RepoID: 102, Name: "ours/other"}

func setup() (*fakeGitHub, *Resolver) {
	f := &fakeGitHub{
		members: map[string][]string{"alice": {"ours"}, "alice2": {"ours"}, "bob": {"ours"}, "eve": {"elsewhere"}},
		repos: map[string][]gh.RepoAccess{
			"alice":  {{RepoID: 100, Role: gh.RoleWrite}, {RepoID: 101, Role: gh.RoleRead}, {RepoID: 999, Role: gh.RoleRead}},
			"alice2": {{RepoID: 100, Role: gh.RoleWrite}},
			"bob":    {{RepoID: 999, Role: gh.RoleRead}},
			"eve":    {{RepoID: 100, Role: gh.RoleRead}},
		},
		revoked: map[string]bool{},
	}
	r := NewResolver(f, served(kb, infra, other), []string{"ours", "partners"}, store.NewMemory())
	return f, r
}

func TestResolve(t *testing.T) {
	_, r := setup()

	g, err := r.Resolve(t.Context(), User{ID: 1, Login: "alice", Token: "alice", Write: true})
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, v := range g.Readable() {
		names = append(names, v.Name)
	}
	if len(names) != 2 || names[0] != "ours/infra" || names[1] != "ours/kb" {
		t.Errorf("readable = %v", names)
	}
	if !g.CanWrite(100) || g.CanWrite(101) || g.CanWrite(102) {
		t.Error("write access doesn't follow push permission")
	}
	if _, ok := g.Vault("ours/other"); ok {
		t.Error("a vault the user can't read is in the grant")
	}
	if g.User().Token != "alice" || g.ReadOnlySession() {
		t.Errorf("user = %+v", g.User())
	}
}

func TestReadRole(t *testing.T) {
	f := &fakeGitHub{repos: map[string][]gh.RepoAccess{
		"reader":     {{RepoID: 100, Role: gh.RoleRead}, {RepoID: 101, Role: gh.RoleRead}},
		"triager":    {{RepoID: 100, Role: gh.RoleTriage}, {RepoID: 101, Role: gh.RoleTriage}},
		"maintainer": {{RepoID: 100, Role: gh.RoleMaintain}, {RepoID: 101, Role: gh.RoleMaintain}},
	}}
	gated := vault.Vault{RepoID: 100, Name: "ours/gated", Config: vault.Config{ReadRole: gh.RoleTriage}}
	open := vault.Vault{RepoID: 101, Name: "ours/open"}
	r := NewResolver(f, served(gated, open), nil, store.NewMemory())
	tests := []struct {
		token    string
		readable []string
		writable bool // ours/gated
	}{
		{token: "reader", readable: []string{"ours/open"}},
		{token: "triager", readable: []string{"ours/gated", "ours/open"}},
		{token: "maintainer", readable: []string{"ours/gated", "ours/open"}, writable: true},
	}
	for _, tt := range tests {
		t.Run(tt.token, func(t *testing.T) {
			g, err := r.Resolve(t.Context(), User{Token: tt.token, Write: true})
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, v := range g.Readable() {
				names = append(names, v.Name)
			}
			if diff := cmp.Diff(tt.readable, names); diff != "" {
				t.Errorf("readable (-want +got):\n%s", diff)
			}
			if g.CanWrite(100) != tt.writable {
				t.Errorf("CanWrite(ours/gated) = %v", g.CanWrite(100))
			}
		})
	}
}

func TestReadersAndWriters(t *testing.T) {
	rw := []gh.RepoAccess{{RepoID: 100, Role: gh.RoleWrite}, {RepoID: 101, Role: gh.RoleWrite}}
	f := &fakeGitHub{
		repos: map[string][]gh.RepoAccess{
			"alice": rw,
			"carol": rw,
			"dave":  rw,
			"erin":  {{RepoID: 100, Role: gh.RoleRead}, {RepoID: 101, Role: gh.RoleRead}},
			"frank": rw,
		},
		teams: map[string][]string{
			"alice": {"ours/editors"},
			"dave":  {"ours/editors", "ours/leads"},
			"erin":  {"ours/editors", "ours/leads"},
			"frank": {"ours/leads"},
		},
	}
	gated := vault.Vault{RepoID: 100, Name: "ours/gated", Config: vault.Config{
		Readers: []vault.Principal{{Org: "ours", Team: "editors"}, {Login: "Carol"}},
		Writers: []vault.Principal{{Org: "ours", Team: "leads"}},
	}}
	open := vault.Vault{RepoID: 101, Name: "ours/open"}
	r := NewResolver(f, served(gated, open), nil, store.NewMemory())
	tests := []struct {
		login    string
		readable []string
		writable bool // ours/gated
	}{
		{login: "alice", readable: []string{"ours/gated", "ours/open"}},                // reader, not a writer
		{login: "carol", readable: []string{"ours/gated", "ours/open"}},                // listed by login
		{login: "dave", readable: []string{"ours/gated", "ours/open"}, writable: true}, // reader, writer, push
		{login: "erin", readable: []string{"ours/gated", "ours/open"}},                 // writer without push
		{login: "frank", readable: []string{"ours/open"}},                              // writer who isn't a reader
	}
	for _, tt := range tests {
		t.Run(tt.login, func(t *testing.T) {
			g, err := r.Resolve(t.Context(), User{Login: tt.login, Token: tt.login, Write: true})
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, v := range g.Readable() {
				names = append(names, v.Name)
			}
			if diff := cmp.Diff(tt.readable, names); diff != "" {
				t.Errorf("readable (-want +got):\n%s", diff)
			}
			if g.CanWrite(100) != tt.writable {
				t.Errorf("CanWrite(ours/gated) = %v", g.CanWrite(100))
			}
			if g.CanWrite(101) != (tt.login != "erin") {
				t.Errorf("CanWrite(ours/open) = %v", g.CanWrite(101))
			}
		})
	}

	// Team answers are cached with the rest.
	calls := f.calls
	_, err := r.Resolve(t.Context(), User{Login: "dave", Token: "dave", Write: true})
	if err != nil {
		t.Fatal(err)
	}
	if f.calls != calls {
		t.Error("second resolve asked GitHub about teams again")
	}

	// A read-only session doesn't ask about writers.
	f.repos["dave-ro"] = rw
	f.asked = nil
	_, err = r.Resolve(t.Context(), User{Login: "dave", Token: "dave-ro"})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"ours/editors"}, f.asked); diff != "" {
		t.Errorf("teams asked about (-want +got):\n%s", diff)
	}
}

func TestReadOnlySessionNeverWrites(t *testing.T) {
	_, r := setup()
	// A read-write session fills the cache first; the read-only one must not inherit its write access.
	_, err := r.Resolve(t.Context(), User{Token: "alice", Write: true})
	if err != nil {
		t.Fatal(err)
	}
	g, err := r.Resolve(t.Context(), User{Token: "alice", Write: false})
	if err != nil {
		t.Fatal(err)
	}
	if g.CanWrite(100) || !g.ReadOnlySession() {
		t.Error("read-only session can write")
	}
}

func TestTurnedAway(t *testing.T) {
	f, r := setup()
	f.revoked["gone"] = true
	tests := []struct {
		token string
		want  error
	}{
		{"eve", ErrNotMember},
		{"bob", ErrNoVaults},
		{"gone", ErrUnauthorized},
	}
	for _, tt := range tests {
		_, err := r.Resolve(t.Context(), User{Token: tt.token})
		if !errors.Is(err, tt.want) {
			t.Errorf("%s: %v, want %v", tt.token, err, tt.want)
		}
	}
	if err := r.CheckMember(t.Context(), "eve"); !errors.Is(err, ErrNotMember) {
		t.Errorf("CheckMember(eve) = %v", err)
	}
}

func TestUnservedVaultsNoWait(t *testing.T) {
	tests := []struct {
		name     string
		token    string
		serving  []vault.Vault
		building []vault.Vault
		failed   []vault.Vault
		readable []string
		err      error
	}{
		{name: "one served, one building", token: "alice", serving: []vault.Vault{kb}, building: []vault.Vault{infra}, readable: []string{"ours/kb"}},
		{name: "only others' vaults building", token: "bob", building: []vault.Vault{kb, infra}, err: ErrNoVaults},
		{name: "own vault failed, others' building", token: "alice", building: []vault.Vault{other}, failed: []vault.Vault{kb}, err: ErrVaultsFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f, _ := setup()
				lib := served(tt.serving...)
				lib.building, lib.failed = tt.building, tt.failed
				r := NewResolver(f, lib, nil, store.NewMemory())
				start := time.Now()
				g, err := r.Resolve(t.Context(), User{Token: tt.token})
				if waited := time.Since(start); waited != 0 {
					t.Errorf("waited %v", waited)
				}
				if !errors.Is(err, tt.err) {
					t.Fatalf("err = %v, want %v", err, tt.err)
				}
				if err != nil {
					return
				}
				var names []string
				for _, v := range g.Readable() {
					names = append(names, v.Name)
				}
				if diff := cmp.Diff(tt.readable, names); diff != "" {
					t.Errorf("readable (-want +got):\n%s", diff)
				}
			})
		})
	}
}

type resolved struct {
	grant *Grant
	err   error
}

// resolveLater resolves u in a goroutine, for tests that move vaults meanwhile.
func resolveLater(ctx context.Context, r *Resolver, u User) <-chan resolved {
	done := make(chan resolved, 1)
	go func() {
		g, err := r.Resolve(ctx, u)
		done <- resolved{g, err}
	}()
	return done
}

func TestWaitForBuildingVault(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, _ := setup()
		lib := served()
		lib.building = []vault.Vault{kb, infra}
		r := NewResolver(f, lib, nil, store.NewMemory())
		start := time.Now()
		done := resolveLater(t.Context(), r, User{Token: "alice"})
		synctest.Wait()
		f.mu.Lock()
		calls := f.calls
		f.mu.Unlock()

		lib.set(nil, []vault.Vault{kb, infra}, []vault.Vault{other}) // a change that serves nothing
		synctest.Wait()
		select {
		case res := <-done:
			t.Fatalf("stopped waiting before a vault was served: %v", res.err)
		default:
		}

		lib.set([]vault.Vault{kb}, []vault.Vault{infra}, nil)
		res := <-done
		if res.err != nil {
			t.Fatal(res.err)
		}
		if _, ok := res.grant.Vault("ours/kb"); !ok || len(res.grant.Readable()) != 1 {
			t.Errorf("readable = %v", res.grant.Readable())
		}
		if waited := time.Since(start); waited != 0 {
			t.Errorf("waited %v after the vault was served", waited)
		}
		if f.calls != calls {
			t.Error("waiting asked GitHub again")
		}
	})
}

func TestNotReadyAfterWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, _ := setup()
		lib := served()
		lib.building = []vault.Vault{kb}
		r := NewResolver(f, lib, nil, store.NewMemory())
		r.wait = time.Second
		start := time.Now()
		_, err := r.Resolve(t.Context(), User{Token: "alice"})
		if !errors.Is(err, ErrNotReady) {
			t.Errorf("err = %v, want ErrNotReady", err)
		}
		if waited := time.Since(start); waited != r.wait {
			t.Errorf("waited %v, want %v", waited, r.wait)
		}
	})
}

func TestWaitEndsWithContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, _ := setup()
		lib := served()
		lib.building = []vault.Vault{kb}
		r := NewResolver(f, lib, nil, store.NewMemory())
		ctx, cancel := context.WithCancel(t.Context())
		start := time.Now()
		done := resolveLater(ctx, r, User{Token: "alice"})
		synctest.Wait()
		cancel()
		res := <-done
		if !errors.Is(res.err, ErrNotReady) || !errors.Is(res.err, context.Canceled) {
			t.Errorf("err = %v, want ErrNotReady and context.Canceled", res.err)
		}
		if waited := time.Since(start); waited != 0 {
			t.Errorf("waited %v after ctx ended", waited)
		}
	})
}

func TestWaitEndsWhenBuildingStops(t *testing.T) {
	tests := []struct {
		name   string
		failed []vault.Vault
		err    error
	}{
		{name: "vault failed", failed: []vault.Vault{kb}, err: ErrVaultsFailed},
		{name: "vault removed", err: ErrNoVaults},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f, _ := setup()
				lib := served()
				lib.building = []vault.Vault{kb}
				r := NewResolver(f, lib, nil, store.NewMemory())
				start := time.Now()
				done := resolveLater(t.Context(), r, User{Token: "alice"})
				synctest.Wait()
				lib.set(nil, nil, tt.failed)
				res := <-done
				if !errors.Is(res.err, tt.err) {
					t.Errorf("err = %v, want %v", res.err, tt.err)
				}
				if waited := time.Since(start); waited != 0 {
					t.Errorf("waited %v", waited)
				}
			})
		})
	}
}

func TestLastLookWhenWaitEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, _ := setup()
		lib := served()
		lib.building = []vault.Vault{kb}
		r := NewResolver(f, lib, nil, store.NewMemory())
		done := resolveLater(t.Context(), r, User{Token: "alice"})
		synctest.Wait()
		lib.setQuietly([]vault.Vault{kb}, nil, nil) // served, but the waiter isn't woken
		res := <-done
		if res.err != nil {
			t.Fatalf("a vault served by the end of the wait gave %v", res.err)
		}
	})
}

func TestStopEndsWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, _ := setup()
		lib := served()
		lib.building = []vault.Vault{kb}
		r := NewResolver(f, lib, nil, store.NewMemory())
		start := time.Now()
		done := resolveLater(t.Context(), r, User{Token: "alice"})
		synctest.Wait()
		r.Stop()
		if res := <-done; !errors.Is(res.err, ErrNotReady) {
			t.Errorf("waiting: err = %v, want ErrNotReady", res.err)
		}
		_, err := r.Resolve(t.Context(), User{Token: "alice"})
		if !errors.Is(err, ErrNotReady) {
			t.Errorf("after Stop: err = %v, want ErrNotReady", err)
		}
		if waited := time.Since(start); waited != 0 {
			t.Errorf("waited %v", waited)
		}
	})
}

// countingCache counts reads, which go to Redis when there are several instances.
type countingCache struct {
	*store.Memory
	mu   sync.Mutex
	gets int
}

func (c *countingCache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	c.mu.Lock()
	c.gets++
	c.mu.Unlock()
	return c.Memory.Get(ctx, key)
}

func (c *countingCache) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gets
}

func TestWakingDoesNotCheckReadersAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, _ := setup()
		f.teams = map[string][]string{"alice": {"ours/editors"}}
		gated := kb
		gated.Config.Readers = []vault.Principal{{Org: "ours", Team: "editors"}}
		lib := served()
		lib.building = []vault.Vault{gated}
		c := &countingCache{Memory: store.NewMemory()}
		r := NewResolver(f, lib, nil, c)
		done := resolveLater(t.Context(), r, User{Login: "alice", Token: "alice"})
		synctest.Wait()
		gets := c.count()
		for range 3 {
			lib.set(nil, []vault.Vault{gated}, nil)
			synctest.Wait()
		}
		if c.count() != gets {
			t.Errorf("waking read the cache %d more times", c.count()-gets)
		}
		lib.set([]vault.Vault{gated}, nil, nil)
		if res := <-done; res.err != nil {
			t.Fatal(res.err)
		}
	})
}

func TestCachePerToken(t *testing.T) {
	f, r := setup()
	now := time.Now()
	r.now = func() time.Time { return now }
	ctx := t.Context()

	_, _ = r.Resolve(ctx, User{Token: "alice"})
	calls := f.calls
	_, _ = r.Resolve(ctx, User{Token: "alice"})
	if f.calls != calls {
		t.Error("second resolve within five minutes asked GitHub")
	}

	// Another token of the same user is checked on its own: a revoked one stays revoked.
	f.revoked["alice2"] = true
	_, err := r.Resolve(ctx, User{ID: 1, Token: "alice2"})
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("revoked second token: %v", err)
	}

	// After five minutes GitHub is asked again, and a removed permission takes effect.
	f.repos["alice"] = nil
	now = now.Add(5*time.Minute + time.Second)
	_, err = r.Resolve(ctx, User{Token: "alice"})
	if !errors.Is(err, ErrNoVaults) {
		t.Errorf("after expiry: %v", err)
	}
}

func TestNoRequiredOrgs(t *testing.T) {
	f, _ := setup()
	r := NewResolver(f, served(kb), nil, store.NewMemory())
	_, err := r.Resolve(t.Context(), User{Token: "eve"})
	if err != nil {
		t.Errorf("without required orgs: %v", err)
	}
}

func TestVaultNamesFoldASCIIOnly(t *testing.T) {
	g := NewGrant(User{}, []vault.Vault{{RepoID: 1, Name: "ours/kb"}}, nil)
	if v, ok := g.Vault("Ours/KB"); !ok || v.Name != "ours/kb" {
		t.Errorf("Ours/KB = %+v, %v", v, ok)
	}
	if _, ok := g.Vault("ours/\u212ab"); ok { // a Kelvin sign
		t.Error("a Kelvin sign matched a k")
	}
}

func TestCacheShared(t *testing.T) {
	mr := miniredis.RunT(t)
	shared, err := store.NewRedis(t.Context(), "redis://"+mr.Addr(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shared.Close() })
	f, _ := setup()
	vaults := served(kb)
	one := NewResolver(f, vaults, []string{"ours"}, shared)
	two := NewResolver(f, vaults, []string{"ours"}, shared)

	_, err = one.Resolve(t.Context(), User{Token: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	calls := f.calls
	g, err := two.Resolve(t.Context(), User{Token: "alice", Write: true})
	if err != nil {
		t.Fatal(err)
	}
	if f.calls != calls {
		t.Error("second instance asked GitHub again")
	}
	if !g.CanWrite(kb.RepoID) {
		t.Error("cached role lost write access")
	}
	for _, k := range mr.Keys() {
		if strings.Contains(k, "alice") {
			t.Errorf("cache key %q contains the token", k)
		}
	}
}

type failingCache struct{}

func (failingCache) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, errors.New("down")
}

func (failingCache) Set(context.Context, string, []byte, time.Duration) error {
	return errors.New("down")
}

func TestCacheDownAsksGitHub(t *testing.T) {
	f, _ := setup()
	r := NewResolver(f, served(kb), nil, failingCache{})
	_, err := r.Resolve(t.Context(), User{Token: "alice"})
	if err != nil {
		t.Errorf("with the cache down: %v", err)
	}
}

// blockingGitHub holds UserRepositories until release is closed.
type blockingGitHub struct {
	*fakeGitHub
	release chan struct{}
}

func (b blockingGitHub) UserRepositories(ctx context.Context, token string) ([]gh.RepoAccess, error) {
	<-b.release
	return b.fakeGitHub.UserRepositories(ctx, token)
}

func TestConcurrentMissesShareOneLookup(t *testing.T) {
	f, _ := setup()
	b := blockingGitHub{fakeGitHub: f, release: make(chan struct{})}
	r := NewResolver(b, served(kb), nil, store.NewMemory())

	// A caller that leaves doesn't fail the lookup for the others.
	gone, cancel := context.WithCancel(t.Context())
	left := make(chan error)
	go func() {
		_, err := r.Resolve(gone, User{Token: "alice"})
		left <- err
	}()
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 10 {
		wg.Go(func() {
			_, err := r.Resolve(t.Context(), User{Token: "alice"})
			errs <- err
		})
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-left; !errors.Is(err, context.Canceled) {
		t.Errorf("caller that left: %v", err)
	}
	close(b.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if f.calls != 1 {
		t.Errorf("GitHub asked %d times, want 1", f.calls)
	}
}
