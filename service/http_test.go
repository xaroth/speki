package service_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/xaroth/speki/library"
	"github.com/xaroth/speki/vault"
)

// Internal endpoints are never reachable through the public server, nor public ones through the internal.
func TestRoutes(t *testing.T) {
	t.Parallel()
	s := startService(t, testConfig(t, newGitHub(t, kbFiles, nil)))
	public, internal := s.URLs()
	testCases := []struct {
		name string
		url  string
		want int
	}{
		{name: "public healthz", url: public + "/healthz", want: http.StatusOK},
		{name: "public readyz", url: public + "/readyz", want: http.StatusOK},
		{name: "public OAuth metadata", url: public + "/.well-known/oauth-authorization-server", want: http.StatusOK},
		{name: "public resource metadata", url: public + "/.well-known/oauth-protected-resource/mcp", want: http.StatusOK},
		{name: "public MCP without a token", url: public + "/mcp", want: http.StatusUnauthorized},
		{name: "public metrics", url: public + "/metrics", want: http.StatusNotFound},
		{name: "public status", url: public + "/status", want: http.StatusNotFound},
		{name: "internal metrics", url: internal + "/metrics", want: http.StatusOK},
		{name: "internal status", url: internal + "/status", want: http.StatusOK},
		{name: "internal MCP", url: internal + "/mcp", want: http.StatusNotFound},
		{name: "internal healthz", url: internal + "/healthz", want: http.StatusNotFound},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := get(t, testCase.url).status; got != testCase.want {
				t.Errorf("got %d, want %d", got, testCase.want)
			}
		})
	}
}

// Until the first discovery round, /readyz and /mcp answer 503, so clients retry rather than see no vaults.
func TestReadyz(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	s := newService(t, testConfig(t, newGitHub(t, kbFiles, nil, holdRoute("GET /app/installations", release))))
	run(t, s)
	public, _ := s.URLs()

	if got := get(t, public+"/readyz").status; got != http.StatusServiceUnavailable {
		t.Errorf("readyz before discovery: got %d, want 503", got)
	}
	resp := get(t, public+"/mcp")
	if resp.status != http.StatusServiceUnavailable || resp.header.Get("Retry-After") != "5" {
		t.Errorf("mcp before discovery: got %d with Retry-After %q, want 503 with 5", resp.status, resp.header.Get("Retry-After"))
	}

	close(release)
	s.WaitServing()
	if got := get(t, public+"/readyz").status; got != http.StatusOK {
		t.Errorf("readyz after discovery: got %d, want 200", got)
	}
}

func TestStatus(t *testing.T) {
	t.Parallel()
	const memoryWarning = "no redis.url: sign-in state is kept in memory, so run exactly one instance; several instances need redis.url"
	testCases := []struct {
		name  string
		redis bool
		want  statusPage
	}{
		{name: "memory store", want: statusPage{Store: "memory", Warnings: []string{memoryWarning}}},
		{name: "redis", redis: true, want: statusPage{Store: "redis", Warnings: []string{}}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			g := newGitHub(t, kbFiles, nil)
			cfg := testConfig(t, g)
			if testCase.redis {
				cfg.Redis.URL = "redis://" + miniredis.RunT(t).Addr()
			}
			var logs syncBuffer
			s, err := create(cfg, slog.New(slog.NewTextHandler(&logs, nil)))
			if err != nil {
				t.Fatal(err)
			}
			if logged, want := strings.Contains(logs.String(), memoryWarning), !testCase.redis; logged != want {
				t.Errorf("got memory store warning logged %t, want %t:\n%s", logged, !testCase.redis, logs.String())
			}
			run(t, s)
			s.WaitServing()

			want := testCase.want
			want.Ready = true
			want.Vaults = []library.Status{{
				RepoID: 100, Name: kbRepo, State: library.StateServing, Commit: g.BranchSHA(kbRepo, branch), Documents: 2,
			}}
			want.Problems = []vault.Problem{}
			got := getStatus(t, s)
			ignore := cmpopts.IgnoreFields(library.Status{}, "Bytes", "LastSync", "Fresh")
			if diff := cmp.Diff(want, got, ignore); diff != "" {
				t.Errorf("status (-want +got):\n%s", diff)
			}
		})
	}
}

// syncBuffer is a bytes.Buffer that is safe for concurrent use: the service logs from several goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
