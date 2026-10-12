package library_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/eve-online-tools/speki/library"
)

// notice sends one change notice, then ends the subscription cleanly.
type notice struct {
	repoID int64
	commit string
}

func (n notice) Subscribe(_ context.Context, f func(int64, string)) error {
	f(n.repoID, n.commit)
	return nil
}

// A notice syncs the vault it names, but doesn't override a vault's backoff after failures.
func TestFollowNotices(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name       string
		backingOff bool
		wantHits   int
	}{
		{name: "syncs the vault", wantHits: 1},
		{name: "vault backing off", backingOff: true, wantHits: 0},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			r := newRemote(t)
			r.push(t, map[string]string{"a.md": "# A\n"})
			l := newLibrary(t, r, library.Options{})
			v := serving(t, l)
			if testCase.backingOff {
				r.setHeadErr(errors.New("502"))
				l.Poll(t.Context())
			}
			head := r.push(t, map[string]string{"a.md": "# A\n\nchanged\n"})

			l.FollowNotices(t.Context(), notice{repoID: kb.RepoID, commit: head})
			if got := search(t, v, "changed"); len(got) != testCase.wantHits {
				t.Errorf("got %v, want %d hits", got, testCase.wantHits)
			}
		})
	}
}

// failingNotices fails Subscribe a number of times, then ends the subscription cleanly.
type failingNotices struct {
	failures int
	calls    int
}

func (n *failingNotices) Subscribe(_ context.Context, f func(int64, string)) error {
	n.calls++
	f(999, "abc") // a vault the library doesn't have: ignored
	if n.calls <= n.failures {
		return errors.New("store unreachable")
	}
	return nil
}

func TestFollowNoticesRetriesSubscribe(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name     string
		failures int
		want     time.Duration // time spent backing off
	}{
		{name: "subscribes at once", failures: 0, want: 0},
		{name: "retries with backoff", failures: 3, want: 7 * time.Second},
		{name: "backoff is capped", failures: 8, want: (1 + 2 + 4 + 8 + 16 + 32 + 60 + 60) * time.Second},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				l := library.New(nil, library.Options{}, discard())
				n := &failingNotices{failures: testCase.failures}
				start := time.Now()
				l.FollowNotices(t.Context(), n)
				if n.calls != testCase.failures+1 {
					t.Errorf("got %d subscriptions, want %d", n.calls, testCase.failures+1)
				}
				if got := time.Since(start); got != testCase.want {
					t.Errorf("got %v backing off, want %v", got, testCase.want)
				}
			})
		})
	}
}

func TestFollowNoticesStopsWhileBackingOff(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		l := library.New(nil, library.Options{}, discard())
		done := make(chan struct{})
		go func() {
			l.FollowNotices(ctx, &failingNotices{failures: 1000})
			close(done)
		}()
		synctest.Wait()
		cancel()
		<-done
	})
}
