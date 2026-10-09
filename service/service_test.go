package service

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestService(t *testing.T) *Service {
	t.Helper()
	s, err := New(Config{Listen: "127.0.0.1:0", MetricsListen: "127.0.0.1:1"}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestNewRejectsBadConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want []string
	}{
		{
			name: "same address",
			cfg:  Config{Listen: ":8080", MetricsListen: ":8080"},
			want: []string{"metrics_listen"},
		},
		{
			name: "same address by default",
			cfg:  Config{Listen: ":9090"},
			want: []string{"metrics_listen"},
		},
		{
			name: "bad log level",
			cfg:  Config{Telemetry: Telemetry{LogLevel: "loud"}},
			want: []string{"log_level"},
		},
		{
			name: "every problem at once",
			cfg:  Config{Listen: ":1", MetricsListen: ":1", Telemetry: Telemetry{LogLevel: "loud"}},
			want: []string{"metrics_listen", "log_level"},
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
	s, err := New(Config{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.cfg.Listen != ":8080" || s.cfg.MetricsListen != ":9090" || s.cfg.Telemetry.LogLevel != "info" {
		t.Errorf("defaults not applied: %+v", s.cfg)
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
		{public, "/metrics", http.StatusNotFound},
		{internal, "/metrics", http.StatusOK},
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

func TestRunServesUntilContextEnds(t *testing.T) {
	s, err := New(Config{Listen: "127.0.0.1:0", MetricsListen: "127.0.0.1:0"}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	select {
	case <-s.started:
	case err := <-done:
		t.Fatalf("Run returned early: %v", err)
	}

	for _, url := range []string{
		"http://" + s.publicAddr.String() + "/readyz",
		"http://" + s.internalAddr.String() + "/metrics",
	} {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", url, resp.StatusCode)
		}
	}

	cancel()
	err = <-done
	if err != nil {
		t.Errorf("Run: %v", err)
	}
	if s.ready.Load() {
		t.Error("still ready after shutdown")
	}
}

func TestRunFailsOnBusyAddress(t *testing.T) {
	busy := httptest.NewServer(http.NotFoundHandler())
	defer busy.Close()
	addr := strings.TrimPrefix(busy.URL, "http://")

	s, err := New(Config{Listen: addr, MetricsListen: "127.0.0.1:0"}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = s.Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "listen") {
		t.Errorf("Run = %v, want a listen error", err)
	}
}
