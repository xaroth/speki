package access_test

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

	"github.com/xaroth/speki/auth/access"
	"github.com/xaroth/speki/gh"
	"github.com/xaroth/speki/store"
	"github.com/xaroth/speki/vault"
)

// fakeGitHub answers the GitHub calls the resolver makes, from maps keyed by token or login.
type fakeGitHub struct {
	mu      sync.Mutex
	members map[string][]string // token -> orgs
	repos   map[string][]gh.RepoAccess
	revoked map[string]bool
	teams   map[string][]string // login -> org/slug
	asked   []string            // org/slug, per TeamMember call
	teamErr error               // returned by TeamMember when set
	calls   int
}

func (f *fakeGitHub) TeamMember(_ context.Context, token, org, slug, login string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.asked = append(f.asked, org+"/"+slug)
	if f.teamErr != nil {
		return false, f.teamErr
	}
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
	return slices.Contains(f.members[token], org), nil
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

func (f *fakeGitHub) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeLibrary holds vaults in the states tests put them in.
type fakeLibrary struct {
	mu       sync.Mutex
	serving  []vault.Vault
	building []vault.Vault
	failed   []vault.Vault
	changed  chan struct{}
}

func newLibrary(serving, building, failed []vault.Vault) *fakeLibrary {
	return &fakeLibrary{serving: serving, building: building, failed: failed, changed: make(chan struct{})}
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

var (
	kb    = vault.Vault{RepoID: 100, Name: "ours/kb"}
	infra = vault.Vault{RepoID: 101, Name: "ours/infra"}
	other = vault.Vault{RepoID: 102, Name: "ours/other"}
	docs  = vault.Vault{RepoID: 999, Name: "ours/docs"}
)

// configured returns v with c as its speki.yaml settings.
func configured(v vault.Vault, c vault.Config) vault.Vault {
	v.Config = c
	return v
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// newGitHub returns a fake where alice reads kb, infra and docs and pushes to kb, alice2 pushes to kb, bob
// reads docs, and eve reads kb but is in no required organisation.
func newGitHub(tb testing.TB) *fakeGitHub {
	tb.Helper()
	return &fakeGitHub{
		members: map[string][]string{"alice": {"ours"}, "alice2": {"ours"}, "bob": {"ours"}, "eve": {"elsewhere"}},
		repos: map[string][]gh.RepoAccess{
			"alice":  {{RepoID: 100, Role: gh.RoleWrite}, {RepoID: 101, Role: gh.RoleRead}, {RepoID: 999, Role: gh.RoleRead}},
			"alice2": {{RepoID: 100, Role: gh.RoleWrite}},
			"bob":    {{RepoID: 999, Role: gh.RoleRead}},
			"eve":    {{RepoID: 100, Role: gh.RoleRead}},
		},
		revoked: map[string]bool{},
	}
}

// setup returns the fake GitHub and a resolver serving kb, infra and other, requiring ours or partners.
func setup(tb testing.TB) (*fakeGitHub, *access.Resolver) {
	tb.Helper()
	f := newGitHub(tb)
	r := access.NewResolver(f, newLibrary([]vault.Vault{kb, infra, other}, nil, nil), []string{"ours", "partners"}, store.NewMemory(), discard())
	return f, r
}

func names(vs []vault.Vault) []string {
	var out []string
	for _, v := range vs {
		out = append(out, v.Name)
	}
	return out
}

func TestResolve(t *testing.T) {
	t.Parallel()
	_, r := setup(t)
	u := access.User{ID: 1, Login: "alice", Token: "alice", Write: true}

	g, err := r.Resolve(t.Context(), u)
	if err != nil {
		t.Fatalf("got err %v, want nil", err)
	}
	if diff := cmp.Diff([]string{"ours/infra", "ours/kb"}, names(g.Readable())); diff != "" {
		t.Errorf("readable (-want +got):\n%s", diff)
	}
	for _, id := range []int64{100, 101, 102} {
		if got, want := g.CanWrite(id), id == 100; got != want {
			t.Errorf("CanWrite(%d): got %v, want %v", id, got, want)
		}
	}
	if diff := cmp.Diff(u, g.User()); diff != "" {
		t.Errorf("user (-want +got):\n%s", diff)
	}
	if g.ReadOnlySession() {
		t.Errorf("ReadOnlySession: got true, want false")
	}
	if len(g.Unavailable()) != 0 {
		t.Errorf("unavailable: got %v, want none", g.Unavailable())
	}
}

func TestResolveTurnedAway(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name  string
		token string
		orgs  []string
		want  error
	}{
		{name: "not a member", token: "eve", orgs: []string{"ours", "partners"}, want: access.ErrNotMember},
		{name: "no required orgs", token: "eve", want: nil},
		{name: "no readable vault", token: "bob", orgs: []string{"ours"}, want: access.ErrNoVaults},
		{name: "revoked token", token: "gone", orgs: []string{"ours"}, want: access.ErrUnauthorized},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			f := newGitHub(t)
			f.revoked["gone"] = true
			r := access.NewResolver(f, newLibrary([]vault.Vault{kb}, nil, nil), testCase.orgs, store.NewMemory(), discard())
			_, err := r.Resolve(t.Context(), access.User{Token: testCase.token})
			if !errors.Is(err, testCase.want) {
				t.Errorf("got err %v, want %v", err, testCase.want)
			}
		})
	}
}

