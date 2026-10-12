package library

// Internal: a stall takes minutes without git progress, which only a synctest bubble around stallContext fakes.

import (
	"testing"
	"testing/synctest"
	"time"
)

func TestStallContext(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, touch, stop := stallContext(t.Context(), 4*time.Minute)
		defer stop()
		for range 5 { // fifteen minutes of steady progress
			touch()
			time.Sleep(3 * time.Minute)
			if ctx.Err() != nil {
				t.Fatalf("got %v while making progress, want no cancellation", ctx.Err())
			}
		}
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if ctx.Err() == nil {
			t.Fatal("got a live context after stalling, want it cancelled")
		}
	})
}

// stop can be called more than once, and returns after the watcher has ended.
func TestStallContextStop(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, touch, stop := stallContext(t.Context(), time.Nanosecond)
		touch()
		stop()
		stop()
		if ctx.Err() == nil {
			t.Error("got a live context after stop, want it cancelled")
		}
	})
}
