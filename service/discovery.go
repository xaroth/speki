package service

import (
	"context"
	"sync"

	"github.com/xaroth/speki/vault"
)

// discover keeps the vault registry and the library up to date. The service is ready after the first
// round, successful or not: vaults on disk are served either way.
func (s *Service) discover(ctx context.Context) {
	firstRound := sync.OnceFunc(func() {
		s.markReady()
		close(s.discovered)
	})
	defer firstRound() // also when stopped before the first round

	s.vaults.Run(ctx, s.cfg.Sync.DiscoveryInterval, func(changes vault.Changes, err error) {
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
			s.library.Reconcile(ctx, s.vaults.Vaults())
		}
		firstRound()
	})
}
