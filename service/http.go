package service

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/xaroth/speki/library"
	"github.com/xaroth/speki/telemetry"
	"github.com/xaroth/speki/vault"
)

// track counts h's running handlers in s.handlers. The go-sdk's stateless handler returns only once its
// tool calls have.
func (s *Service) track(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handlersMu.Lock()
		if s.handlersClosed {
			s.handlersMu.Unlock()
			http.Error(w, "speki is stopping", http.StatusServiceUnavailable)
			return
		}
		s.handlers.Add(1)
		s.handlersMu.Unlock()
		defer s.handlers.Done()
		h.ServeHTTP(w, r)
	})
}

// publicHandler serves what MCP clients and browsers need: MCP, sign-in and health checks.
func (s *Service) publicHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/mcp", s.whenReady(s.mcp.Handler()))
	auth := s.auth.Handler()
	mux.Handle("/.well-known/", auth)
	mux.Handle("/oauth/", auth)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return s.track(telemetry.RequestID(telemetry.Trace(mux)))
}

// whenReady answers 503 until the service is ready, and again once it's stopping, so a client that
// reaches the instance directly retries rather than seeing no vaults.
func (s *Service) whenReady(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.ready.Load() {
			w.Header().Set("Retry-After", "5")
			http.Error(w, "speki is starting or stopping; try again", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// internalHandler serves metrics and the status page. It runs on its own listener and is never
// reachable through the public one.
func (s *Service) internalHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /status", s.status)
	return s.track(mux)
}

type statusPage struct {
	Ready    bool             `json:"ready"`
	Store    string           `json:"store"` // memory or redis
	Vaults   []library.Status `json:"vaults"`
	Problems []vault.Problem  `json:"problems"`
	Warnings []string         `json:"warnings"`
}

func (s *Service) status(w http.ResponseWriter, _ *http.Request) {
	page := statusPage{
		Ready:    s.ready.Load(),
		Store:    "redis",
		Vaults:   s.library.Status(),
		Problems: s.vaults.Problems(),
		Warnings: []string{},
	}
	if s.cfg.Redis.URL == "" {
		page.Store = "memory"
		page.Warnings = append(page.Warnings, memoryStoreWarning)
	}
	if page.Vaults == nil {
		page.Vaults = []library.Status{}
	}
	if page.Problems == nil {
		page.Problems = []vault.Problem{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(page)
}

func serve(srv *http.Server, ln net.Listener) error {
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// newServer's WriteTimeout is above the /mcp stack's 50 second request deadline, which includes
// access.Resolver's wait for vaults being built, with room for a change still looking for a commit whose
// answer was lost when the deadline came. The /mcp stack gives the body its own read deadline after the
// wait.
func newServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      75 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}
