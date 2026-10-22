package ghcache_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/xaroth/speki/gh"
	"github.com/xaroth/speki/gh/ghcache"
	"github.com/xaroth/speki/gh/ghtest"
	"github.com/xaroth/speki/store"
)

const (
	headAge      = 50 * time.Second
	discoveryAge = 4 * time.Minute
	installsPath = "GET /app/installations"
	branchPath   = "GET /repos/acme/kb/git/ref/heads/main"
)

var install = gh.Installation{ID: 1, AccountID: 10}

var repo = gh.Repository{ID: 100, FullName: "acme/kb", DefaultBranch: "main"}

// clock is a settable time, shared by the instances of a test.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// instances returns two clients that share one store and one fake GitHub, as two instances of speki do.
func instances(tb testing.TB, fake *ghtest.Fake, kv ghcacheStore) ([]*ghcache.Client, *clock) {
	tb.Helper()
	clk := &clock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	out := make([]*ghcache.Client, 2)
	for i := range out {
		out[i] = ghcache.New(fake.Client, kv, ghcache.Options{Heads: headAge, Discovery: discoveryAge})
		out[i].SetNow(clk.Now)
	}
	return out, clk
}

type ghcacheStore interface {
	SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Release(ctx context.Context, key string, value []byte) error
}

// requests counts the fake's requests to route, query left out.
func requests(fake *ghtest.Fake, route string) int {
	n := 0
	for _, r := range fake.Requests() {
		path, _, _ := strings.Cut(r, "?")
		if path == route {
			n++
		}
	}
	return n
}

func TestInstallations(t *testing.T) {
	t.Parallel()
	fake := ghtest.New(t, ghtest.ExpectInstallation(install, repo))
	c, clk := instances(t, fake, store.NewMemory())
	ctx := t.Context()

	for i, step := range []struct {
		client  *ghcache.Client
		advance time.Duration
		want    int // requests to GitHub so far
	}{
		{client: c[0], want: 1},
		{client: c[1], want: 1},                        // the first instance's answer
		{client: c[1], advance: discoveryAge, want: 2}, // due again
		{client: c[0], want: 2},
	} {
		clk.Advance(step.advance)
		got, err := step.client.Installations(ctx)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if diff := cmp.Diff([]gh.Installation{install}, got); diff != "" {
			t.Errorf("step %d: installations (-want +got):\n%s", i, diff)
		}
		if n := requests(fake, installsPath); n != step.want {
			t.Errorf("step %d: got %d requests to GitHub, want %d", i, n, step.want)
		}
	}
}

func TestRepositories(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name         string
		opts         []ghtest.Option
		wantRepos    []gh.Repository
		wantNotFound bool
		wantRequests int // token requests, by both instances
	}{
		{
			name:         "listed",
			opts:         []ghtest.Option{ghtest.ExpectInstallation(install, repo)},
			wantRepos:    []gh.Repository{repo},
			wantRequests: 1,
		},
		{
			name:         "installation gone is shared",
			wantNotFound: true,
			wantRequests: 1,
		},
		{
			name: "other failures aren't shared",
			opts: []ghtest.Option{
				ghtest.ExpectInstallation(install, repo),
				ghtest.ExpectInstallation_Refused(install.ID),
			},
			wantRequests: 2,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := ghtest.New(t, testCase.opts...)
			c, _ := instances(t, fake, store.NewMemory())
			for i, client := range c {
				got, err := client.Repositories(t.Context(), install.ID)
				switch {
				case testCase.wantRepos != nil && err != nil:
					t.Fatalf("instance %d: %v", i, err)
				case testCase.wantRepos == nil && err == nil:
					t.Fatalf("instance %d: got %v, want an error", i, got)
				}
				if errors.Is(err, gh.ErrNotFound) != testCase.wantNotFound {
					t.Errorf("instance %d: got %v, want not found %v", i, err, testCase.wantNotFound)
				}
				if diff := cmp.Diff(testCase.wantRepos, got); diff != "" {
					t.Errorf("instance %d: repositories (-want +got):\n%s", i, diff)
				}
			}
			if n := requests(fake, "POST /app/installations/1/access_tokens"); n != testCase.wantRequests {
				t.Errorf("got %d token requests, want %d", n, testCase.wantRequests)
			}
		})
	}
}

func TestBranchHead(t *testing.T) {
	t.Parallel()
	fake := ghtest.New(t, ghtest.ExpectInstallation(install, repo), ghtest.ExpectBranch(repo.FullName, "main", "sha1"))
	c, clk := instances(t, fake, store.NewMemory())
	head1 := gh.Head{SHA: "sha1", ETag: `"sha1"`}
	head2 := gh.Head{SHA: "sha2", ETag: `"sha2"`}

	for i, step := range []struct {
		client  *ghcache.Client
		advance time.Duration
		push    string // moves the branch first
		etag    string
		want    gh.Head
		wantN   int // requests to GitHub so far
	}{
		{client: c[0], want: head1, wantN: 1},
		{client: c[1], want: head1, wantN: 1},
		{client: c[1], etag: head1.ETag, want: gh.Head{ETag: head1.ETag}, wantN: 1}, // unchanged for the caller
		{client: c[0], advance: headAge, etag: head1.ETag, want: gh.Head{ETag: head1.ETag}, wantN: 2},
		{client: c[1], push: "sha2", etag: head1.ETag, want: gh.Head{ETag: head1.ETag}, wantN: 2}, // not due yet
		{client: c[1], advance: headAge, etag: head1.ETag, want: head2, wantN: 3},
		{client: c[0], etag: head1.ETag, want: head2, wantN: 3},
		{client: c[0], etag: "", want: head2, wantN: 3},
	} {
		clk.Advance(step.advance)
		if step.push != "" {
			fake.Apply(ghtest.ExpectBranch(repo.FullName, "main", step.push))
		}
		got, err := step.client.BranchHead(t.Context(), install.ID, repo.FullName, "main", step.etag)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if got != step.want {
			t.Errorf("step %d: got %+v, want %+v", i, got, step.want)
		}
		if n := requests(fake, branchPath); n != step.wantN {
			t.Errorf("step %d: got %d requests to GitHub, want %d", i, n, step.wantN)
		}
	}
}

