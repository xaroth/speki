package store

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// kv is what both stores offer for values.
type kv interface {
	SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Delete(ctx context.Context, key string) error
}

// testValues runs the same checks against a store. advance moves the store's clock.
func testValues(t *testing.T, s kv, advance func(time.Duration)) {
	t.Helper()
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	ok, err := s.SetNX(ctx, "code:1", []byte("x"), time.Minute)
	must(err)
	if !ok {
		t.Fatal("first SetNX refused")
	}
	ok, err = s.SetNX(ctx, "code:1", []byte("y"), time.Minute)
	must(err)
	if ok {
		t.Fatal("second SetNX accepted")
	}
	v, found, err := s.Get(ctx, "code:1")
	must(err)
	if !found || string(v) != "x" {
		t.Errorf("Get = %q, %v", v, found)
	}

	advance(2 * time.Minute)
	_, found, err = s.Get(ctx, "code:1")
	must(err)
	if found {
		t.Error("expired value still found")
	}
	ok, err = s.SetNX(ctx, "code:1", []byte("z"), time.Minute)
	must(err)
	if !ok {
		t.Error("SetNX refused after expiry")
	}

	must(s.Set(ctx, "k", []byte("v"), time.Minute))
	v, found, err = s.Get(ctx, "k")
	must(err)
	if !found || string(v) != "v" {
		t.Errorf("Get after Set = %q, %v", v, found)
	}
	must(s.Delete(ctx, "k"))
	_, found, err = s.Get(ctx, "k")
	must(err)
	if found {
		t.Error("deleted value still found")
	}
}

func TestRedis(t *testing.T) {
	mr := miniredis.RunT(t)
	r, err := NewRedis(t.Context(), "redis://"+mr.Addr(), discard())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	testValues(t, r, mr.FastForward)
	if !mr.Exists("speki:code:1") {
		t.Error("keys aren't prefixed")
	}

	_, err = NewRedis(t.Context(), "redis://127.0.0.1:1", discard())
	if err == nil {
		t.Error("connected to nothing")
	}
	_, err = NewRedis(t.Context(), "http://x", discard())
	if err == nil {
		t.Error("accepted a URL that isn't redis")
	}
}

func TestRedisNotices(t *testing.T) {
	mr := miniredis.RunT(t)
	a, err := NewRedis(t.Context(), "redis://"+mr.Addr(), discard())
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewRedis(t.Context(), "redis://"+mr.Addr(), discard())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	got := make(chan string, 100)
	done := make(chan error, 1)
	go func() {
		done <- b.Subscribe(ctx, func(repoID int64, commit string) {
			if repoID == 100 {
				got <- commit
			}
		})
	}()
	// Publish until b hears it: notices sent before b's subscription is in place are lost, as designed.
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(5 * time.Second)
wait:
	for {
		select {
		case c := <-got:
			if c != "abc" {
				t.Errorf("notice commit = %q", c)
			}
			break wait
		case <-tick.C:
			err = a.Publish(t.Context(), 100, "abc")
			if err != nil {
				t.Fatal(err)
			}
		case <-timeout:
			t.Fatal("no notice")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Subscribe: %v", err)
	}
}

func TestMemory(t *testing.T) {
	m := NewMemory()
	now := time.Now()
	m.now = func() time.Time { return now }
	testValues(t, m, func(d time.Duration) { now = now.Add(d) })
	if len(m.values) > 2 {
		t.Errorf("expired entries kept: %d", len(m.values))
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := m.Publish(ctx, 1, "c"); err != nil {
		t.Error(err)
	}
	if err := m.Subscribe(ctx, func(int64, string) { t.Error("a notice in memory") }); err != nil {
		t.Error(err)
	}
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
