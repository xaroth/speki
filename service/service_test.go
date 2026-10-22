package service_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xaroth/speki/gh/ghtest"
	"github.com/xaroth/speki/library"
	"github.com/xaroth/speki/service"
)

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

	g.Apply(ghtest.ExpectInstallation_Refused(1))
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
			wait := ghtest.ExpectRoute("POST /graphql", func(next http.Handler) http.Handler {
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