func TestCheckMember(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name  string
		token string
		want  error
	}{
		{name: "member", token: "alice"},
		{name: "not a member", token: "eve", want: access.ErrNotMember},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, r := setup(t)
			if err := r.CheckMember(t.Context(), testCase.token); !errors.Is(err, testCase.want) {
				t.Errorf("got err %v, want %v", err, testCase.want)
			}
		})
	}
}

func TestReadRole(t *testing.T) {
	t.Parallel()
	f := &fakeGitHub{repos: map[string][]gh.RepoAccess{
		"reader":     {{RepoID: 100, Role: gh.RoleRead}, {RepoID: 101, Role: gh.RoleRead}},
		"triager":    {{RepoID: 100, Role: gh.RoleTriage}, {RepoID: 101, Role: gh.RoleTriage}},
		"maintainer": {{RepoID: 100, Role: gh.RoleMaintain}, {RepoID: 101, Role: gh.RoleMaintain}},
	}}
	gated := vault.Vault{RepoID: 100, Name: "ours/gated", Config: vault.Config{ReadRole: gh.RoleTriage}}
	open := vault.Vault{RepoID: 101, Name: "ours/open"}
	r := access.NewResolver(f, newLibrary([]vault.Vault{gated, open}, nil, nil), nil, store.NewMemory(), discard())
	testCases := []struct {
		name     string
		readable []string
		writable bool // ours/gated
	}{
		{name: "reader", readable: []string{"ours/open"}},
		{name: "triager", readable: []string{"ours/gated", "ours/open"}},
		{name: "maintainer", readable: []string{"ours/gated", "ours/open"}, writable: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			g, err := r.Resolve(t.Context(), access.User{Token: testCase.name, Write: true})
			if err != nil {
				t.Fatalf("got err %v, want nil", err)
			}
			if diff := cmp.Diff(testCase.readable, names(g.Readable())); diff != "" {
				t.Errorf("readable (-want +got):\n%s", diff)
			}
			if got := g.CanWrite(100); got != testCase.writable {
				t.Errorf("CanWrite(ours/gated): got %v, want %v", got, testCase.writable)
			}
		})
	}
}

