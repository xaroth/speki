package library

import (
	"context"
	"errors"
	"sync"
	"time"
)

// notices is the channel instances tell each other about changed vaults on.
type notices interface {
	Subscribe(ctx context.Context, f func(repoID int64, commit string)) error
}

// FollowNotices syncs a vault when another instance says its branch moved, so a write on one instance
// shows up on the others within seconds instead of at the next poll. Notices for one vault are
// coalesced: at most one sync runs per vault, and notices that arrive meanwhile cause one more. The
// subscription is retried with backoff while n is unreachable; polling covers the gap. It returns when
// ctx ends and the syncs it started are done.
func (l *Library) FollowNotices(ctx context.Context, n notices) {
	var c coalescer
	defer c.wg.Wait()
	onNotice := func(repoID int64, commit string) {
		v, ok := l.Vault(repoID)
		if !ok || v.Index().Commit() == commit {
			return
		}
		c.notify(ctx, repoID, func() {
			err := l.syncAfterNotice(ctx, v)
			if err != nil && ctx.Err() == nil {
				l.logger.WarnContext(ctx, "sync after a change notice failed", "vault", v.Info().Name, "err", err)
			}
		})
	}

	wait, failing := time.Second, false
	for ctx.Err() == nil {
		start := time.Now()
		err := n.Subscribe(ctx, onNotice)
		if err == nil {
			return
		}
		if time.Since(start) > time.Minute { // it worked for a while: a new outage
			wait, failing = time.Second, false
		}
		if !failing { // once per outage
			l.logger.WarnContext(ctx, "change notices unavailable; retrying", "err", err)
			failing = true
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
		wait = min(wait*2, time.Minute)
	}
}

// coalescer runs at most one sync per vault at a time.
type coalescer struct {
	wg      sync.WaitGroup
	mu      sync.Mutex
	pending map[int64]bool // vaults with a sync running: true when a notice came during it
}

// notify calls run for repoID, or, when a sync for it is running, has that one run once more. Nothing
// runs again once ctx ends.
func (c *coalescer) notify(ctx context.Context, repoID int64, run func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, running := c.pending[repoID]; running {
		c.pending[repoID] = true
		return
	}
	if c.pending == nil {
		c.pending = make(map[int64]bool)
	}
	c.pending[repoID] = false
	c.wg.Go(func() {
		for {
			run()
			c.mu.Lock()
			if !c.pending[repoID] || ctx.Err() != nil {
				delete(c.pending, repoID)
				c.mu.Unlock()
				return
			}
			c.pending[repoID] = false
			c.mu.Unlock()
		}
	})
}

// syncAfterNotice syncs a vault another instance changed, as a poll would: not while the vault is
// backing off after failures, and a failure is recorded the same way, for the status page and backoff.
func (l *Library) syncAfterNotice(ctx context.Context, v *Vault) error {
	if v.Status().State != StateServing || !v.due() {
		return nil
	}
	err := l.Sync(ctx, v)
	if err != nil && !errors.Is(err, errRemoved) {
		v.recordFailure(err)
	}
	return err
}
