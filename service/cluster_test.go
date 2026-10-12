package service_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/eve-online-tools/speki/service"
)

// TestSeveralInstances runs two instances that share Redis, as behind a load balancer without sticky
// sessions.
func TestSeveralInstances(t *testing.T) {
	t.Parallel()
	g := newGitHub(t, kbFiles, nil)
	cfgA := testConfig(t, g)
	cfgA.Redis.URL = "redis://" + miniredis.RunT(t).Addr()
	cfgB := cfgA // the same keys, app and Redis
	cfgB.Sync.DataDir = t.TempDir()
	a := startService(t, cfgA)
	b := startService(t, cfgB)

	// Sign in on one, use the other.
	tokens := signIn(t, a)
	onA := connectMCP(t, a, tokens.AccessToken)
	onB := connectMCP(t, b, tokens.AccessToken)
	if text := callTool(t, onB, "search", map[string]any{"query": "zeppelin"}); !strings.Contains(text, "fleet.md") {
		t.Errorf("search on the other instance: got\n%s\nwant fleet.md", text)
	}

	// Write on one, find it on the other within seconds: the change notice makes it sync, long before
	// its next poll.
	callTool(t, onA, "write", map[string]any{"vault": kbRepo, "path": "airships.md",
		"content": "---\ntitle: Airships\n---\nDirigibles live in hangar nine.\n"})
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(100 * time.Millisecond) // at the per-user rate limit
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
	for i, s := range []*service.Service{a, b} {
		wg.Go(func() {
			public, _ := s.URLs()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, public+"/oauth/token", strings.NewReader(form.Encode()))
			if err != nil {
				t.Error(err)
				return
			}
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
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
		t.Errorf("concurrent refreshes: got %v, want both 200", codes)
	}
	if results[0].AccessToken == "" || results[0] != results[1] {
		t.Errorf("concurrent refreshes: got %+v and %+v, want the same tokens", results[0], results[1])
	}
	var exchanges int
	for _, r := range g.Requests() {
		if r == "POST /login/oauth/access_token" {
			exchanges++
		}
	}
	if exchanges != 2 { // the sign-in, and one refresh
		t.Errorf("got %d token requests to GitHub, want 2", exchanges)
	}
}
