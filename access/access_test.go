package access

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/eve-online-tools/speki/gh"
	"github.com/eve-online-tools/speki/vault"
)

type fakeGitHub struct {
	mu      sync.Mutex
	members map[string][]string // token -> orgs
	repos   map[string][]gh.RepoAccess
	revoked map[string]bool
	calls   int
}

func (f *fakeGitHub) ActiveMember(_ context.Context, token, org string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.revoked[token] {
		return false, gh.ErrUnauthorized
	}
	for _, o := range f.members[token] {
		if o == org {
			return true, nil
		}
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

var (
	kb    = vault.Vault{RepoID: 100, Name: "ours/kb"}
	infra = vault.Vault{RepoID: 101, Name: "ours/infra"}
	other = vault.Vault{RepoID: 102, Name: "ours/other"}
)

func setup() (*fakeGitHub, *Resolver) {
	f := &fakeGitHub{
		members: map[string][]string{"alice": {"ours"}, "alice2": {"ours"}, "bob": {"ours"}, "eve": {"elsewhere"}},
		repos: map[string][]gh.RepoAccess{
			"alice":  {{RepoID: 100, Pull: true, Push: true}, {RepoID: 101, Pull: true}, {RepoID: 999, Pull: true}},
			"alice2": {{RepoID: 100, Pull: true, Push: true}},
			"bob":    {{RepoID: 999, Pull: true}},
			"eve":    {{RepoID: 100, Pull: true}},
		},
		revoked: map[string]bool{},
	}
	r := NewResolver(f, func() []vault.Vault { return []vault.Vault{kb, infra, other} }, []string{"ours", "partners"})
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
		t.Error("second resolve within a minute asked GitHub")
	}

	// Another token of the same user is checked on its own: a revoked one stays revoked.
	f.revoked["alice2"] = true
	_, err := r.Resolve(ctx, User{ID: 1, Token: "alice2"})
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("revoked second token: %v", err)
	}

	// After a minute GitHub is asked again, and a removed permission takes effect.
	f.repos["alice"] = nil
	now = now.Add(61 * time.Second)
	_, err = r.Resolve(ctx, User{Token: "alice"})
	if !errors.Is(err, ErrNoVaults) {
		t.Errorf("after expiry: %v", err)
	}
}

func TestNoRequiredOrgs(t *testing.T) {
	f, _ := setup()
	r := NewResolver(f, func() []vault.Vault { return []vault.Vault{kb} }, nil)
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