func TestReadersAndWriters(t *testing.T) {
	t.Parallel()
	rw := []gh.RepoAccess{{RepoID: 100, Role: gh.RoleWrite}, {RepoID: 101, Role: gh.RoleWrite}}
	gated := vault.Vault{RepoID: 100, Name: "ours/gated", Config: vault.Config{
		Readers: []vault.Principal{{Org: "ours", Team: "editors"}, {Login: "Carol"}},
		Writers: []vault.Principal{{Org: "ours", Team: "leads"}},
	}}
	open := vault.Vault{RepoID: 101, Name: "ours/open"}
	testCases := []struct {
		name      string
		login     string
		repos     []gh.RepoAccess
		teams     []string
		readOnly  bool
		readable  []string
		writeGate bool // ours/gated
		writeOpen bool // ours/open
		asked     []string
	}{
		{
			name: "reader, not a writer", login: "alice", repos: rw, teams: []string{"ours/editors"},
			readable: []string{"ours/gated", "ours/open"}, writeOpen: true, asked: []string{"ours/editors", "ours/leads"},
		},
		{
			name: "listed by login", login: "carol", repos: rw,
			readable: []string{"ours/gated", "ours/open"}, writeOpen: true, asked: []string{"ours/leads"},
		},
		{
			name: "reader, writer, push", login: "dave", repos: rw, teams: []string{"ours/editors", "ours/leads"},
			readable: []string{"ours/gated", "ours/open"}, writeGate: true, writeOpen: true, asked: []string{"ours/editors", "ours/leads"},
		},
		{
			name: "writer without push", login: "erin", teams: []string{"ours/editors", "ours/leads"},
			repos:    []gh.RepoAccess{{RepoID: 100, Role: gh.RoleRead}, {RepoID: 101, Role: gh.RoleRead}},
			readable: []string{"ours/gated", "ours/open"}, asked: []string{"ours/editors"},
		},
		{
			name: "writer who isn't a reader", login: "frank", repos: rw, teams: []string{"ours/leads"},
			readable: []string{"ours/open"}, writeOpen: true, asked: []string{"ours/editors"},
		},
		{
			name: "read-only session doesn't ask about writers", login: "dave", repos: rw, teams: []string{"ours/editors", "ours/leads"},
			readOnly: true, readable: []string{"ours/gated", "ours/open"}, asked: []string{"ours/editors"},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeGitHub{
				repos: map[string][]gh.RepoAccess{testCase.login: testCase.repos},
				teams: map[string][]string{testCase.login: testCase.teams},
			}
			r := access.NewResolver(f, newLibrary([]vault.Vault{gated, open}, nil, nil), nil, store.NewMemory(), discard())
			g, err := r.Resolve(t.Context(), access.User{Login: testCase.login, Token: testCase.login, Write: !testCase.readOnly})
			if err != nil {
				t.Fatalf("got err %v, want nil", err)
			}
			if diff := cmp.Diff(testCase.readable, names(g.Readable())); diff != "" {
				t.Errorf("readable (-want +got):\n%s", diff)
			}
			if got := g.CanWrite(100); got != testCase.writeGate {
				t.Errorf("CanWrite(ours/gated): got %v, want %v", got, testCase.writeGate)
			}
			if got := g.CanWrite(101); got != testCase.writeOpen {
				t.Errorf("CanWrite(ours/open): got %v, want %v", got, testCase.writeOpen)
			}
			if diff := cmp.Diff(testCase.asked, f.asked); diff != "" {
				t.Errorf("teams asked about (-want +got):\n%s", diff)
			}
		})
	}
}

func TestReadOnlySessionNeverWrites(t *testing.T) {
	t.Parallel()
	_, r := setup(t)
	// A read-write session fills the cache first; the read-only one must not inherit its write access.
	if _, err := r.Resolve(t.Context(), access.User{Token: "alice", Write: true}); err != nil {
		t.Fatalf("got err %v, want nil", err)
	}
	g, err := r.Resolve(t.Context(), access.User{Token: "alice"})
	if err != nil {
		t.Fatalf("got err %v, want nil", err)
	}
	if g.CanWrite(100) {
		t.Errorf("CanWrite(ours/kb): got true, want false")
	}
	if !g.ReadOnlySession() {
		t.Errorf("ReadOnlySession: got false, want true")
	}
}

