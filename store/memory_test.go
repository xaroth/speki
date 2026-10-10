package store

import (
	"testing"
	"time"
)

func TestMemory(t *testing.T) {
	m := NewMemory()
	now := time.Now()
	m.now = func() time.Time { return now }
	ctx := t.Context()

	ok, _ := m.SetNX(ctx, "code:1", []byte("x"), time.Minute)
	if !ok {
		t.Fatal("first SetNX refused")
	}
	ok, _ = m.SetNX(ctx, "code:1", []byte("y"), time.Minute)
	if ok {
		t.Fatal("second SetNX accepted")
	}
	v, found, _ := m.Get(ctx, "code:1")
	if !found || string(v) != "x" {
		t.Errorf("Get = %q, %v", v, found)
	}

	now = now.Add(2 * time.Minute)
	if _, found, _ := m.Get(ctx, "code:1"); found {
		t.Error("expired value still found")
	}
	ok, _ = m.SetNX(ctx, "code:1", []byte("z"), time.Minute)
	if !ok {
		t.Error("SetNX refused after expiry")
	}
	if len(m.values) != 1 {
		t.Errorf("expired entries kept: %d", len(m.values))
	}

	_ = m.Set(ctx, "k", []byte("v"), time.Minute)
	_ = m.Delete(ctx, "k")
	if _, found, _ := m.Get(ctx, "k"); found {
		t.Error("deleted value still found")
	}
}
