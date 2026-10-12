package access

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
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
	r := NewResolver(f, func() []vault.Vault { return []vault.Vault{kb, infra, other} }, []string{"ours", "partners"}, store.NewMemory())
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
	r := NewResolver(f, func() []vault.Vault { return []vault.Vault{gated, open} }, nil, store.NewMemory())
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
	r := NewResolver(f, func() []vault.Vault { return []vault.Vault{gated, open} }, nil, store.NewMemory())
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
	r := NewResolver(f, func() []vault.Vault { return []vault.Vault{kb} }, nil, store.NewMemory())
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
	vaults := func() []vault.Vault { return []vault.Vault{kb} }
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
	r := NewResolver(f, func() []vault.Vault { return []vault.Vault{kb} }, nil, failingCache{})
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
	r := NewResolver(b, func() []vault.Vault { return []vault.Vault{kb} }, nil, store.NewMemory())

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
