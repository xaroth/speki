package library

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

// Run checks every vault against GitHub every interval until ctx ends: changed vaults are synced, and
// failed ones are retried with backoff.
func (l *Library) Run(ctx context.Context, interval time.Duration) {
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

// Poll runs one round of checks, four vaults at a time.
func (l *Library) Poll(ctx context.Context) {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	for _, v := range l.all() {
		g.Go(func() error {
			l.check(gctx, v)
			return nil
		})
	}
	_ = g.Wait()
}

func (l *Library) check(ctx context.Context, v *Vault) {
	st := v.Status()
	switch st.State {
	case StateBuilding:
		return
	case StateFailed:
		if v.due() {
			l.initialise(ctx, v)
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
			l.logger.Warn("checking vault failed", "vault", info.Name, "err", err)
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

	if remote != "" && remote == v.index.Commit() {
		v.mu.Lock()
		v.status.Fresh = time.Now()
		v.mu.Unlock()
		return
	}
	err = l.Sync(ctx, v)
	if err != nil && !errors.Is(err, errRemoved) {
		l.logger.Warn("syncing vault failed", "vault", info.Name, "err", err)
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
	go func() {
		ticker := time.NewTicker(min(timeout/4, 10*time.Second))
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
	}()
	stop := func() {
		close(done)
		cancel()
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