func TestUnservedVaultsNoWait(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name        string
		token       string
		serving     []vault.Vault
		building    []vault.Vault
		failed      []vault.Vault
		readable    []string
		unavailable []string // name:state
		err         error
	}{
		{
			name:        "served, building and failed",
			token:       "alice",
			serving:     []vault.Vault{kb},
			building:    []vault.Vault{infra, other},
			failed:      []vault.Vault{docs},
			readable:    []string{"ours/kb"},
			unavailable: []string{"ours/docs:failed", "ours/infra:building"},
		},
		{
			name:     "others' vaults building",
			token:    "bob",
			serving:  []vault.Vault{docs},
			building: []vault.Vault{kb, infra},
			failed:   []vault.Vault{other},
			readable: []string{"ours/docs"},
		},
		{
			name:     "read role and readers exclude",
			token:    "alice",
			serving:  []vault.Vault{kb},
			building: []vault.Vault{configured(infra, vault.Config{ReadRole: gh.RoleWrite})},
			failed:   []vault.Vault{configured(docs, vault.Config{Readers: []vault.Principal{{Login: "bob"}}})},
			readable: []string{"ours/kb"},
		},
		{
			name:     "team readers exclude",
			token:    "alice",
			serving:  []vault.Vault{kb},
			building: []vault.Vault{configured(docs, vault.Config{Readers: []vault.Principal{{Org: "ours", Team: "admins"}}})},
			readable: []string{"ours/kb"},
		},
		{
			name:        "team readers include",
			token:       "alice",
			serving:     []vault.Vault{kb},
			building:    []vault.Vault{configured(docs, vault.Config{Readers: []vault.Principal{{Org: "ours", Team: "editors"}}})},
			readable:    []string{"ours/kb"},
			unavailable: []string{"ours/docs:building"},
		},
		{
			name:     "only others' vaults building",
			token:    "bob",
			building: []vault.Vault{kb, infra},
			err:      access.ErrNoVaults,
		},
		{
			name:     "own vault failed, others' building",
			token:    "alice",
			building: []vault.Vault{other},
			failed:   []vault.Vault{kb},
			err:      access.ErrVaultsFailed,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := newGitHub(t)
				f.teams = map[string][]string{"alice": {"ours/editors"}}
				lib := newLibrary(testCase.serving, testCase.building, testCase.failed)
				r := access.NewResolver(f, lib, nil, store.NewMemory(), discard())
				start := time.Now()
				g, err := r.Resolve(t.Context(), access.User{Login: testCase.token, Token: testCase.token})
				if waited := time.Since(start); waited != 0 {
					t.Errorf("waited: got %v, want 0", waited)
				}
				if !errors.Is(err, testCase.err) {
					t.Fatalf("got err %v, want %v", err, testCase.err)
				}
				if err != nil {
					return
				}
				if diff := cmp.Diff(testCase.readable, names(g.Readable())); diff != "" {
					t.Errorf("readable (-want +got):\n%s", diff)
				}
				var unavailable []string
				for _, u := range g.Unavailable() {
					unavailable = append(unavailable, u.Vault.Name+":"+string(u.State))
					if _, ok := g.Vault(u.Vault.Name); ok {
						t.Errorf("%s is both readable and unavailable", u.Vault.Name)
					}
				}
				if diff := cmp.Diff(testCase.unavailable, unavailable); diff != "" {
					t.Errorf("unavailable (-want +got):\n%s", diff)
				}
				for _, v := range append(testCase.building, testCase.failed...) {
					_, got := g.UnavailableVault(strings.ToUpper(v.Name))
					want := slices.Contains(unavailable, v.Name+":building") || slices.Contains(unavailable, v.Name+":failed")
					if got != want {
						t.Errorf("UnavailableVault(%s): got %v, want %v", v.Name, got, want)
					}
				}
			})
		})
	}
}

