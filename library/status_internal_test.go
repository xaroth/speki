package library

// Internal: this flips a vault's state directly while Vaults reads it, a race no library call makes on demand.

import (
	"sync"
	"testing"
	"time"
)

// A vault changing state while Vaults runs is in one of its lists, never in none: callers tell "not
// served yet" from "unknown" by it.
func TestVaultsListsEveryVaultOnce(t *testing.T) {
	t.Parallel()
	l := New(nil, Options{DataDir: t.TempDir()}, discard())
	v := newVault(testVault, t.TempDir(), time.Now)
	l.vaults[testVault.RepoID] = v

	stop := make(chan struct{})
	var flipping sync.WaitGroup
	flipping.Go(func() {
		states := []State{StateBuilding, StateServing, StateFailed}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			v.mu.Lock()
			v.status.State = states[i%len(states)]
			v.mu.Unlock()
		}
	})
	defer flipping.Wait()
	defer close(stop)
	for range 1000 {
		serving, building, failed := l.Vaults()
		if n := len(serving) + len(building) + len(failed); n != 1 {
			t.Fatalf("got the vault listed %d times, want once", n)
		}
	}
}
