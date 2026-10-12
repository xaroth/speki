package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eve-online-tools/speki/gh"
	"github.com/eve-online-tools/speki/library"
	"github.com/eve-online-tools/speki/service"
	"github.com/eve-online-tools/speki/vault"
)

var kbFiles = map[string]string{
	"speki.yaml":   "title: KB\ndescription: Test vault.\n",
	"fleet.md":     "# Fleet\n\nWe fly zeppelins.\n",
	"ops/roles.md": "# Roles\n",
}

func TestNew(t *testing.T) {
	t.Parallel()
	base := validConfig(t)
	with := func(change func(*service.Config)) service.Config {
		c := base
		change(&c)
		return c
	}
	testCases := []struct {
		name string
		cfg  service.Config
		want []string // in the error; none for a config New takes
	}{
		{name: "key file", cfg: base},
		{name: "single key", cfg: with(func(c *service.Config) { c.Auth = service.Auth{Key: testSecret()} })},
		{
			name: "every problem at once",
			cfg:  service.Config{Listen: ":1", MetricsListen: ":1", Telemetry: service.Telemetry{LogLevel: "loud"}},
			want: []string{"metrics_listen", "log_level", "app_id", "private_key", "client_id", "public_url", "auth.keys"},
		},
		{
			name: "bad keys reported together",
			cfg: with(func(c *service.Config) {
				c.GitHub.PrivateKey, c.Auth.Keys, c.Telemetry.LogLevel = "not a key", "keys: []", "loud"
			}),
			want: []string{"github.private_key", "auth.keys", "log_level"},
		},
		{
			name: "same address by default",
			cfg:  with(func(c *service.Config) { c.Listen = ":9090" }),
			want: []string{"metrics_listen"},
		},
		{
			name: "query memory over 3/4 of the limit",
			cfg:  with(func(c *service.Config) { c.SQLite.MemoryLimit, c.SQLite.QueryMemory = 512, 400 }),
			want: []string{"sqlite.query_memory_mib"},
		},
		{
			name: "tiny memory limit",
			cfg:  with(func(c *service.Config) { c.SQLite.MemoryLimit = 64 }),
			want: []string{"sqlite.memory_limit_mib"},
		},
		{
			name: "plain http public URL",
			cfg:  with(func(c *service.Config) { c.PublicURL = "http://speki.example.com" }),
			want: []string{"https"},
		},
		{
			name: "bad single key",
			cfg:  with(func(c *service.Config) { c.Auth.Keys, c.Auth.Key = "", "c2hvcnQ=" }),
			want: []string{"auth.key:", "at least 32"},
		},
		{
			name: "single key and key file",
			cfg:  with(func(c *service.Config) { c.Auth.Key = testSecret() }),
			want: []string{"auth.key and auth.keys are both set"},
		},
		{
			name: "metadata host with a scheme",
			cfg: with(func(c *service.Config) {
				c.Auth.ClientMetadataHosts = []string{"https://claude.ai", "Claude.ai", "claude.ai."}
			}),
			want: []string{"client_metadata_hosts"},
		},
		{
			name: "account ID that isn't one",
			cfg:  with(func(c *service.Config) { c.GitHub.AllowedAccounts = []int64{0} }),
			want: []string{"allowed_accounts"},
		},
		{
			name: "bad instance",
			cfg: with(func(c *service.Config) {
				c.Instance = service.Instance{Name: "Work!", Title: "hidden\u202etext", Description: "tag\U000E0041"}
			}),
			want: []string{"instance.name", "instance.title", "instance.description"},
		},
		{
			name: "long instance description",
			cfg:  with(func(c *service.Config) { c.Instance.Description = strings.Repeat("x", 301) }),
			want: []string{"instance.description"},
		},
		{
			name: "negative interval",
			cfg:  with(func(c *service.Config) { c.Sync.DiscoveryInterval = -1 }),
			want: []string{"sync.discovery_interval"},
		},
		{
			name: "redis not answering",
			cfg:  with(func(c *service.Config) { c.Redis.URL = "redis://" + hangUp(t) + "?max_retries=-1" }),
			want: []string{"redis.url"},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := create(testCase.cfg, discardLogger())
			if len(testCase.want) == 0 {
				if err != nil {
					t.Fatalf("got %v, want no error", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("got no error, want one mentioning %q", testCase.want)
			}
			for _, w := range testCase.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("got error %q, want it to mention %q", err, w)
				}
			}
		})
	}
}

// hangUp listens for connections and closes each one straight away, so a client fails fast rather than
// retrying a refused dial.
func hangUp(tb testing.TB) string {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().String()
}

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

// holdRoute makes GitHub hold requests to route until release is closed or the request ends.
func holdRoute(route string, release <-chan struct{}) gh.FakeOption {
	return gh.ExpectRoute(route, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			next.ServeHTTP(w, r)
		})
	})
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

// Stopped before the first discovery round finished: the service must not turn ready during the drain.
func TestNotReadyAfterEarlyStop(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	defer close(release)
	s := newService(t, testConfig(t, newGitHub(t, kbFiles, nil, holdRoute("GET /app/installations", release))))
	stop := run(t, s)
	s.URLs()
	if err := stop(); err != nil {
		t.Errorf("Run: got %v, want nil", err)
	}
	if s.Ready() {
		t.Error("got ready after stopping, want not ready")
	}
}

type statusPage struct {
	Ready    bool             `json:"ready"`
	Store    string           `json:"store"`
	Vaults   []library.Status `json:"vaults"`
	Problems []vault.Problem  `json:"problems"`
	Warnings []string         `json:"warnings"`
}

