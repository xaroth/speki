package store

import (
	"testing"
	"testing/synctest"
	"time"
)

// Internal: dropping expired entries isn't visible through the API, only in the map's size.
func TestMemoryDropsExpired(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := NewMemory()
		for _, key := range []string{"a", "b", "c"} {
			_ = m.Set(t.Context(), key, []byte("v"), time.Second)
		}
		time.Sleep(time.Minute) // past the cleaning interval
		_ = m.Set(t.Context(), "d", []byte("v"), time.Minute)
		if got := len(m.values); got != 1 {
			t.Errorf("got %d entries, want 1", got)
		}
	})
}
