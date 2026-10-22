package oauth_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/xaroth/speki/gh"
)

// A refresh whose result can't be kept for other requests with the same token must not let them ask
// GitHub with the spent token.
func TestRefreshResultLost(t *testing.T) {
	t.Parallel()
	lost := oauthError{Error: "temporarily_unavailable", Description: "this refresh token was just used, but its result was lost"}
	testCases := []struct {
		name string
		hook func(t *testing.T, s *fakeStore) func(op, key string) error

		wantFirst      int // status of the refresh that finds the store failing
		wantLockHeld   bool
		wantSecond     int
		wantSecondErr  oauthError
		wantRetryAfter string
		wantRefreshes  int // made with GitHub
	}{
		{
			name: "kept on the second try",
			hook: failSets(1), wantFirst: http.StatusOK, wantSecond: http.StatusOK, wantRefreshes: 1,
		},
		{
			name: "lost, marker kept",
			hook: failSets(2), wantFirst: http.StatusOK, wantSecond: http.StatusServiceUnavailable, wantSecondErr: lost, wantRetryAfter: "5", wantRefreshes: 1,
		},
		{
			name: "nothing kept",
			hook: failSets(-1), wantFirst: http.StatusOK, wantLockHeld: true, wantRefreshes: 1, // requests with the token wait out the lock
		},
		{
			name: "marker lands while locking",
			hook: func(t *testing.T, s *fakeStore) func(op, key string) error {
				return func(op, key string) error {
					if rest, ok := strings.CutPrefix(key, "refresh-lock:"); ok && op == "setnx" {
						_ = s.Memory.Set(t.Context(), "refresh-result:"+rest, []byte("lost"), time.Minute)
					}
					return nil
				}
			},
			wantFirst: http.StatusServiceUnavailable, wantSecond: http.StatusServiceUnavailable, wantSecondErr: lost, wantRetryAfter: "5",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				clientID, first := e.signedIn(t)
				form := refreshForm(clientID, first.RefreshToken)
				e.store.onCall(testCase.hook(t, e.store))
				if _, rec := e.token(t, form); rec.Code != testCase.wantFirst {
					t.Fatalf("first refresh: got %d %s, want %d", rec.Code, rec.Body, testCase.wantFirst)
				}
				e.store.onCall(nil)
				if _, held, _ := e.store.Memory.Get(t.Context(), storeKey("refresh-lock", first.RefreshToken)); held != testCase.wantLockHeld {
					t.Errorf("got lock held %v, want %v", held, testCase.wantLockHeld)
				}
				if testCase.wantSecond != 0 {
					rec := e.do(t, "/oauth/token", form)
					checkTokenAnswer(t, rec, testCase.wantSecond, bothScopes, testCase.wantSecondErr, testCase.wantRetryAfter)
				}
				if n := countSpent(e.github); n != testCase.wantRefreshes {
					t.Errorf("got %d GitHub refreshes, want %d", n, testCase.wantRefreshes)
				}
			})
		})
	}
}

// failSets fails the first n Set calls, or all of them for n < 0.
func failSets(n int) func(*testing.T, *fakeStore) func(op, key string) error {
	return func(*testing.T, *fakeStore) func(op, key string) error {
		return func(op, _ string) error {
			if op != "set" || n == 0 {
				return nil
			}
			n--
			return errStoreDown
		}
	}
}

func countSpent(f *fakeGitHub) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.spent)
}

// A refresh that finds the token's lock taken waits for the other request's result or for the lock.
func TestRefreshWaitsForTheLock(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name        string
		lockFor     time.Duration
		leaveAfter  time.Duration // the client hangs up
		wantWait    time.Duration
		wantStatus  int
		wantErr     oauthError
		wantRetry   string
		wantNoReply bool
	}{
		{name: "lock runs out", lockFor: 3 * time.Second, wantWait: 3 * time.Second, wantStatus: http.StatusOK},
		{
			name: "lock outlasts the wait", lockFor: time.Minute, wantWait: 25 * time.Second, wantStatus: http.StatusServiceUnavailable,
			wantErr: oauthError{Error: "temporarily_unavailable", Description: "another refresh is in progress; try again"}, wantRetry: "1",
		},
		{name: "client leaves", lockFor: time.Minute, leaveAfter: 2 * time.Second, wantWait: 2 * time.Second, wantNoReply: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				clientID, first := e.signedIn(t)
				_, _ = e.store.Memory.SetNX(t.Context(), storeKey("refresh-lock", first.RefreshToken), []byte("other"), testCase.lockFor)
				ctx := t.Context()
				if testCase.leaveAfter > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, testCase.leaveAfter)
					defer cancel()
				}
				start := time.Now()
				rec := e.doCtx(t, ctx, "/oauth/token", refreshForm(clientID, first.RefreshToken))
				if waited := time.Since(start); waited < testCase.wantWait || waited > testCase.wantWait+200*time.Millisecond {
					t.Errorf("got an answer after %v, want %v", waited, testCase.wantWait)
				}
				if testCase.wantNoReply {
					if rec.Body.Len() != 0 {
						t.Errorf("got %s, want no answer", rec.Body)
					}
					return
				}
				checkTokenAnswer(t, rec, testCase.wantStatus, bothScopes, testCase.wantErr, testCase.wantRetry)
			})
		})
	}
}

