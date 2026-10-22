package store_test

import (
	"context"
	"testing"
	"time"
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