// TestBranchHead_AnotherInstanceAsking holds the first instance's request to GitHub while the second
// one reads.
func TestBranchHead_AnotherInstanceAsking(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name   string
		shared bool // the store has an older answer
		want   gh.Head
		wantN  int // requests to GitHub by the second instance
	}{
		{name: "nothing shared yet: asks itself", want: gh.Head{SHA: "sha2", ETag: `"sha2"`}, wantN: 1},
		{name: "older answer used", shared: true, want: gh.Head{SHA: "sha1", ETag: `"sha1"`}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := ghtest.New(t, ghtest.ExpectInstallation(install, repo), ghtest.ExpectBranch(repo.FullName, "main", "sha1"))
			c, clk := instances(t, fake, store.NewMemory())
			ctx := t.Context()
			if testCase.shared {
				_, err := c[0].BranchHead(ctx, install.ID, repo.FullName, "main", "")
				if err != nil {
					t.Fatal(err)
				}
				clk.Advance(headAge)
			}
			fake.Apply(ghtest.ExpectBranch(repo.FullName, "main", "sha2"))
			before := requests(fake, branchPath)

			var held atomic.Bool
			entered, release := make(chan struct{}), make(chan struct{})
			fake.Apply(ghtest.ExpectRoute("GET /repos/acme/kb/git/ref/heads/main", func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if held.CompareAndSwap(false, true) {
						close(entered)
						<-release
					}
					next.ServeHTTP(w, r)
				})
			}))
			done := make(chan error)
			go func() {
				_, err := c[0].BranchHead(ctx, install.ID, repo.FullName, "main", "")
				done <- err
			}()
			<-entered

			got, err := c[1].BranchHead(ctx, install.ID, repo.FullName, "main", "")
			close(release)
			if err != nil {
				t.Fatal(err)
			}
			if got != testCase.want {
				t.Errorf("got %+v, want %+v", got, testCase.want)
			}
			if n := requests(fake, branchPath) - before - 1; n != testCase.wantN {
				t.Errorf("got %d requests to GitHub, want %d", n, testCase.wantN)
			}
			err = <-done
			if err != nil {
				t.Errorf("first instance: %v", err)
			}
		})
	}
}

// brokenStore fails every call, as Redis does when it's unreachable.
type brokenStore struct{}

var errBroken = errors.New("store unreachable")

func (brokenStore) SetNX(context.Context, string, []byte, time.Duration) (bool, error) {
	return false, errBroken
}

func (brokenStore) Set(context.Context, string, []byte, time.Duration) error { return errBroken }

func (brokenStore) Get(context.Context, string) ([]byte, bool, error) { return nil, false, errBroken }

func (brokenStore) Release(context.Context, string, []byte) error { return errBroken }

func TestStoreUnreachable(t *testing.T) {
	t.Parallel()
	fake := ghtest.New(t, ghtest.ExpectInstallation(install, repo), ghtest.ExpectBranch(repo.FullName, "main", "sha1"))
	c, _ := instances(t, fake, brokenStore{})
	for i, client := range c {
		installs, err := client.Installations(t.Context())
		if err != nil {
			t.Fatalf("instance %d: installations: %v", i, err)
		}
		if diff := cmp.Diff([]gh.Installation{install}, installs); diff != "" {
			t.Errorf("instance %d: installations (-want +got):\n%s", i, diff)
		}
		repos, err := client.Repositories(t.Context(), install.ID)
		if err != nil {
			t.Fatalf("instance %d: repositories: %v", i, err)
		}
		if diff := cmp.Diff([]gh.Repository{repo}, repos); diff != "" {
			t.Errorf("instance %d: repositories (-want +got):\n%s", i, diff)
		}
		// The caller's own ETag goes to GitHub.
		head, err := client.BranchHead(t.Context(), install.ID, repo.FullName, "main", `"sha1"`)
		if err != nil {
			t.Fatalf("instance %d: branch head: %v", i, err)
		}
		if want := (gh.Head{ETag: `"sha1"`}); head != want {
			t.Errorf("instance %d: got %+v, want %+v", i, head, want)
		}
	}
	for _, route := range []string{installsPath, "GET /installation/repositories", branchPath} {
		if n := requests(fake, route); n != len(c) {
			t.Errorf("%s: got %d requests, want %d", route, n, len(c))
		}
	}
}
