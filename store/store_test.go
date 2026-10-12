package store_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/eve-online-tools/speki/store"
)

type kv interface {
	SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Get(ctx context.Context, key string) ([]byte, bool, error)
	GetDel(ctx context.Context, key string) ([]byte, bool, error)
	Release(ctx context.Context, key string, value []byte) error
}

// testValues runs the same checks against a store. advance moves the store's clock.
func testValues(tb testing.TB, s kv, advance func(time.Duration)) {
	tb.Helper()
	ctx := tb.Context()
	must := func(err error) {
		tb.Helper()
		if err != nil {
			tb.Fatal(err)
		}
	}
	wantValue := func(step string, gotValue []byte, gotFound bool, want string) {
		tb.Helper()
		if want == "" && gotFound {
			tb.Errorf("%s: got %q, want no value", step, gotValue)
		}
		if want != "" && (!gotFound || string(gotValue) != want) {
			tb.Errorf("%s: got %q, %v, want %q", step, gotValue, gotFound, want)
		}
	}

	ok, err := s.SetNX(ctx, "code:1", []byte("x"), time.Minute)
	must(err)
	if !ok {
		tb.Fatal("first SetNX: got refused, want stored")
	}
	ok, err = s.SetNX(ctx, "code:1", []byte("y"), time.Minute)
	must(err)
	if ok {
		tb.Fatal("second SetNX: got stored, want refused")
	}
	v, found, err := s.Get(ctx, "code:1")
	must(err)
	wantValue("Get", v, found, "x")

	advance(2 * time.Minute)
	v, found, err = s.Get(ctx, "code:1")
	must(err)
	wantValue("Get after expiry", v, found, "")
	ok, err = s.SetNX(ctx, "code:1", []byte("z"), time.Minute)
	must(err)
	if !ok {
		tb.Error("SetNX after expiry: got refused, want stored")
	}

	must(s.Set(ctx, "k", []byte("v"), time.Minute))
	v, found, err = s.Get(ctx, "k")
	must(err)
	wantValue("Get after Set", v, found, "v")

	// A lock is only released by its owner: after it expired and someone else took it, the first
	// holder's release leaves it alone.
	_, err = s.SetNX(ctx, "lock", []byte("first"), time.Second)
	must(err)
	advance(2 * time.Second)
	ok, err = s.SetNX(ctx, "lock", []byte("second"), time.Minute)
	must(err)
	if !ok {
		tb.Fatal("SetNX on an expired lock: got refused, want stored")
	}
	must(s.Release(ctx, "lock", []byte("first")))
	v, found, err = s.Get(ctx, "lock")
	must(err)
	wantValue("Get after someone else's release", v, found, "second")
	must(s.Release(ctx, "lock", []byte("second")))
	v, found, err = s.Get(ctx, "lock")
	must(err)
	wantValue("Get after the owner's release", v, found, "")

	must(s.Set(ctx, "marker", []byte("m"), time.Minute))
	v, found, err = s.GetDel(ctx, "marker")
	must(err)
	wantValue("GetDel", v, found, "m")
	v, found, err = s.GetDel(ctx, "marker")
	must(err)
	wantValue("second GetDel", v, found, "")
	must(s.Set(ctx, "marker", []byte("m"), time.Second))
	advance(2 * time.Second)
	v, found, err = s.GetDel(ctx, "marker")
	must(err)
	wantValue("GetDel after expiry", v, found, "")
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newRedis(tb testing.TB, addr string) *store.Redis {
	tb.Helper()
	r, err := store.NewRedis(tb.Context(), "redis://"+addr, discard())
	if err != nil {
		tb.Fatalf("NewRedis: %v", err)
	}
	tb.Cleanup(func() { _ = r.Close() })
	return r
}

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

func TestRedis(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	r := newRedis(t, mr.Addr())

	testValues(t, r, mr.FastForward)

	if !mr.Exists("speki:code:1") {
		t.Error("got key code:1 without the speki: prefix, want it prefixed")
	}
}

func TestNewRedisRejects(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name string
		url  string
	}{
		// No retries: the default backoff only makes the test slower.
		{name: "not answering", url: "redis://" + hangUp(t) + "?max_retries=-1"},
		{name: "not redis", url: "http://x"},
		{name: "bad port", url: "redis://user:hunter2@host:notaport/0"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := store.NewRedis(t.Context(), testCase.url, discard())
			if err == nil {
				t.Fatal("got no error, want one")
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("got error %q, want it without the password", err)
			}
		})
	}
}

// hangUp listens for connections and closes each one straight away, so a client fails fast rather than
// retrying a refused dial.
func hangUp(tb testing.TB) string {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().String()
}

func TestRedisNotices(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	a, b := newRedis(t, mr.Addr()), newRedis(t, mr.Addr())
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
				t.Errorf("got commit %q, want %q", c, "abc")
			}
			break wait
		case <-tick.C:
			err := a.Publish(t.Context(), 100, "abc")
			if err != nil {
				t.Fatalf("Publish: %v", err)
			}
		case <-timeout:
			t.Fatal("got no notice, want one")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Subscribe: got %v, want nil", err)
	}
}

// stallingProxy forwards to addr, but swallows the SUBSCRIBE commands a client sends, as a half-open
// connection or a stuck proxy would. It sends on swallowed each time.
func stallingProxy(tb testing.TB, addr string, swallowed chan<- struct{}) string {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", addr)
			if err != nil {
				_ = client.Close()
				return
			}
			go func() { _, _ = io.Copy(client, server) }()
			go func() {
				buf := make([]byte, 4096)
				for {
					n, err := client.Read(buf)
					if err != nil {
						return
					}
					if bytes.Contains(bytes.ToUpper(buf[:n]), []byte("SUBSCRIBE")) {
						swallowed <- struct{}{}
						continue
					}
					_, _ = server.Write(buf[:n])
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func TestSubscribeStopsWhenRedisStalls(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	swallowed := make(chan struct{}, 10)
	r := newRedis(t, stallingProxy(t, mr.Addr(), swallowed))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Subscribe(ctx, func(int64, string) {}) }()
	<-swallowed // Subscribe now waits for a confirmation that won't come
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("got Subscribe still running after its context ended, want it returned")
	}
}
