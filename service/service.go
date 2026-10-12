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

	"github.com/eve-online-tools/speki/auth/access"
	"github.com/eve-online-tools/speki/auth/oauth"
	"github.com/eve-online-tools/speki/gh"
	"github.com/eve-online-tools/speki/gitsync"
	"github.com/eve-online-tools/speki/index"
	"github.com/eve-online-tools/speki/knowledge"
	"github.com/eve-online-tools/speki/library"
	"github.com/eve-online-tools/speki/mcpserver"
	"github.com/eve-online-tools/speki/store"
	"github.com/eve-online-tools/speki/telemetry"
	"github.com/eve-online-tools/speki/vault"
)

// A stopping service drains running requests, then stops background work, each on its own budget, so
// work that ignores stop can't take time from requests. Together they stay under Kubernetes' default grace
// period of 30 seconds.
const (
	// drainTimeout is how long running requests get to finish before their connections are cut.
	drainTimeout = 15 * time.Second
	// handlerWait is how long handlers still running after their connections were cut get to return.
	// Components stay open until they have.
	handlerWait = 3 * time.Second
	// backgroundTimeout is how long polling, notices and the work after commits get to stop.
	backgroundTimeout = 5 * time.Second
)

// Service is one running speki.
type Service struct {
	cfg      Config
	logger   *slog.Logger
	github   *gh.Client
	library  *library.Library
	sandbox  *index.Sandbox
	store    sharedStore
	auth     *oauth.Server
	resolver *access.Resolver
	kb       *knowledge.Base
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
	// handlers counts running HTTP handlers, which may outlive their connections.
	handlers       sync.WaitGroup
	handlersMu     sync.Mutex
	handlersClosed bool // set once shutdown waits for handlers
	// background is the work Run starts besides serving: discovery, polling and notices.
	background sync.WaitGroup
	// Fields so tests can shorten them.
	drainTimeout      time.Duration
	handlerWait       time.Duration
	backgroundTimeout time.Duration
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
	s.resolver.Stop() // requests waiting for vaults being built mustn't hold up the drain
}

// New checks cfg, fills in defaults and builds every component. Nothing runs yet.
func New(cfg Config, logger *slog.Logger) (*Service, error) {
	cfg = cfg.withDefaults()
	err := cfg.validate()
	if err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}
	index.SetMemoryLimits(cfg.SQLite.MemoryLimit, cfg.SQLite.QueryMemory)

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
		lib,
		cfg.Access.RequiredOrgs,
		shared,
		logger,
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
		resolver:   resolver,
		kb:         kb,
		mcp:        mcp,
		started:    make(chan struct{}),
		discovered: make(chan struct{}),

		drainTimeout:      drainTimeout,
		handlerWait:       handlerWait,
		backgroundTimeout: backgroundTimeout,
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
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
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

	work, stopWork := context.WithCancel(gctx)
	s.background.Go(func() { s.discover(work) })
	s.background.Go(func() { s.library.Run(work, s.cfg.Sync.PollInterval) })
	s.background.Go(func() { s.library.FollowNotices(work, s.store) })

	g.Go(func() error {
		<-gctx.Done()
		s.markStopping()
		stopped := context.WithoutCancel(ctx)

		// Running requests finish first, with vaults still open and polled. When their time runs out,
		// their connections are cut: a routine stop, not a failure.
		drainCtx, cancel := context.WithTimeout(stopped, s.drainTimeout)
		defer cancel()
		for _, srv := range []*http.Server{publicSrv, internalSrv} {
			err := srv.Shutdown(drainCtx)
			if errors.Is(err, context.DeadlineExceeded) {
				s.logger.WarnContext(ctx, "requests still running at shutdown; closing their connections")
				err = srv.Close()
			}
			if err != nil {
				s.logger.WarnContext(ctx, "stopping a server failed", "err", err)
			}
		}
		// Close doesn't wait for handlers, and closing components under one would fail its request.
		hctx, cancel := context.WithTimeout(stopped, s.handlerWait)
		defer cancel()
		s.handlersMu.Lock()
		s.handlersClosed = true // no Add after this, so Wait can't race one
		s.handlersMu.Unlock()
		handlersDone := waitFor(hctx, &s.handlers)

		bgCtx, cancel := context.WithTimeout(stopped, s.backgroundTimeout)
		defer cancel()
		stopWork()
		workDone := true
		err := s.kb.Stop(bgCtx)
		if err != nil {
			s.logger.WarnContext(ctx, "work after commits didn't finish in time")
			workDone = false
		}
		if !waitFor(bgCtx, &s.background) {
			s.logger.WarnContext(ctx, "background work didn't stop in time")
			workDone = false
		}

		// The process is about to end; whatever still runs goes with it, but not with components closed
		// under it.
		if !handlersDone {
			s.logger.WarnContext(ctx, "requests still running after their connections were cut; leaving components open")
			return nil
		}
		if !workDone {
			s.logger.WarnContext(ctx, "leaving components open for the background work still running")
			return nil
		}
		return s.closeComponents()
	})

	err = g.Wait()
	s.logger.InfoContext(ctx, "speki stopped")
	return err
}

// waitFor waits for wg until ctx ends, and reports whether it finished.
func waitFor(ctx context.Context, wg *sync.WaitGroup) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

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
		Problems: []vault.Problem{},
		Warnings: []string{},
	}
	if s.cfg.Redis.URL == "" {
		page.Store = "memory"
		page.Warnings = append(page.Warnings, memoryStoreWarning)
	}
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
	GetDel(ctx context.Context, key string) ([]byte, bool, error)
	Release(ctx context.Context, key string, value []byte) error
	Publish(ctx context.Context, repoID int64, commit string) error
	Subscribe(ctx context.Context, f func(repoID int64, commit string)) error
	Close() error
}

// memoryStoreWarning is logged at startup and shown on the status page. Instances with a memory store
// each keep their own refresh locks and results, so two of them refreshing one token sign the user out.
const memoryStoreWarning = "no redis.url: sign-in state is kept in memory, so run exactly one instance; several instances need redis.url"

func newStore(cfg Redis, logger *slog.Logger) (sharedStore, error) {
	if cfg.URL == "" {
		logger.Warn(memoryStoreWarning)
		return store.NewMemory(), nil
	}
	r, err := store.NewRedis(context.Background(), cfg.URL, logger)
	if err != nil {
		return nil, fmt.Errorf("configuration: redis.url: %w", err)
	}
	return r, nil
}
