package store_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/xaroth/speki/store"
)

func TestMemory(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := store.NewMemory()
		testValues(t, m, time.Sleep)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := m.Publish(ctx, 1, "c"); err != nil {
			t.Errorf("Publish: got %v, want nil", err)
		}
		if err := m.Subscribe(ctx, func(int64, string) { t.Error("got a notice in memory, want none") }); err != nil {
			t.Errorf("Subscribe: got %v, want nil", err)
		}
	})
}
