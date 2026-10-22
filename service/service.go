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
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/xaroth/speki/auth/access"
	"github.com/xaroth/speki/auth/oauth"
	"github.com/xaroth/speki/gh"
	"github.com/xaroth/speki/knowledge"
	"github.com/xaroth/speki/library"
	"github.com/xaroth/speki/library/gitsync"
	"github.com/xaroth/speki/library/index"
	"github.com/xaroth/speki/mcpserver"
	"github.com/xaroth/speki/telemetry"
	"github.com/xaroth/speki/vault"
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
	polled   polledAPI // github, with the reads polling makes shared between instances
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

	keys, err := cfg.Auth.keyring()
	if err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}
	shared, err := newStore(cfg.Redis, logger)
	if err != nil {
		return nil, err
	}
	polled := newPolledGitHub(cfg, github, shared)

	lib := library.New(
		polled,
		library.Options{
			DataDir: cfg.Sync.DataDir,
			GitURL:  cfg.GitHub.GitURL,
			Tokens:  func(id int64) gitsync.TokenSource { return github.InstallationTokens(id) },
		},
		logger,
	)

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
		polled:     polled,
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

	g, gctx := errgroup.WithContext(ctx)
	servers, err := s.listenAndServe(ctx, g)
	if err != nil {
		return err
	}
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
		return s.shutdown(ctx, stopWork, servers)
	})

	err = g.Wait()
	s.logger.InfoContext(ctx, "speki stopped")
	return err
}

// listenAndServe opens the public and internal listeners and serves them in g. When a listener can't be
// opened, it closes the components, since Run won't get to.
func (s *Service) listenAndServe(ctx context.Context, g *errgroup.Group) ([]*http.Server, error) {
	var lc net.ListenConfig
	public, err := lc.Listen(ctx, "tcp", s.cfg.Listen)
	if err != nil {
		_ = s.closeComponents()
		return nil, fmt.Errorf("listen: %w", err)
	}
	internal, err := lc.Listen(ctx, "tcp", s.cfg.MetricsListen)
	if err != nil {
		_ = public.Close()
		_ = s.closeComponents()
		return nil, fmt.Errorf("metrics_listen: %w", err)
	}
	s.publicAddr = public.Addr()
	s.internalAddr = internal.Addr()

	publicSrv := newServer(s.publicHandler())
	internalSrv := newServer(s.internalHandler())
	g.Go(func() error { return serve(publicSrv, public) })
	g.Go(func() error { return serve(internalSrv, internal) })
	return []*http.Server{publicSrv, internalSrv}, nil
}

// shutdown stops the service once Run's context ends: it drains the servers, waits for handlers, stops
// background work, and closes the components if nothing still uses them.
func (s *Service) shutdown(ctx context.Context, stopWork context.CancelFunc, servers []*http.Server) error {
	s.markStopping()
	stopped := context.WithoutCancel(ctx)

	// Running requests finish first, with vaults still open and polled. When their time runs out,
	// their connections are cut: a routine stop, not a failure.
	drainCtx, cancel := context.WithTimeout(stopped, s.drainTimeout)
	defer cancel()
	for _, srv := range servers {
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