// Two requests refreshing one token at once, as from two windows, both get the new pair.
func TestRefreshConcurrent(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		clientID, first := e.signedIn(t)
		github := make(chan struct{})
		e.github.set(func(f *fakeGitHub) { f.afterSpend = func(context.Context) { <-github } })
		answers := make(chan tokenResponse, 2)
		refresh := func() {
			resp, rec := e.token(t, refreshForm(clientID, first.RefreshToken))
			if rec.Code != http.StatusOK {
				t.Errorf("got %d %s, want 200", rec.Code, rec.Body)
			}
			answers <- resp
		}
		go refresh()
		synctest.Wait() // the first holds the lock and waits on GitHub
		go refresh()
		synctest.Wait() // the second waits on the lock
		close(github)
		if a, b := <-answers, <-answers; a != b {
			t.Error("got two different pairs, want the same")
		}
		if n := countSpent(e.github); n != 1 {
			t.Errorf("got %d GitHub refreshes, want 1", n)
		}
	})
}

// GitHub spends the old refresh token as it answers. A client that hung up meanwhile retries with the same
// token, and gets the pair speki finished making anyway.
func TestRefreshDroppedConnection(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	clientID, first := e.signedIn(t)
	form := refreshForm(clientID, first.RefreshToken)
	ctx, cancel := context.WithCancel(t.Context())
	e.github.set(func(f *fakeGitHub) { f.afterSpend = func(context.Context) { cancel() } })
	e.doCtx(t, ctx, "/oauth/token", form)
	e.github.set(func(f *fakeGitHub) { f.afterSpend = nil })

	rec := e.do(t, "/oauth/token", form)
	checkTokenAnswer(t, rec, http.StatusOK, bothScopes, oauthError{}, "")
}

// A GitHub call without an answer ends before the token's lock does, so no second refresh can start
// alongside it.
func TestRefreshGitHubTimeout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		clientID, first := e.signedIn(t)
		start := time.Now()
		var ended time.Duration
		var lockHeld bool
		e.github.set(func(f *fakeGitHub) {
			f.afterSpend = func(ctx context.Context) {
				<-ctx.Done()
				ended = time.Since(start)
				_, lockHeld, _ = e.store.Memory.Get(t.Context(), storeKey("refresh-lock", first.RefreshToken))
			}
		})
		rec := e.do(t, "/oauth/token", refreshForm(clientID, first.RefreshToken))
		checkTokenAnswer(t, rec, http.StatusServiceUnavailable, tokenResponse{},
			oauthError{Error: "temporarily_unavailable", Description: "GitHub didn't answer; try again"}, "5")
		if ended != 20*time.Second || !lockHeld {
			t.Errorf("got GitHub call ended after %v with lock held %v, want 20s with the lock held", ended, lockHeld)
		}
	})
}

// GitHub refusing a refresh token signs the user out. When an earlier refresh with that token got no
// answer, GitHub most likely spent the token then: the sign-out is logged as lost to the timeout, once.
func TestRefreshSignOut(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name     string
		first    func(f *fakeGitHub) // makes a first refresh fail; nil for none
		retryOK  bool                // a retry then succeeds, and its stored result runs out
		wantLost bool
	}{
		{name: "no answer from GitHub", first: func(f *fakeGitHub) { f.afterSpend = func(ctx context.Context) { <-ctx.Done() } }, wantLost: true},
		{name: "answer lost", first: func(f *fakeGitHub) { f.refreshErr = errors.New("connection reset") }, wantLost: true},
		{name: "GitHub not reached", first: func(f *fakeGitHub) { f.refreshErr = gh.ErrUnreached }},
		{name: "no earlier failure"},
		{name: "a retry got through", first: func(f *fakeGitHub) { f.refreshErr = errors.New("connection reset") }, retryOK: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				clientID, first := e.signedIn(t)
				form := refreshForm(clientID, first.RefreshToken)
				if testCase.first != nil {
					e.github.set(testCase.first)
					if _, rec := e.token(t, form); rec.Code != http.StatusServiceUnavailable {
						t.Fatalf("first refresh: got %d %s, want 503", rec.Code, rec.Body)
					}
					e.github.set(func(f *fakeGitHub) { f.refreshErr, f.afterSpend = nil, nil })
				}
				if testCase.retryOK {
					if _, rec := e.token(t, form); rec.Code != http.StatusOK {
						t.Fatalf("retry: got %d %s, want 200", rec.Code, rec.Body)
					}
					time.Sleep(time.Minute + time.Second) // the stored result runs out
				}
				e.github.set(func(f *fakeGitHub) { f.refreshErr = gh.ErrBadGrant }) // spent, by then or by someone else

				lostLogs := func() int { return strings.Count(e.logs.String(), "refresh lost to GitHub timeout") }
				rec := e.do(t, "/oauth/token", form)
				checkTokenAnswer(t, rec, http.StatusBadRequest, tokenResponse{}, oauthError{Error: "invalid_grant", Description: "sign in again"}, "")
				want := 0
				if testCase.wantLost {
					want = 1
				}
				if got := lostLogs(); got != want {
					t.Errorf("got %d lost-to-timeout logs, want %d:\n%s", got, want, e.logs.String())
				}
				if strings.Contains(e.logs.String(), "ghr_1") {
					t.Error("got the GitHub refresh token in the logs, want it kept out")
				}

				_, _ = e.token(t, form)
				if got := lostLogs(); got != want {
					t.Errorf("got %d lost-to-timeout logs after a second refusal, want still %d", got, want)
				}
			})
		})
	}
}
