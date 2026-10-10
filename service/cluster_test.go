package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// startService runs a service until the test ends, and returns once its vaults are served.
func startService(t *testing.T, cfg Config) *Service {
	t.Helper()
	s, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	<-s.discovered
	s.library.WaitIdle()
	return s
}

func connectMCP(t *testing.T, s *Service, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "1"}, nil)
	cs, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:             "http://" + s.publicAddr.String() + "/mcp",
		HTTPClient:           &http.Client{Transport: bearer(token)},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// TestSeveralInstances runs two instances that share Redis, as behind a load balancer without sticky
// sessions.
func TestSeveralInstances(t *testing.T) {
	fake := newFakeGitHub(t, map[string]string{
		"speki.yaml": "title: KB\ndescription: Test vault.\n",
		"fleet.md":   "# Fleet\n\nWe fly zeppelins.\n",
	})
	mr := miniredis.RunT(t)
	cfgA := testConfig(t, fake)
	cfgA.Redis.URL = Secret("redis://" + mr.Addr())
	cfgB := cfgA // the same keys, app and Redis
	cfgB.Sync.DataDir = t.TempDir()
	a := startService(t, cfgA)
	b := startService(t, cfgB)

	// Sign in on one, use the other.
	tokens := signInFully(t, "http://"+a.publicAddr.String())
	onA := connectMCP(t, a, tokens.AccessToken)
	onB := connectMCP(t, b, tokens.AccessToken)
	if text := callTool(t, onB, "search", map[string]any{"query": "zeppelin"}); !strings.Contains(text, "fleet.md") {
		t.Errorf("search on the other instance:\n%s", text)
	}

	// Write on one, find it on the other within seconds: the change notice makes it sync, long before
	// its next poll.
	callTool(t, onA, "write", map[string]any{"vault": fakeRepo, "path": "airships.md",
		"content": "---\ntitle: Airships\n---\nDirigibles live in hangar nine.\n"})
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(200 * time.Millisecond) // under the per-user rate limit
	defer tick.Stop()
	for found := false; !found; {
		select {
		case <-deadline:
			t.Fatal("the other instance didn't pick up the write")
		case <-tick.C:
			found = strings.Contains(callTool(t, onB, "search", map[string]any{"query": "dirigible"}), "airships.md")
		}
	}

	// Refresh the same token on both at once: both get the same new tokens, and GitHub sees one refresh.
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tokens.RefreshToken}, "client_id": {tokens.ClientID}}
	var wg sync.WaitGroup
	results := make([]signedIn, 2)
	codes := make([]int, 2)
	for i, s := range []*Service{a, b} {
		wg.Go(func() {
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+s.publicAddr.String()+"/oauth/token", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = resp.Body.Close() }()
			codes[i] = resp.StatusCode
			_ = json.NewDecoder(resp.Body).Decode(&results[i])
		})
	}
	wg.Wait()
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK || results[0].AccessToken == "" || results[0].AccessToken != results[1].AccessToken {
		t.Errorf("concurrent refreshes: %v, same token %v", codes, results[0].AccessToken == results[1].AccessToken)
	}
	if fake.refreshes != 1 {
		t.Errorf("GitHub saw %d refreshes", fake.refreshes)
	}
}

func TestBadRedisFailsStartup(t *testing.T) {
	fake := newFakeGitHub(t, map[string]string{"speki.yaml": "title: KB\ndescription: Test vault.\n"})
	cfg := testConfig(t, fake)
	cfg.Redis.URL = "redis://127.0.0.1:1"
	_, err := New(cfg, discardLogger())
	if err == nil || !strings.Contains(err.Error(), "redis.url") {
		t.Errorf("New = %v", err)
	}
}
