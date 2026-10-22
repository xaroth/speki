package service

import (
	"context"
	"sync"
	"time"

	"github.com/xaroth/speki/vault"
)

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
	registry := vault.NewRegistry(s.polled, allowed, s.logger)
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