// A failed read check on a vault that isn't served leaves that vault out, rather than turning away a user
// who has served vaults. Without served vaults, the error stands.
func TestUnavailableCheckFails(t *testing.T) {
	t.Parallel()
	f := newGitHub(t)
	f.teamErr = errors.New("GitHub is down")
	gated := configured(docs, vault.Config{Readers: []vault.Principal{{Org: "ours", Team: "editors"}}})
	lib := newLibrary([]vault.Vault{kb}, []vault.Vault{gated}, nil)
	var logs strings.Builder
	r := access.NewResolver(f, lib, nil, store.NewMemory(), slog.New(slog.NewTextHandler(&logs, nil)))
	u := access.User{Login: "alice", Token: "alice"}

	g, err := r.Resolve(t.Context(), u)
	if err != nil {
		t.Fatalf("got err %v, want nil", err)
	}
	if diff := cmp.Diff([]string{"ours/kb"}, names(g.Readable())); diff != "" {
		t.Errorf("readable (-want +got):\n%s", diff)
	}
	if len(g.Unavailable()) != 0 {
		t.Errorf("unavailable: got %v, want none", g.Unavailable())
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "ours/docs") {
		t.Errorf("logs: got %q, want a warning naming ours/docs", logs.String())
	}

	lib.setQuietly(nil, []vault.Vault{gated}, nil)
	if _, err := r.Resolve(t.Context(), u); !errors.Is(err, f.teamErr) {
		t.Errorf("without served vaults: got err %v, want %v", err, f.teamErr)
	}

	f.mu.Lock()
	f.teamErr = nil
	f.revoked["alice"] = true
	f.mu.Unlock()
	lib.setQuietly([]vault.Vault{kb}, []vault.Vault{gated}, nil) // repositories still cached: only the team check sees it
	if _, err := r.Resolve(t.Context(), u); !errors.Is(err, access.ErrUnauthorized) {
		t.Errorf("revoked token: got err %v, want %v", err, access.ErrUnauthorized)
	}
}

func TestUnavailableTeamChecksCached(t *testing.T) {
	t.Parallel()
	f := newGitHub(t)
	f.teams = map[string][]string{"alice": {"ours/editors"}}
	gated := configured(docs, vault.Config{Readers: []vault.Principal{{Org: "ours", Team: "editors"}}})
	r := access.NewResolver(f, newLibrary([]vault.Vault{kb}, []vault.Vault{gated}, nil), nil, store.NewMemory(), discard())
	for range 2 {
		g, err := r.Resolve(t.Context(), access.User{Login: "alice", Token: "alice"})
		if err != nil {
			t.Fatalf("got err %v, want nil", err)
		}
		if len(g.Unavailable()) != 1 {
			t.Errorf("unavailable: got %v, want ours/docs", g.Unavailable())
		}
	}
	if diff := cmp.Diff([]string{"ours/editors"}, f.asked); diff != "" {
		t.Errorf("teams asked about (-want +got):\n%s", diff)
	}
}

type resolved struct {
	grant *access.Grant
	err   error
}

// resolveLater resolves u in a goroutine, for tests that move vaults meanwhile.
func resolveLater(ctx context.Context, r *access.Resolver, u access.User) <-chan resolved {
	done := make(chan resolved, 1)
	go func() {
		g, err := r.Resolve(ctx, u)
		done <- resolved{g, err}
	}()
	return done
}

func TestWaitForBuildingVault(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newGitHub(t)
		lib := newLibrary(nil, []vault.Vault{kb, infra}, nil)
		r := access.NewResolver(f, lib, nil, store.NewMemory(), discard())
		start := time.Now()
		done := resolveLater(t.Context(), r, access.User{Token: "alice"})
		synctest.Wait()
		calls := f.callCount()

		lib.set(nil, []vault.Vault{kb, infra}, []vault.Vault{other}) // a change that serves nothing
		synctest.Wait()
		select {
		case res := <-done:
			t.Fatalf("stopped waiting before a vault was served: got err %v", res.err)
		default:
		}

		lib.set([]vault.Vault{kb}, []vault.Vault{infra}, nil)
		res := <-done
		if res.err != nil {
			t.Fatalf("got err %v, want nil", res.err)
		}
		if diff := cmp.Diff([]string{"ours/kb"}, names(res.grant.Readable())); diff != "" {
			t.Errorf("readable (-want +got):\n%s", diff)
		}
		if waited := time.Since(start); waited != 0 {
			t.Errorf("waited after the vault was served: got %v, want 0", waited)
		}
		if got := f.callCount(); got != calls {
			t.Errorf("GitHub calls while waiting: got %d, want 0", got-calls)
		}
	})
}

