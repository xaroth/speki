package library

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestCoalescer(t *testing.T) {
	tests := []struct {
		name   string
		during int  // notices while the first sync runs
		cancel bool // ctx ends during the first sync
		want   int32
	}{
		{name: "one notice", want: 1},
		{name: "a notice during the sync", during: 1, want: 2},
		{name: "several notices during the sync", during: 3, want: 2},
		{name: "stopped during the sync", during: 1, cancel: true, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var c coalescer
			var runs atomic.Int32
			started := make(chan struct{}, 10)
			release := make(chan struct{})
			run := func() {
				runs.Add(1)
				started <- struct{}{}
				<-release
			}

			c.notify(ctx, 1, run)
			<-started
			for range tt.during {
				c.notify(ctx, 1, run)
			}
			if tt.cancel {
				cancel()
			}
			close(release)
			c.wg.Wait()

			if got := runs.Load(); got != tt.want {
				t.Errorf("ran %d times, want %d", got, tt.want)
			}
		})
	}
}

func TestCoalescerRunsVaultsIndependently(t *testing.T) {
	var c coalescer
	started := make(chan int64, 2)
	release := make(chan struct{})
	for _, id := range []int64{1, 2} {
		c.notify(t.Context(), id, func() {
			started <- id
			<-release
		})
	}
	// Both start while neither has finished.
	<-started
	<-started
	close(release)
	c.wg.Wait()
}

// failingNotices fails Subscribe a number of times, then ends the subscription cleanly.
type failingNotices struct {
	failures int
	calls    int
}

func (n *failingNotices) Subscribe(_ context.Context, f func(int64, string)) error {
	n.calls++
	f(999, "abc") // a vault the library doesn't have: ignored
	if n.calls <= n.failures {
		return errors.New("store unreachable")
	}
	return nil
}

func TestFollowNoticesRetriesSubscribe(t *testing.T) {
	tests := []struct {
		name     string
		failures int
		want     time.Duration // time spent backing off
	}{
		{name: "subscribes at once", failures: 0, want: 0},
		{name: "retries with backoff", failures: 3, want: 7 * time.Second},
		{name: "backoff is capped", failures: 8, want: (1 + 2 + 4 + 8 + 16 + 32 + 60 + 60) * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				l := New(nil, Options{}, discard())
				n := &failingNotices{failures: tt.failures}
				start := time.Now()
				l.FollowNotices(t.Context(), n)
				if n.calls != tt.failures+1 {
					t.Errorf("subscribed %d times, want %d", n.calls, tt.failures+1)
				}
				if got := time.Since(start); got != tt.want {
					t.Errorf("backed off for %v, want %v", got, tt.want)
				}
			})
		})
	}
}

func TestFollowNoticesStopsWhileBackingOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		l := New(nil, Options{}, discard())
		done := make(chan struct{})
		go func() {
			l.FollowNotices(ctx, &failingNotices{failures: 1000})
			close(done)
		}()
		synctest.Wait()
		cancel()
		<-done
	})
}
