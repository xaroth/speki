// Package service is speki without the process around it: it builds every component from a Config and
// runs them until its context ends.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"

	"github.com/eve-online-tools/speki/access"
	"github.com/eve-online-tools/speki/gh"
	"github.com/eve-online-tools/speki/gitsync"
	"github.com/eve-online-tools/speki/index"
	"github.com/eve-online-tools/speki/knowledge"
	"github.com/eve-online-tools/speki/library"
	"github.com/eve-online-tools/speki/mcpserver"
	"github.com/eve-online-tools/speki/oauth"
	"github.com/eve-online-tools/speki/seal"
	"github.com/eve-online-tools/speki/store"
	"github.com/eve-online-tools/speki/telemetry"
	"github.com/eve-online-tools/speki/vault"
)

// shutdownTimeout is how long running requests get to finish once the service stops.
const shutdownTimeout = 25 * time.Second

// Service is one running speki.
type Service struct {
	cfg      Config
	logger   *slog.Logger
	metrics  *prometheus.Registry
	github   *gh.Client
	library  *library.Library
	sandbox  *index.Sandbox
	store    sharedStore
	auth     *oauth.Server
	mcp      *mcpserver.Server
	ready    atomic.Bool
	vaultsMu sync.Mutex
	vaults   *vault.Registry // nil until the allowed accounts are known

	// started is closed once both listeners are bound; publicAddr and internalAddr are set by then.
	started      chan struct{}
	publicAddr   net.Addr
	internalAddr net.Addr
	// discovered is closed after the first discovery round, successful or not.
	discovered chan struct{}
}

// New checks cfg, fills in defaults and builds every component. Nothing runs yet.
func New(cfg Config, logger *slog.Logger) (*Service, error) {
	cfg = cfg.withDefaults()
	err := cfg.validate()
	if err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}

	metrics := prometheus.NewRegistry()
	metrics.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	github, err := gh.New(gh.Config{
		AppID:      cfg.GitHub.AppID,
		PrivateKey: []byte(cfg.GitHub.PrivateKey.Reveal()),
		APIURL:     cfg.GitHub.APIURL,
	}, metrics)
	if err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}

	lib := library.New(github, library.Options{
		DataDir: cfg.Sync.DataDir,
		GitURL:  cfg.GitHub.GitURL,
		Tokens:  func(id int64) gitsync.TokenSource { return github.InstallationTokens(id) },
	}, logger)
	metrics.MustRegister(lib.Collector())

	keys, err := seal.ParseKeyring([]byte(cfg.Auth.Keys.Reveal()))
	if err != nil {
		return nil, fmt.Errorf("configuration: auth.keys: %w", err)
	}
	shared, err := newStore(cfg.Redis, logger)
	if err != nil {
		return nil, err
	}
	resolver := access.NewResolver(github, lib.Vaults, cfg.Access.RequiredOrgs)
	auth := oauth.New(oauth.Config{
		Issuer:              cfg.PublicURL,
		ClientMetadataHosts: cfg.Auth.ClientMetadataHosts,
		GitHub: gh.OAuthConfig{
			ClientID:     cfg.GitHub.ClientID,
			ClientSecret: cfg.GitHub.ClientSecret.Reveal(),
			URL:          cfg.GitHub.OAuthURL,
		},
		CheckUser: resolver.CheckMember,
	}, keys, github, shared, logger)
	metrics.MustRegister(auth.Collector())

	sandbox, err := index.NewSandbox()
	if err != nil {
		return nil, err
	}
	kb := knowledge.New(libraryVaults{lib}, github, sandbox, shared, logger)
	mcp := mcpserver.New(auth, resolver, kb, cfg.Version, logger)
	metrics.MustRegister(mcp.Collector())

	return &Service{
		cfg:        cfg,
		logger:     logger,
		metrics:    metrics,
		github:     github,
		library:    lib,
		sandbox:    sandbox,
		store:      shared,
		auth:       auth,
		mcp:        mcp,
		started:    make(chan struct{}),
		discovered: make(chan struct{}),
	}, nil
}

// Run serves vaults already on disk straight away, keeps the vaults in step with GitHub, and serves HTTP.
// It blocks until ctx ends and shutdown is done.
func (s *Service) Run(ctx context.Context) error {
	stopTracing, err := telemetry.StartTracing(ctx, s.cfg.Telemetry.Tracing, s.cfg.Version)
	if err != nil {
		return err
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err := stopTracing(flushCtx)
		if err != nil {
			s.logger.Warn("flushing traces failed", "err", err)
		}
	}()

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
	close(s.started)
	s.logger.Info("speki started", "listen", s.publicAddr.String(), "metrics_listen", s.internalAddr.String())

	err = s.library.Restore(gctx)
	if err != nil {
		s.logger.Error("restoring vaults from disk failed", "err", err)
	}

	// Background work stops before the servers do, so in-flight requests still see open vaults.
	work, stopWork := context.WithCancel(gctx)
	var background sync.WaitGroup
	background.Go(func() { s.discover(work) })
	background.Go(func() { s.library.Run(work, s.cfg.Sync.PollInterval) })
	background.Go(func() { s.followNotices(work) })

	g.Go(func() error {
		<-gctx.Done()
		s.ready.Store(false)
		stopWork()
		background.Wait()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		err := errors.Join(publicSrv.Shutdown(shutdownCtx), internalSrv.Shutdown(shutdownCtx))
		s.library.Close()
		return errors.Join(err, s.sandbox.Close(), s.store.Close())
	})

	err = g.Wait()
	s.logger.Info("speki stopped")
	return err
}