// The resolver waits 30 seconds by default for a vault being built.
func TestWaitEnds(t *testing.T) {
	t.Parallel()
	const fullWait = 30 * time.Second
	testCases := []struct {
		name      string
		stopFirst bool
		act       func(lib *fakeLibrary, r *access.Resolver, cancel context.CancelFunc) // nil lets the wait run out
		err       []error                                                               // nil means no error
		waited    time.Duration
	}{
		{name: "nothing served in time", err: []error{access.ErrNotReady}, waited: fullWait},
		{
			name: "served without a wake-up gets a last look",
			act: func(lib *fakeLibrary, _ *access.Resolver, _ context.CancelFunc) {
				lib.setQuietly([]vault.Vault{kb}, nil, nil)
			},
			waited: fullWait,
		},
		{
			name: "vault failed",
			act:  func(lib *fakeLibrary, _ *access.Resolver, _ context.CancelFunc) { lib.set(nil, nil, []vault.Vault{kb}) },
			err:  []error{access.ErrVaultsFailed},
		},
		{
			name: "vault removed",
			act:  func(lib *fakeLibrary, _ *access.Resolver, _ context.CancelFunc) { lib.set(nil, nil, nil) },
			err:  []error{access.ErrNoVaults},
		},
		{
			name: "context ends",
			act:  func(_ *fakeLibrary, _ *access.Resolver, cancel context.CancelFunc) { cancel() },
			err:  []error{access.ErrNotReady, context.Canceled},
		},
		{
			name: "resolver stops",
			act:  func(_ *fakeLibrary, r *access.Resolver, _ context.CancelFunc) { r.Stop() },
			err:  []error{access.ErrNotReady},
		},
		{name: "resolver already stopped", stopFirst: true, err: []error{access.ErrNotReady}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				lib := newLibrary(nil, []vault.Vault{kb}, nil)
				r := access.NewResolver(newGitHub(t), lib, nil, store.NewMemory(), discard())
				if testCase.stopFirst {
					r.Stop()
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				start := time.Now()
				done := resolveLater(ctx, r, access.User{Token: "alice"})
				synctest.Wait()
				if testCase.act != nil {
					testCase.act(lib, r, cancel)
				}
				res := <-done
				if testCase.err == nil && res.err != nil {
					t.Errorf("got err %v, want nil", res.err)
				}
				for _, want := range testCase.err {
					if !errors.Is(res.err, want) {
						t.Errorf("got err %v, want %v", res.err, want)
					}
				}
				if waited := time.Since(start); waited != testCase.waited {
					t.Errorf("waited: got %v, want %v", waited, testCase.waited)
				}
			})
		})
	}
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
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newGitHub(t)
		f.teams = map[string][]string{"alice": {"ours/editors"}}
		gated := configured(kb, vault.Config{Readers: []vault.Principal{{Org: "ours", Team: "editors"}}})
		lib := newLibrary(nil, []vault.Vault{gated}, nil)
		c := &countingCache{Memory: store.NewMemory()}
		r := access.NewResolver(f, lib, nil, c, discard())
		done := resolveLater(t.Context(), r, access.User{Login: "alice", Token: "alice"})
		synctest.Wait()
		gets := c.count()
		lib.set(nil, []vault.Vault{gated}, nil)
		synctest.Wait()
		if got := c.count() - gets; got != 0 {
			t.Errorf("cache reads on waking: got %d, want 0", got)
		}
		lib.set([]vault.Vault{gated}, nil, nil)
		if res := <-done; res.err != nil {
			t.Fatalf("got err %v, want nil", res.err)
		}
	})
}

func TestCachePerToken(t *testing.T) {
	t.Parallel()
	// The resolver and store.Memory read time.Now, which synctest's fake clock moves.
	synctest.Test(t, func(t *testing.T) {
		f, r := setup(t)
		ctx := t.Context()

		if _, err := r.Resolve(ctx, access.User{Token: "alice"}); err != nil {
			t.Fatalf("got err %v, want nil", err)
		}
		calls := f.callCount()
		if _, err := r.Resolve(ctx, access.User{Token: "alice"}); err != nil {
			t.Fatalf("got err %v, want nil", err)
		}
		if got := f.callCount() - calls; got != 0 {
			t.Errorf("GitHub calls within five minutes: got %d, want 0", got)
		}

		// Another token of the same user is checked on its own: a revoked one stays revoked.
		f.mu.Lock()
		f.revoked["alice2"] = true
		f.mu.Unlock()
		if _, err := r.Resolve(ctx, access.User{ID: 1, Token: "alice2"}); !errors.Is(err, access.ErrUnauthorized) {
			t.Errorf("revoked second token: got err %v, want %v", err, access.ErrUnauthorized)
		}

		// After five minutes GitHub is asked again, and a removed permission takes effect.
		f.mu.Lock()
		f.repos["alice"] = nil
		f.mu.Unlock()
		time.Sleep(5*time.Minute + time.Second)
		if _, err := r.Resolve(ctx, access.User{Token: "alice"}); !errors.Is(err, access.ErrNoVaults) {
			t.Errorf("after expiry: got err %v, want %v", err, access.ErrNoVaults)
		}
	})
}