func getStatus(tb testing.TB, s *service.Service) statusPage {
	tb.Helper()
	_, internal := s.URLs()
	var page statusPage
	if err := json.Unmarshal(get(tb, internal+"/status").body, &page); err != nil {
		tb.Fatal(err)
	}
	return page
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

// A restart serves the vaults on disk, even when GitHub refuses the app.
func TestRestartWithoutGitHub(t *testing.T) {
	t.Parallel()
	g := newGitHub(t, kbFiles, nil)
	cfg := testConfig(t, g)
	first := newService(t, cfg)
	stop := run(t, first)
	first.WaitServing()
	if err := stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	g.Apply(gh.ExpectInstallation_Refused(1))
	s := startService(t, cfg)
	page := getStatus(t, s)
	if len(page.Vaults) != 1 || page.Vaults[0].State != library.StateServing {
		t.Errorf("got vaults %+v, want ours/kb serving", page.Vaults)
	}
}

func TestRunFails(t *testing.T) {
	t.Parallel()
	busy := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(busy.Close)
	busyAddr := strings.TrimPrefix(busy.URL, "http://")
	testCases := []struct {
		name   string
		change func(*service.Config)
		want   string
	}{
		{name: "busy listen address", change: func(c *service.Config) { c.Listen = busyAddr }, want: "listen"},
		{name: "busy metrics address", change: func(c *service.Config) { c.MetricsListen = busyAddr }, want: "metrics_listen"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig(t, newGitHub(t, kbFiles, nil))
			testCase.change(&cfg)
			err := newService(t, cfg).Run(t.Context())
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("got %v, want a %s error", err, testCase.want)
			}
		})
	}
}

// A service runs once: a second Run fails, while the first runs and after it ended, without touching
// the first.
func TestRunTwice(t *testing.T) {
	t.Parallel()
	s := newService(t, testConfig(t, newGitHub(t, kbFiles, nil)))
	stop := run(t, s)
	s.URLs()
	if err := s.Run(t.Context()); err == nil {
		t.Error("got no error from a second Run while the first runs, want one")
	}
	if err := stop(); err != nil {
		t.Errorf("first Run: got %v, want nil", err)
	}
	if err := s.Run(t.Context()); err == nil {
		t.Error("got no error from Run after the first ended, want one")
	}
}

// A stopping service lets running requests finish before it closes the components, and gives background
// work its own budget, after the requests'.
func TestStopDuringASlowCommit(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name string
		// drain and background are the stop timeouts. Zero keeps the default.
		drain, background time.Duration
		stuckBackground   bool
		// commitDelay is how long the commit takes after the stop began. Negative holds it until the
		// write's connection was cut.
		commitDelay time.Duration
		wantSaved   bool
	}{
		{name: "commit within the drain", wantSaved: true},
		{name: "commit slower than the drain", drain: 10 * time.Millisecond, commitDelay: -1},
		{
			name:            "background work stuck",
			background:      10 * time.Millisecond,
			stuckBackground: true,
			commitDelay:     50 * time.Millisecond, // longer than the background budget
			wantSaved:       true,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			committing, hold := make(chan struct{}), make(chan struct{})
			var once sync.Once
			wait := gh.ExpectRoute("POST /graphql", func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					once.Do(func() { close(committing) })
					<-hold
					next.ServeHTTP(w, r)
				})
			})
			s := newService(t, testConfig(t, newGitHub(t, kbFiles, nil, wait)))
			drain, background := 15*time.Second, 5*time.Second
			if testCase.drain != 0 {
				drain = testCase.drain
			}
			if testCase.background != 0 {
				background = testCase.background
			}
			s.SetStopTimeouts(drain, background)
			if testCase.stuckBackground {
				stuck := make(chan struct{})
				t.Cleanup(func() { close(stuck) })
				s.StartBackground(func() { <-stuck })
			}
			stop := run(t, s)
			s.WaitServing()
			cs := connectMCP(t, s, signIn(t, s).AccessToken)

			type answer struct {
				text    string
				isError bool
				err     error
			}
			answered := make(chan answer, 1)
			go func() {
				res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "write", Arguments: map[string]any{
					"vault": kbRepo, "path": "deploy.md", "content": "# Deploy\n"}})
				if err != nil {
					answered <- answer{err: err}
					return
				}
				answered <- answer{text: res.Content[0].(*mcp.TextContent).Text, isError: res.IsError}
			}()
			<-committing
			stopped := make(chan error, 1)
			go func() { stopped <- stop() }()

			var got answer
			var runErr error
			if testCase.commitDelay < 0 {
				got = <-answered // the connection is cut while the commit waits
				close(hold)
				runErr = <-stopped
			} else {
				for s.Ready() { // the stop began
					runtime.Gosched()
				}
				time.AfterFunc(testCase.commitDelay, func() { close(hold) })
				select {
				case got = <-answered:
					runErr = <-stopped
				case runErr = <-stopped:
					got = <-answered
					t.Error("Run returned before the write answered")
				}
			}
			if runErr != nil {
				t.Errorf("Run: got %v, want nil", runErr)
			}
			idle, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
			defer cancel()
			if !s.WaitHandlers(idle) {
				t.Error("Run returned, closing the components, under a running handler")
			}

			saved := got.err == nil && !got.isError && strings.Contains(got.text, "Saved in commit")
			if saved != testCase.wantSaved || !testCase.wantSaved && got.err == nil {
				t.Errorf("got write answer %+v, want saved %t, else a cut connection", got, testCase.wantSaved)
			}
		})
	}
}
