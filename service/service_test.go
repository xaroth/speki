package service

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestService(t *testing.T) *Service {
	t.Helper()
	cfg := testConfig(t, newFakeGitHub(t, map[string]string{"a.md": "# A\n"}))
	s, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// validConfig has every required setting, and nothing else.
func validConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		PublicURL: "https://speki.example.com",
		GitHub:    GitHub{AppID: 1, PrivateKey: testKey(t), ClientID: "Iv1", ClientSecret: "s"},
		Auth:      Auth{Keys: testKeys()},
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	base := validConfig(t)
	with := func(change func(*Config)) Config {
		c := base
		change(&c)
		return c
	}
	tests := []struct {
		name string
		cfg  Config
		want []string
	}{
		{
			name: "same address",
			cfg:  with(func(c *Config) { c.Listen, c.MetricsListen = ":8080", ":8080" }),
			want: []string{"metrics_listen"},
		},
		{
			name: "same address by default",
			cfg:  with(func(c *Config) { c.Listen = ":9090" }),
			want: []string{"metrics_listen"},
		},
		{
			name: "bad log level",
			cfg:  with(func(c *Config) { c.Telemetry.LogLevel = "loud" }),
			want: []string{"log_level"},
		},
		{
			name: "every problem at once",
			cfg:  Config{Listen: ":1", MetricsListen: ":1", Telemetry: Telemetry{LogLevel: "loud"}},
			want: []string{"metrics_listen", "log_level", "app_id", "private_key", "client_id", "public_url", "auth.keys"},
		},
		{
			name: "bad private key",
			cfg:  with(func(c *Config) { c.GitHub.PrivateKey = "not a key" }),
			want: []string{"github app key"},
		},
		{
			name: "plain http public URL",
			cfg:  with(func(c *Config) { c.PublicURL = "http://speki.example.com" }),
			want: []string{"https"},
		},
		{
			name: "bad key file",
			cfg:  with(func(c *Config) { c.Auth.Keys = "keys: []" }),
			want: []string{"auth.keys"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.cfg, discardLogger())
			if err == nil {
				t.Fatal("New succeeded")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q doesn't mention %q", err, w)
				}
			}
		})
	}
}

func TestNewDefaults(t *testing.T) {
	s, err := New(validConfig(t), discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := s.cfg
	if c.Listen != ":8080" || c.MetricsListen != ":9090" || c.Telemetry.LogLevel != "info" ||
		c.GitHub.GitURL != "https://github.com" || c.Sync.DataDir != "data" ||
		c.Sync.PollInterval != time.Minute || c.Sync.DiscoveryInterval != 5*time.Minute ||
		c.GitHub.OAuthURL != "https://github.com" || c.Auth.ClientMetadataHosts[0] != "claude.ai" {
		t.Errorf("defaults not applied: %+v", c)
	}
}

func TestEndpointsAreSeparated(t *testing.T) {
	s := newTestService(t)
	public := httptest.NewServer(s.publicHandler())
	defer public.Close()
	internal := httptest.NewServer(s.internalHandler())
	defer internal.Close()

	tests := []struct {
		server *httptest.Server
		path   string
		want   int
	}{
		{public, "/healthz", http.StatusOK},
		{public, "/.well-known/oauth-authorization-server", http.StatusOK},
		{public, "/.well-known/oauth-protected-resource/mcp", http.StatusOK},
		{public, "/mcp", http.StatusUnauthorized},
		{internal, "/mcp", http.StatusNotFound},
		{public, "/metrics", http.StatusNotFound},
		{public, "/status", http.StatusNotFound},
		{internal, "/metrics", http.StatusOK},
		{internal, "/status", http.StatusOK},
		{internal, "/healthz", http.StatusNotFound},
	}
	for _, tt := range tests {
		resp, err := http.Get(tt.server.URL + tt.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tt.path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tt.want {
			name := "public"
			if tt.server == internal {
				name = "internal"
			}
			t.Errorf("%s %s = %d, want %d", name, tt.path, resp.StatusCode, tt.want)
		}
	}
}

func TestReadyz(t *testing.T) {
	s := newTestService(t)
	h := s.publicHandler()

	status := func() int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return rec.Code
	}

	if got := status(); got != http.StatusServiceUnavailable {
		t.Errorf("before start: %d, want 503", got)
	}
	s.ready.Store(true)
	if got := status(); got != http.StatusOK {
		t.Errorf("when ready: %d, want 200", got)
	}
}

func TestRunDiscoversAndServesVaults(t *testing.T) {
	fake := newFakeGitHub(t, map[string]string{
		"speki.yaml":   "title: KB\ndescription: Test vault.\n",
		"fleet.md":     "# Fleet\n\nzeppelin notes\n",
		"ops/roles.md": "# Roles\n",
	})
	s, err := New(testConfig(t, fake), discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	select {
	case <-s.discovered:
	case err := <-done:
		t.Fatalf("Run returned early: %v", err)
	}
	s.library.WaitIdle()

	resp, err := http.Get("http://" + s.publicAddr.String() + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("readyz = %d", resp.StatusCode)
	}

	page := getStatus(t, s)
	if !page.Ready || len(page.Vaults) != 1 {
		t.Fatalf("status = %+v", page)
	}
	v := page.Vaults[0]
	if v.Name != fakeRepo || v.State != "serving" || v.Documents != 2 {
		t.Errorf("vault status = %+v", v)
	}

	cancel()
	err = <-done
	if err != nil {
		t.Errorf("Run: %v", err)
	}
	if s.ready.Load() {
		t.Error("still ready after shutdown")
	}

	// A restart serves the vault from disk, even when GitHub doesn't answer.
	fake.api.Close()
	s2, err := New(s.cfg, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()
	go func() { done <- s2.Run(ctx2) }()
	<-s2.discovered
	page = getStatus(t, s2)
	if len(page.Vaults) != 1 || page.Vaults[0].State != "serving" {
		t.Errorf("after restart without GitHub: %+v", page)
	}
	cancel2()
	<-done
}

func getStatus(t *testing.T, s *Service) statusPage {
	t.Helper()
	resp, err := http.Get("http://" + s.internalAddr.String() + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var page statusPage
	err = json.NewDecoder(resp.Body).Decode(&page)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func TestRunFailsOnBusyAddress(t *testing.T) {
	busy := httptest.NewServer(http.NotFoundHandler())
	defer busy.Close()
	addr := strings.TrimPrefix(busy.URL, "http://")

	cfg := testConfig(t, newFakeGitHub(t, map[string]string{"a.md": "# A\n"}))
	cfg.Listen = addr
	s, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = s.Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "listen") {
		t.Errorf("Run = %v, want a listen error", err)
	}
}