func TestCacheShared(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	shared, err := store.NewRedis(t.Context(), "redis://"+mr.Addr(), discard())
	if err != nil {
		t.Fatalf("got err %v, want nil", err)
	}
	t.Cleanup(func() { _ = shared.Close() })
	f := newGitHub(t)
	vaults := newLibrary([]vault.Vault{kb}, nil, nil)
	one := access.NewResolver(f, vaults, []string{"ours"}, shared, discard())
	two := access.NewResolver(f, vaults, []string{"ours"}, shared, discard())

	if _, err := one.Resolve(t.Context(), access.User{Token: "alice"}); err != nil {
		t.Fatalf("got err %v, want nil", err)
	}
	calls := f.callCount()
	g, err := two.Resolve(t.Context(), access.User{Token: "alice", Write: true})
	if err != nil {
		t.Fatalf("got err %v, want nil", err)
	}
	if got := f.callCount() - calls; got != 0 {
		t.Errorf("GitHub calls from the second instance: got %d, want 0", got)
	}
	if !g.CanWrite(kb.RepoID) {
		t.Errorf("CanWrite(ours/kb) from the cached role: got false, want true")
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
	t.Parallel()
	r := access.NewResolver(newGitHub(t), newLibrary([]vault.Vault{kb}, nil, nil), nil, failingCache{}, discard())
	if _, err := r.Resolve(t.Context(), access.User{Token: "alice"}); err != nil {
		t.Errorf("got err %v, want nil", err)
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
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newGitHub(t)
		b := blockingGitHub{fakeGitHub: f, release: make(chan struct{})}
		r := access.NewResolver(b, newLibrary([]vault.Vault{kb}, nil, nil), nil, store.NewMemory(), discard())

		// A caller that leaves doesn't fail the lookup for the others.
		gone, cancel := context.WithCancel(t.Context())
		left := resolveLater(gone, r, access.User{Token: "alice"})
		var stayed []<-chan resolved
		for range 3 {
			stayed = append(stayed, resolveLater(t.Context(), r, access.User{Token: "alice"}))
		}
		synctest.Wait()
		cancel()
		if res := <-left; !errors.Is(res.err, context.Canceled) {
			t.Errorf("caller that left: got err %v, want %v", res.err, context.Canceled)
		}
		close(b.release)
		for _, done := range stayed {
			if res := <-done; res.err != nil {
				t.Errorf("got err %v, want nil", res.err)
			}
		}
		if got := f.callCount(); got != 1 {
			t.Errorf("GitHub calls: got %d, want 1", got)
		}
	})
}

func TestGrantVaultNames(t *testing.T) {
	t.Parallel()
	g := access.NewGrant(access.User{}, []vault.Vault{{RepoID: 1, Name: "ours/kb"}}, nil)
	testCases := []struct {
		name   string
		lookup string
		found  bool
	}{
		{name: "ASCII case folds", lookup: "Ours/KB", found: true},
		{name: "Kelvin sign is not a k", lookup: "ours/Kb"},
		{name: "same length, different name", lookup: "ours/kc"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			v, ok := g.Vault(testCase.lookup)
			if ok != testCase.found {
				t.Fatalf("Vault(%q) found: got %v, want %v", testCase.lookup, ok, testCase.found)
			}
			if ok && v.Name != "ours/kb" {
				t.Errorf("Vault(%q): got %q, want %q", testCase.lookup, v.Name, "ours/kb")
			}
		})
	}
}
