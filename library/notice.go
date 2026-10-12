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
	var syncs sync.WaitGroup
	defer syncs.Wait()
	var mu sync.Mutex
	state := map[int64]string{} // "running", or "again" when a notice came during the sync

	onNotice := func(repoID int64, commit string) {
		v, ok := l.Vault(repoID)
		if !ok || v.Index().Commit() == commit {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if state[repoID] != "" {
			state[repoID] = "again"
			return
		}
		state[repoID] = "running"
		syncs.Go(func() {
			for {
				err := l.syncAfterNotice(ctx, v)
				if err != nil && ctx.Err() == nil {
					l.logger.WarnContext(ctx, "sync after a change notice failed", "vault", v.Info().Name, "err", err)
				}
				mu.Lock()
				if state[repoID] != "again" || ctx.Err() != nil {
					delete(state, repoID)
					mu.Unlock()
					return
				}
				state[repoID] = "running"
				mu.Unlock()
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