// discover finds the allowed accounts, then keeps the vault registry and the library up to date. The
// service is ready after the first round, successful or not: vaults on disk are served either way.
func (s *Service) discover(ctx context.Context) {
	firstRound := sync.OnceFunc(func() {
		s.ready.Store(true)
		close(s.discovered)
	})
	defer firstRound() // also when stopped before the first round

	allowed, ok := s.allowedAccounts(ctx, firstRound)
	if !ok {
		return
	}
	registry := vault.NewRegistry(s.github, allowed, s.logger)
	s.vaultsMu.Lock()
	s.vaults = registry
	s.vaultsMu.Unlock()

	registry.Run(ctx, s.cfg.Sync.DiscoveryInterval, func(changes vault.Changes, err error) {
		if err != nil {
			s.logger.Warn("vault discovery incomplete", "err", err)
		}
		if !changes.Empty() {
			s.logger.Info("vaults changed",
				"added", len(changes.Added), "changed", len(changes.Changed), "removed", len(changes.Removed))
			s.library.Update(ctx, changes)
		}
		if err == nil {
			// A complete answer: drop vaults restored from disk that GitHub no longer has.
			s.library.Reconcile(ctx, registry.Vaults())
		}
		firstRound()
	})
}

// allowedAccounts returns the configured accounts, or asks GitHub for the app's owner, retrying until it
// answers. While it can't, the service is marked ready anyway (vaults on disk can be served).
func (s *Service) allowedAccounts(ctx context.Context, giveUp func()) ([]int64, bool) {
	if len(s.cfg.GitHub.AllowedAccounts) > 0 {
		return s.cfg.GitHub.AllowedAccounts, true
	}
	wait := time.Second
	for {
		owner, err := s.github.AppOwner(ctx)
		if err == nil {
			return []int64{owner}, true
		}
		s.logger.Warn("can't find the app's owner; retrying", "err", err, "in", wait)
		giveUp()
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(wait):
		}
		wait = min(wait*2, 5*time.Minute)
	}
}

// followNotices syncs a vault when another instance says its branch moved, so a write on one instance
// shows up on the others within seconds instead of at the next poll. The store is retried while it's
// unreachable; polling covers the gap.
func (s *Service) followNotices(ctx context.Context) {
	for ctx.Err() == nil {
		err := s.store.Subscribe(ctx, func(repoID int64, commit string) {
			v, ok := s.library.Vault(repoID)
			if !ok || v.Index().Commit() == commit {
				return
			}
			err := s.library.Sync(ctx, v)
			if err != nil {
				s.logger.Warn("sync after a change notice failed", "vault", v.Info().Name, "err", err)
			}
		})
		if err == nil {
			return
		}
		s.logger.Warn("change notices unavailable; retrying", "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}

// publicHandler serves what MCP clients and browsers need: MCP, sign-in and health checks.
func (s *Service) publicHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/mcp", s.mcp.Handler())
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
	return telemetry.RequestID(telemetry.Trace(mux))
}

// internalHandler serves metrics and the status page. It runs on its own listener and is never
// reachable through the public one.
func (s *Service) internalHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.metrics, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /status", s.status)
	return mux
}

// statusPage is what /status returns.
type statusPage struct {
	Ready    bool             `json:"ready"`
	Vaults   []library.Status `json:"vaults"`
	Problems []vault.Problem  `json:"problems"`
}

func (s *Service) status(w http.ResponseWriter, _ *http.Request) {
	page := statusPage{Ready: s.ready.Load(), Vaults: s.library.Status(), Problems: []vault.Problem{}}
	if page.Vaults == nil {
		page.Vaults = []library.Status{}
	}
	s.vaultsMu.Lock()
	if s.vaults != nil {
		page.Problems = append(page.Problems, s.vaults.Problems()...)
	}
	s.vaultsMu.Unlock()
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

func newServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// libraryVaults hands knowledge the library's vaults.
type libraryVaults struct{ lib *library.Library }

func (l libraryVaults) Copy(repoID int64) (knowledge.Copy, bool) {
	v, ok := l.lib.Vault(repoID)
	if !ok {
		return nil, false // not a typed nil inside the interface
	}
	return v, true
}

func (l libraryVaults) Sync(ctx context.Context, repoID int64) error {
	v, ok := l.lib.Vault(repoID)
	if !ok {
		return fmt.Errorf("vault %d isn't served", repoID)
	}
	return l.lib.Sync(ctx, v)
}

// sharedStore is the state instances share, and the channel they tell each other about changes on.
type sharedStore interface {
	SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Delete(ctx context.Context, key string) error
	Publish(ctx context.Context, repoID int64, commit string) error
	Subscribe(ctx context.Context, f func(repoID int64, commit string)) error
	Close() error
}

// newStore returns Redis when it's configured, otherwise memory for a single instance.
func newStore(cfg Redis, logger *slog.Logger) (sharedStore, error) {
	if cfg.URL == "" {
		return store.NewMemory(), nil
	}
	r, err := store.NewRedis(context.Background(), cfg.URL.Reveal(), logger)
	if err != nil {
		return nil, fmt.Errorf("configuration: redis.url: %w", err)
	}
	return r, nil
}
