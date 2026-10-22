package mcpserver

import (
	"log/slog"
	"testing"
	"testing/synctest"
	"time"
)

// Internal: dropping idle limiters isn't visible through the API, only in the map's size.
func TestLimiterDropsIdle(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := New(nil, nil, nil, Identity{}, "test", slog.New(slog.DiscardHandler))
		s.limiter(1)
		s.limiter(2)
		time.Sleep(limiterIdle / 2)
		s.limiter(2)
		time.Sleep(limiterIdle / 2)
		s.limiter(3)
		if _, ok := s.limiters[1]; ok {
			t.Errorf("got user 1's limiter kept, want it dropped after %v idle", limiterIdle)
		}
		if got := len(s.limiters); got != 2 {
			t.Errorf("got %d limiters, want 2", got)
		}
	})
}
