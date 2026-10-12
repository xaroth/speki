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
	"github.com/eve-online-tools/speki/store"
	"github.com/eve-online-tools/speki/telemetry"
	"github.com/eve-online-tools/speki/vault"
)

// shutdownTimeout is how long a stopping service takes at most: background work, then running requests.
// It's under Kubernetes' default grace period of 30 seconds.
const shutdownTimeout = 25 * time.Second

// Service is one running speki.
type Service struct {
	cfg      Config
	logger   *slog.Logger
	github   *gh.Client
	library  *library.Library
	sandbox  *index.Sandbox
	store    sharedStore
	auth     *oauth.Server
	mcp      *mcpserver.Server
	ready    atomic.Bool // read by /readyz; set through markReady and markStopping
	readyMu  sync.Mutex
	stopping bool // guarded by readyMu: once set, the service never turns ready again
	vaultsMu sync.Mutex
	vaults   *vault.Registry // nil until the allowed accounts are known
	ran      atomic.Bool     // Run was called: it closes the components, so a service runs once

	// started is closed once both listeners are bound; publicAddr and internalAddr are set by then.
	started      chan struct{}
	publicAddr   net.Addr
	internalAddr net.Addr
	// discovered is closed after the first discovery round, successful or not.
	discovered chan struct{}
	// shutdownTimeout is a field so tests can shorten it.
	shutdownTimeout time.Duration
}

func (s *Service) closeComponents() error {
	s.library.Close()
	return errors.Join(s.sandbox.Close(), s.store.Close())
}

// markReady marks the service ready, unless it's already stopping.
func (s *Service) markReady() {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	if !s.stopping {
		s.ready.Store(true)
	}
}

func (s *Service) markStopping() {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	s.stopping = true
	s.ready.Store(false)
}

// New checks cfg, fills in defaults and builds every component. Nothing runs yet.
func New(cfg Config, logger *slog.Logger) (*Service, error) {
	cfg = cfg.withDefaults()
	err := cfg.validate()
	if err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}

	github, err := gh.New(gh.Config{
		AppID:      cfg.GitHub.AppID,
		PrivateKey: []byte(cfg.GitHub.PrivateKey),
		APIURL:     cfg.GitHub.APIURL,
		Logger:     logger,
	})
	if err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}

	lib := library.New(
		github,
		library.Options{
			DataDir: cfg.Sync.DataDir,
			GitURL:  cfg.GitHub.GitURL,
			Tokens:  func(id int64) gitsync.TokenSource { return github.InstallationTokens(id) },
		},
		logger,
	)

	keys, err := cfg.Auth.keyring()
	if err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}
	shared, err := newStore(cfg.Redis, logger)
	if err != nil {
		return nil, err
	}
	resolver := access.NewResolver(
		github,
		lib.Vaults,
		cfg.Access.RequiredOrgs,
		shared,
	)
	auth := oauth.New(
		oauth.Config{
			Issuer:              cfg.PublicURL,
			Title:               cfg.Instance.Title,
			ClientMetadataHosts: cfg.Auth.ClientMetadataHosts,
			GitHub: gh.OAuthConfig{
				ClientID:     cfg.GitHub.ClientID,
				ClientSecret: cfg.GitHub.ClientSecret,
				URL:          cfg.GitHub.OAuthURL,
			},
			CheckUser: func(ctx context.Context, token string) error {
				err := resolver.CheckMember(ctx, token)
				if errors.Is(err, access.ErrNotMember) {
					return &oauth.Refusal{Reason: err.Error()}
				}
				return err
			},
		},
		keys,
		github,
		shared,
		logger,
	)

	sandbox, err := index.NewSandbox()
	if err != nil {
		_ = shared.Close()
		return nil, err
	}
	kb := knowledge.New(
		libraryVaults{lib},
		github,
		sandbox,
		shared,
		logger,
	)
	mcp := mcpserver.New(
		oauthUsers{auth},
		resolver,
		kb,
		mcpserver.Identity{
			Name:        cfg.Instance.Name,
			Title:       cfg.Instance.Title,
			Description: cfg.Instance.Description,
		},
		cfg.Version,
		logger,
	)

	return &Service{
		cfg:        cfg,
		logger:     logger,
		github:     github,
		library:    lib,
		sandbox:    sandbox,
		store:      shared,
		auth:       auth,
		mcp:        mcp,
		started:    make(chan struct{}),
		discovered: make(chan struct{}),

		shutdownTimeout: shutdownTimeout,
	}, nil
}

