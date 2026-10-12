package library

// Internal: coalescing needs syncs that block on demand, which FollowNotices can't be made to run.

import (
	"context"
	"sync/atomic"
	"testing"
)

func TestCoalescer(t *testing.T) {
	t.Parallel()
	testCases := []struct {
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
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
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
			for range testCase.during {
				c.notify(ctx, 1, run)
			}
			if testCase.cancel {
				cancel()
			}
			close(release)
			c.wg.Wait()

			if got := runs.Load(); got != testCase.want {
				t.Errorf("got %d runs, want %d", got, testCase.want)
			}
		})
	}
}

func TestCoalescerRunsVaultsIndependently(t *testing.T) {
	t.Parallel()
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
