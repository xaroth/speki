// Package service is speki without the process around it: it builds every component from a Config and
// runs them until its context ends.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"

	"github.com/eve-online-tools/speki/telemetry"
)

// shutdownTimeout is how long running requests get to finish once the service stops.
const shutdownTimeout = 25 * time.Second

// Service is one running speki.
type Service struct {
	cfg      Config
	logger   *slog.Logger
	registry *prometheus.Registry
	ready    atomic.Bool

	// started is closed once both listeners are bound; publicAddr and internalAddr are set by then.
	started      chan struct{}
	publicAddr   net.Addr
	internalAddr net.Addr
}

// New checks cfg, fills in defaults and builds every component. Nothing runs yet.
func New(cfg Config, logger *slog.Logger) (*Service, error) {
	cfg = cfg.withDefaults()
	err := cfg.validate()
	if err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}

	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	return &Service{
		cfg:      cfg,
		logger:   logger,
		registry: registry,
		started:  make(chan struct{}),
	}, nil
}

// Run starts the public and internal HTTP servers and blocks until ctx ends and shutdown is done.
func (s *Service) Run(ctx context.Context) error {
	var lc net.ListenConfig
	public, err := lc.Listen(ctx, "tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	internal, err := lc.Listen(ctx, "tcp", s.cfg.MetricsListen)
	if err != nil {
		_ = public.Close()
		return fmt.Errorf("metrics_listen: %w", err)
	}
	s.publicAddr = public.Addr()
	s.internalAddr = internal.Addr()

	publicSrv := newServer(s.publicHandler())
	internalSrv := newServer(s.internalHandler())

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return serve(publicSrv, public) })
	g.Go(func() error { return serve(internalSrv, internal) })
	g.Go(func() error {
		<-gctx.Done()
		s.ready.Store(false)
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		return errors.Join(publicSrv.Shutdown(shutdownCtx), internalSrv.Shutdown(shutdownCtx))
	})

	// The listeners are bound, so connections queue up even if Serve hasn't been reached yet. Being
	// ready before that is fine.
	s.ready.Store(true)
	close(s.started)
	s.logger.Info("speki started", "listen", s.publicAddr.String(), "metrics_listen", s.internalAddr.String())

	err = g.Wait()
	s.logger.Info("speki stopped")
	return err
}

func serve(srv *http.Server, ln net.Listener) error {
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// publicHandler serves what MCP clients and browsers need.
func (s *Service) publicHandler() http.Handler {
	mux := http.NewServeMux()
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
	return telemetry.RequestID(mux)
}

// internalHandler serves metrics. It runs on its own listener and is never reachable through the
// public one.
func (s *Service) internalHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{}))
	return mux
}

func newServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}
