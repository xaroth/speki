package library

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

// Run checks every vault against GitHub every interval until ctx ends: changed vaults are synced, and
// failed ones are retried with backoff.
func (l *Library) Run(ctx context.Context, interval time.Duration) {
	l.publish()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		l.Poll(ctx)
	}
}

// Poll runs one round of checks, four vaults at a time. Work it starts in the background lives on ctx,
// not on the round.
func (l *Library) Poll(ctx context.Context) {
	var g errgroup.Group
	g.SetLimit(4)
	for _, v := range l.all() {
		g.Go(func() error {
			l.check(ctx, v)
			return nil
		})
	}
	_ = g.Wait()
	l.publish()
}

func (l *Library) check(ctx context.Context, v *Vault) {
	st := v.Status()
	switch st.State {
	case StateBuilding:
		return
	case StateFailed:
		if v.due() {
			l.inBackground(ctx, v, l.initialise) // a clone can take minutes: not in the round
		}
		return
	}
	if !v.due() {
		return // the last sync failed: back off
	}

	info := v.Info()
	v.mu.Lock()
	etag := v.etag
	v.mu.Unlock()
	head, err := l.github.BranchHead(ctx, info.InstallationID, info.Name, info.DefaultBranch, etag)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			l.logger.WarnContext(ctx, "checking vault failed", "vault", info.Name, "err", err)
			v.recordFailure(err) // on the status page, with backoff, like a failed sync
		}
		return
	}

	v.mu.Lock()
	v.etag = head.ETag
	if head.SHA != "" {
		v.remote = head.SHA
	}
	remote := v.remote
	v.mu.Unlock()

	if remote != "" && remote == v.index.Commit() && !v.broken.Load() {
		v.mu.Lock()
		v.status.Fresh = v.now()
		v.recordLocked(nil) // current again: an earlier failed check no longer counts
		v.mu.Unlock()
		l.inBackground(ctx, v, l.compact) // writes add packs without the poller ever syncing them
		return
	}
	// Changes are applied in the round. Anything bigger (a rebuild, a fresh clone) runs in the background,
	// so one vault can't hold up the others; and a vault busy with one is checked again next round.
	if !v.syncMu.TryLock() {
		return
	}
	err = func() error {
		defer v.syncMu.Unlock()
		return l.syncLocked(ctx, v, true)
	}()
	if errors.Is(err, ErrRebuildNeeded) {
		l.inBackground(ctx, v, l.rebuild)
		return
	}
	if err != nil && !errors.Is(err, errRemoved) {
		l.logger.WarnContext(ctx, "syncing vault failed", "vault", info.Name, "err", err)
		v.recordFailure(err)
	}
}

// inBackground runs work on a vault outside the poll round, a few vaults at a time, once at a time per
// vault.
func (l *Library) inBackground(ctx context.Context, v *Vault, work func(context.Context, *Vault)) {
	if l.closed.Load() || !v.scheduled.CompareAndSwap(false, true) {
		return
	}
	l.background.Go(func() {
		defer v.scheduled.Store(false)
		work(ctx, v)
	})
}

// rebuild brings a vault up to date the slow way: a full rebuild, or a fresh clone first.
func (l *Library) rebuild(ctx context.Context, v *Vault) {
	if l.opening.Acquire(ctx, 1) != nil {
		return
	}
	defer l.opening.Release(1)
	if v.syncMu.LockContext(ctx) != nil {
		return
	}
	defer v.syncMu.Unlock()
	err := l.syncLocked(ctx, v, false)
	if err != nil && !errors.Is(err, errRemoved) && ctx.Err() == nil {
		l.logger.WarnContext(ctx, "rebuilding vault failed", "vault", v.Info().Name, "err", err)
		v.recordFailure(err)
	}
}

// stallContext returns a context that's cancelled when touch hasn't been called for timeout.
func stallContext(ctx context.Context, timeout time.Duration) (context.Context, func(), func()) {
	ctx, cancel := context.WithCancel(ctx)
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	touch := func() { last.Store(time.Now().UnixNano()) }
	done := make(chan struct{})
	var watcher sync.WaitGroup
	watcher.Go(func() {
		ticker := time.NewTicker(max(min(timeout/4, 10*time.Second), time.Millisecond))
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if now.Sub(time.Unix(0, last.Load())) > timeout {
					cancel()
					return
				}
			}
		}
	})
	var once sync.Once
	stop := func() {
		once.Do(func() {
			close(done)
			cancel()
			watcher.Wait()
		})
	}
	return ctx, touch, stop
}

// progressWriter calls touch whenever git reports progress.
func progressWriter(touch func()) io.Writer {
	return writerFunc(func(p []byte) (int, error) {
		touch()
		return len(p), nil
	})
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// SyncAfterNotice syncs a vault another instance changed, as a poll would: not while the vault is
// backing off after failures, and a failure is recorded the same way, for the status page and backoff.
func (l *Library) SyncAfterNotice(ctx context.Context, v *Vault) error {
	if v.Status().State != StateServing || !v.due() {
		return nil
	}
	err := l.Sync(ctx, v)
	if err != nil && !errors.Is(err, errRemoved) {
		v.recordFailure(err)
	}
	return err
}

// compact clones a vault's copy afresh when fetches have piled up too many pack files. A sync does this
// too, but on the instance that writes to a vault the poller never needs to sync: each write's own sync
// already brought the index up to date.
func (l *Library) compact(ctx context.Context, v *Vault) {
	if !v.syncMu.TryLock() {
		return // busy: the next round looks again
	}
	defer v.syncMu.Unlock()
	if v.removed || v.mirror == nil || v.index == nil {
		return
	}
	l.compactLocked(ctx, v)
}