// Run serves vaults already on disk straight away, keeps the vaults in step with GitHub, and serves HTTP.
// It blocks until ctx ends and shutdown is done. A service runs once: a second Run returns an error.
func (s *Service) Run(ctx context.Context) error {
	if !s.ran.CompareAndSwap(false, true) {
		return errors.New("the service has already run")
	}
	stopTracing, err := telemetry.StartTracing(ctx, s.cfg.Telemetry.Tracing, s.cfg.Version)
	if err != nil {
		return err
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err := stopTracing(flushCtx)
		if err != nil {
			s.logger.WarnContext(ctx, "flushing traces failed", "err", err)
		}
	}()

	var lc net.ListenConfig
	public, err := lc.Listen(ctx, "tcp", s.cfg.Listen)
	if err != nil {
		_ = s.closeComponents()
		return fmt.Errorf("listen: %w", err)
	}
	internal, err := lc.Listen(ctx, "tcp", s.cfg.MetricsListen)
	if err != nil {
		_ = public.Close()
		_ = s.closeComponents()
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
	s.logger.InfoContext(ctx, "speki started", "listen", s.publicAddr.String(), "metrics_listen", s.internalAddr.String())

	err = s.library.Restore(gctx)
	if err != nil {
		s.logger.ErrorContext(ctx, "restoring vaults from disk failed", "err", err)
	}

	// Background work stops before the servers do, so in-flight requests still see open vaults.
	work, stopWork := context.WithCancel(gctx)
	var background sync.WaitGroup
	background.Go(func() { s.discover(work) })
	background.Go(func() { s.library.Run(work, s.cfg.Sync.PollInterval) })
	background.Go(func() { s.library.FollowNotices(work, s.store) })

	g.Go(func() error {
		<-gctx.Done()
		s.markStopping()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.shutdownTimeout)
		defer cancel()

		stopWork()
		stopped := make(chan struct{})
		go func() {
			background.Wait()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-shutdownCtx.Done(): // the process is about to end; the rest of the work goes with it
			s.logger.WarnContext(ctx, "background work didn't stop in time")
		}

		// Running requests get the rest of the time. When it runs out, their connections are cut:
		// a routine stop, not a failure.
		for _, srv := range []*http.Server{publicSrv, internalSrv} {
			err := srv.Shutdown(shutdownCtx)
			if errors.Is(err, context.DeadlineExceeded) {
				s.logger.WarnContext(ctx, "requests still running at shutdown; closing their connections")
				err = srv.Close()
			}
			if err != nil {
				s.logger.WarnContext(ctx, "stopping a server failed", "err", err)
			}
		}
		return s.closeComponents()
	})

	err = g.Wait()
	s.logger.InfoContext(ctx, "speki stopped")
	return err
}

// discover finds the allowed accounts, then keeps the vault registry and the library up to date. The
// service is ready after the first round, successful or not: vaults on disk are served either way.
func (s *Service) discover(ctx context.Context) {
	firstRound := sync.OnceFunc(func() {
		s.markReady()
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
			s.logger.WarnContext(ctx, "vault discovery incomplete", "err", err)
		}
		if !changes.Empty() {
			s.logger.InfoContext(ctx, "vaults changed",
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
		s.logger.WarnContext(ctx, "can't find the app's owner; retrying", "err", err, "in", wait)
		giveUp()
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(wait):
		}
		wait = min(wait*2, 5*time.Minute)
	}
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
	return telemetry.RequestID(telemetry.Trace(mux))
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
	return mux
}

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

// oauthUsers hands mcpserver the signed-in user as access knows them.
type oauthUsers struct{ *oauth.Server }

func (oauthUsers) User(ctx context.Context) (access.User, bool) {
	id, ok := oauth.IdentityFrom(ctx)
	return access.User{
		ID:     id.UserID,
		Login:  id.Login,
		Token:  id.GitHubToken,
		Client: id.Client,
		Write:  id.Write,
	}, ok
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
	err := l.lib.SyncChanges(ctx, v) // knowledge syncs for writes, which mustn't wait for a rebuild
	if errors.Is(err, library.ErrRebuildNeeded) {
		return knowledge.ErrRebuilding
	}
	return err
}

// sharedStore is the state instances share, and the channel they tell each other about changes on.
type sharedStore interface {
	SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Release(ctx context.Context, key string, value []byte) error
	Publish(ctx context.Context, repoID int64, commit string) error
	Subscribe(ctx context.Context, f func(repoID int64, commit string)) error
	Close() error
}

func newStore(cfg Redis, logger *slog.Logger) (sharedStore, error) {
	if cfg.URL == "" {
		return store.NewMemory(), nil
	}
	r, err := store.NewRedis(context.Background(), cfg.URL, logger)
	if err != nil {
		return nil, fmt.Errorf("configuration: redis.url: %w", err)
	}
	return r, nil
}
