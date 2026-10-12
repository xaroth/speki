package oauth_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/eve-online-tools/speki/gh"
	"github.com/eve-online-tools/speki/oauth"
)

// checkTokenAnswer checks a token endpoint answer: a token pair when wantErr is empty, else that error.
func checkTokenAnswer(tb testing.TB, rec *httptest.ResponseRecorder, status int, want tokenResponse, wantErr oauthError, retryAfter string) {
	tb.Helper()
	if rec.Code != status {
		tb.Fatalf("got status %d %s, want %d", rec.Code, rec.Body, status)
	}
	got := map[string]string{"Content-Type": rec.Header().Get("Content-Type"), "Cache-Control": rec.Header().Get("Cache-Control"), "Retry-After": rec.Header().Get("Retry-After")}
	if diff := cmp.Diff(map[string]string{"Content-Type": "application/json", "Cache-Control": "no-store", "Retry-After": retryAfter}, got); diff != "" {
		tb.Errorf("headers mismatch (-want +got):\n%s", diff)
	}
	if wantErr.Error != "" {
		var gotErr oauthError
		_ = json.Unmarshal(rec.Body.Bytes(), &gotErr)
		if diff := cmp.Diff(wantErr, gotErr); diff != "" {
			tb.Errorf("error mismatch (-want +got):\n%s", diff)
		}
		return
	}
	var resp tokenResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.AccessToken == "" || resp.RefreshToken == "" {
		tb.Errorf("got access token %q and refresh token %q, want both", resp.AccessToken, resp.RefreshToken)
	}
	resp.AccessToken, resp.RefreshToken = "", ""
	if diff := cmp.Diff(want, resp); diff != "" {
		tb.Errorf("token response mismatch (-want +got):\n%s", diff)
	}
}

var (
	bothScopes = tokenResponse{TokenType: "Bearer", ExpiresIn: 3600, Scope: "speki:read speki:write"}
	readScope  = tokenResponse{TokenType: "Bearer", ExpiresIn: 3600, Scope: "speki:read"}
)

func TestTokenCodeGrant(t *testing.T) {
	t.Parallel()
	invalidGrant := func(description string) oauthError {
		return oauthError{Error: "invalid_grant", Description: description}
	}
	testCases := []struct {
		name       string
		github     func(f *fakeGitHub)
		noRedirect bool // neither authorize nor the token request name a redirect_uri
		storeDown  bool
		wait       time.Duration
		form       func(t *testing.T, e *env, good url.Values) url.Values

		wantStatus    int
		want          tokenResponse
		wantErr       oauthError
		codeStillGood bool // the code works after the rejected request
	}{
		{name: "exchanged", wantStatus: http.StatusOK, want: bothScopes},
		{name: "GitHub token expires sooner", github: func(f *fakeGitHub) { f.accessTTL = 30 * time.Minute }, wantStatus: http.StatusOK,
			want: tokenResponse{TokenType: "Bearer", ExpiresIn: 1800, Scope: "speki:read speki:write"}},
		{name: "redirect_uri named at neither end", noRedirect: true, wantStatus: http.StatusOK, want: bothScopes},

		{name: "unknown grant type", form: set("grant_type", "password"), wantStatus: http.StatusBadRequest,
			wantErr: oauthError{Error: "unsupported_grant_type", Description: "use authorization_code or refresh_token"}, codeStillGood: true},
		{name: "form too large", form: set("padding", strings.Repeat("x", 64<<10)), wantStatus: http.StatusBadRequest,
			wantErr: oauthError{Error: "invalid_request", Description: "the form couldn't be read"}, codeStillGood: true},
		{name: "not a code", form: set("code", "v1.k1.xyz"), wantStatus: http.StatusBadRequest, wantErr: invalidGrant("the code is invalid or expired"), codeStillGood: true},
		{name: "expired code", wait: time.Minute + skew + time.Second, wantStatus: http.StatusBadRequest, wantErr: invalidGrant("the code is invalid or expired")},
		{
			name: "another client",
			form: func(t *testing.T, e *env, good url.Values) url.Values {
				good.Set("client_id", e.register(t, "http://127.0.0.1/other"))
				return good
			},
			wantStatus: http.StatusBadRequest, wantErr: invalidGrant("the code was issued to another client"), codeStillGood: true,
		},
		{name: "redirect_uri left out", form: set("redirect_uri", ""), wantStatus: http.StatusBadRequest,
			wantErr: invalidGrant("redirect_uri is missing or doesn't match"), codeStillGood: true},
		{name: "another redirect_uri", form: set("redirect_uri", "http://127.0.0.1/other"), wantStatus: http.StatusBadRequest,
			wantErr: invalidGrant("redirect_uri is missing or doesn't match"), codeStillGood: true},
		{
			name: "code from another speki with the same keys",
			form: func(t *testing.T, e *env, _ url.Values) url.Values {
				other := newEnv(t, withIssuer("https://other.example"), withKeys(e.keyring))
				clientID := other.register(t, loopback)
				return codeForm(clientID, other.signIn(t, other.authorizeURL(clientID), "write"), loopback)
			},
			wantStatus: http.StatusBadRequest, wantErr: invalidGrant("the code was issued for another server"),
		},
		{name: "wrong verifier", form: set("code_verifier", "wrong"), wantStatus: http.StatusBadRequest,
			wantErr: invalidGrant("the PKCE verifier doesn't match"), codeStillGood: true},
		{name: "another resource", form: set("resource", "https://other.example/mcp"), wantStatus: http.StatusBadRequest,
			wantErr: oauthError{Error: "invalid_target", Description: "tokens are only issued for " + issuer + "/mcp"}, codeStillGood: true},
		{
			name: "code used twice",
			form: func(t *testing.T, e *env, good url.Values) url.Values {
				if _, rec := e.token(t, good); rec.Code != http.StatusOK {
					t.Fatalf("first exchange: got %d %s, want 200", rec.Code, rec.Body)
				}
				return good
			},
			wantStatus: http.StatusBadRequest, wantErr: invalidGrant("the code was already used"),
		},
		{name: "store down", storeDown: true, wantStatus: http.StatusInternalServerError, wantErr: oauthError{Error: "server_error", Description: "try again"}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				if testCase.github != nil {
					e.github.set(testCase.github)
				}
				redirect := loopback
				if testCase.noRedirect {
					redirect = ""
				}
				clientID := e.register(t, loopback)
				code := e.signIn(t, authorizeURL(clientID, redirect, nil), "write")
				good := codeForm(clientID, code, redirect)
				form := codeForm(clientID, code, redirect)
				if testCase.form != nil {
					form = testCase.form(t, e, form)
				}
				if testCase.storeDown {
					e.store.onCall(func(string, string) error { return errStoreDown })
				}
				time.Sleep(testCase.wait)

				rec := e.do(t, "/oauth/token", form)
				checkTokenAnswer(t, rec, testCase.wantStatus, testCase.want, testCase.wantErr, "")
				if testCase.codeStillGood {
					if _, rec := e.token(t, good); rec.Code != http.StatusOK {
						t.Errorf("exchange after the rejected one: got %d %s, want 200", rec.Code, rec.Body)
					}
				}
			})
		})
	}
}

// set returns a form change that sets key, or deletes it for an empty value.
func set(key, value string) func(*testing.T, *env, url.Values) url.Values {
	return func(_ *testing.T, _ *env, f url.Values) url.Values {
		if value == "" {
			f.Del(key)
		} else {
			f.Set(key, value)
		}
		return f
	}
}

func TestTokenRefreshGrant(t *testing.T) {
	t.Parallel()
	invalidGrant := func(description string) oauthError {
		return oauthError{Error: "invalid_grant", Description: description}
	}
	unavailable := func(description string) oauthError {
		return oauthError{Error: "temporarily_unavailable", Description: description}
	}
	scope := func(s string) func(*testing.T, *env, url.Values) url.Values { return set("scope", s) }
	testCases := []struct {
		name   string
		access string // chosen at sign-in; empty is write
		github func(f *fakeGitHub)
		store  func(s *fakeStore)
		wait   time.Duration
		before func(t *testing.T, e *env, form url.Values) url.Values // a refresh sent first with the same token
		form   func(t *testing.T, e *env, form url.Values) url.Values

		wantStatus     int
		want           tokenResponse
		wantErr        oauthError
		wantRetryAfter string
		wantShared     bool // the answer is the one the first refresh got
	}{
		{name: "refreshed", wantStatus: http.StatusOK, want: bothScopes},
		{name: "refreshed again with the same token", before: scope(""), wantStatus: http.StatusOK, want: bothScopes, wantShared: true},
		{name: "same scopes in another order", before: scope(""), form: scope("speki:write speki:read"), wantStatus: http.StatusOK, want: bothScopes, wantShared: true},
		{name: "narrowed to read", form: scope("speki:read"), wantStatus: http.StatusOK, want: readScope},
		{name: "write alone keeps read", form: scope("speki:write"), wantStatus: http.StatusOK, want: bothScopes},
		{name: "unknown scopes ignored", form: scope("offline_access speki:read"), wantStatus: http.StatusOK, want: readScope},

		{name: "not a refresh token", form: set("refresh_token", "v1.k1.xyz"), wantStatus: http.StatusBadRequest, wantErr: invalidGrant("the refresh token is invalid or expired")},
		{name: "expired", wait: 90*24*time.Hour + skew + time.Second, wantStatus: http.StatusBadRequest, wantErr: invalidGrant("the refresh token is invalid or expired")},
		{
			name: "token from another speki with the same keys",
			form: func(t *testing.T, e *env, _ url.Values) url.Values {
				other := newEnv(t, withIssuer("https://other.example"), withKeys(e.keyring))
				clientID, resp := other.signedIn(t)
				return refreshForm(clientID, resp.RefreshToken)
			},
			wantStatus: http.StatusBadRequest, wantErr: invalidGrant("the refresh token was issued for another server"),
		},
		{
			name: "another client",
			form: func(t *testing.T, e *env, f url.Values) url.Values {
				f.Set("client_id", e.register(t, loopback))
				return f
			},
			wantStatus: http.StatusBadRequest, wantErr: invalidGrant("the refresh token was issued to another client"),
		},
		{name: "widened", access: "read", form: scope("speki:read speki:write"), wantStatus: http.StatusBadRequest,
			wantErr: oauthError{Error: "invalid_scope", Description: "a refresh can't add scopes, or drop speki:read"}},
		{name: "stored result has other scopes", before: scope(""), form: scope("speki:read"), wantStatus: http.StatusBadRequest,
			wantErr: oauthError{Error: "invalid_scope", Description: "this refresh token was just refreshed with other scopes"}},

		{name: "GitHub refuses the token", github: func(f *fakeGitHub) { f.refreshErr = fmt.Errorf("%w: bad_refresh_token", gh.ErrBadGrant) },
			wantStatus: http.StatusBadRequest, wantErr: invalidGrant("sign in again")},
		{name: "app misconfigured", github: func(f *fakeGitHub) { f.refreshErr = fmt.Errorf("%w: incorrect_client_credentials", gh.ErrAppConfig) },
			wantStatus: http.StatusServiceUnavailable, wantErr: unavailable("speki's GitHub app is misconfigured; tell the operator")},
		{name: "GitHub unreached", github: func(f *fakeGitHub) { f.refreshErr = fmt.Errorf("%w: connection refused", gh.ErrUnreached) },
			wantStatus: http.StatusServiceUnavailable, wantErr: unavailable("GitHub didn't answer; try again"), wantRetryAfter: "5"},
		{name: "GitHub's answer lost", github: func(f *fakeGitHub) { f.refreshErr = errors.New("connection reset") },
			wantStatus: http.StatusServiceUnavailable, wantErr: unavailable("GitHub didn't answer; try again"), wantRetryAfter: "5"},
		{
			name: "store down",
			store: func(s *fakeStore) {
				s.onCall(func(op, _ string) error {
					if op == "get" {
						return errStoreDown
					}
					return nil
				})
			},
			wantStatus: http.StatusInternalServerError, wantErr: oauthError{Error: "server_error", Description: "try again"},
		},
		{
			name: "lock fails",
			store: func(s *fakeStore) {
				s.onCall(func(op, key string) error {
					if op == "setnx" && strings.HasPrefix(key, "refresh-lock:") {
						return errStoreDown
					}
					return nil
				})
			},
			wantStatus: http.StatusInternalServerError, wantErr: oauthError{Error: "server_error", Description: "try again"},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				clientID := e.register(t, loopback)
				access := testCase.access
				if access == "" {
					access = "write"
				}
				first, rec := e.token(t, codeForm(clientID, e.signIn(t, authorizeURL(clientID, loopback, nil), access), loopback))
				if rec.Code != http.StatusOK {
					t.Fatalf("code exchange: got %d %s, want 200", rec.Code, rec.Body)
				}
				form := refreshForm(clientID, first.RefreshToken)
				var earlier tokenResponse
				if testCase.before != nil {
					earlier, rec = e.token(t, testCase.before(t, e, refreshForm(clientID, first.RefreshToken)))
					if rec.Code != http.StatusOK {
						t.Fatalf("first refresh: got %d %s, want 200", rec.Code, rec.Body)
					}
				}
				if testCase.form != nil {
					form = testCase.form(t, e, form)
				}
				if testCase.github != nil {
					e.github.set(testCase.github)
				}
				if testCase.store != nil {
					testCase.store(e.store)
				}
				time.Sleep(testCase.wait)

				rec = e.do(t, "/oauth/token", form)
				checkTokenAnswer(t, rec, testCase.wantStatus, testCase.want, testCase.wantErr, testCase.wantRetryAfter)
				if rec.Code != http.StatusOK {
					return
				}
				var resp tokenResponse
				_ = json.Unmarshal(rec.Body.Bytes(), &resp)
				if testCase.wantShared && resp != earlier {
					t.Errorf("got a new pair, want the one the first refresh got")
				}
				got, _ := e.identity(t, resp.AccessToken)
				want := oauth.Identity{UserID: 7, Login: "octocat", GitHubToken: "ghu_2", Client: "Claude Code", Write: strings.Contains(resp.Scope, "speki:write")}
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("identity mismatch (-want +got):\n%s", diff)
				}
			})
		})
	}
}

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

func refreshCount(tb testing.TB, result string) float64 {
	tb.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		tb.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "speki_refreshes_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "result" && l.GetValue() == result {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// GitHub refusing a refresh token signs the user out. When an earlier refresh with that token got no
// answer, GitHub most likely spent the token then: the sign-out is logged and counted as lost to the
// timeout, once.
func TestRefreshSignOutMetrics(t *testing.T) {
	// Not parallel: it reads the package's global counters, which parallel tests also move.
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

				lost, signedOut := refreshCount(t, "lost_to_timeout"), refreshCount(t, "signed_out")
				rec := e.do(t, "/oauth/token", form)
				checkTokenAnswer(t, rec, http.StatusBadRequest, tokenResponse{}, oauthError{Error: "invalid_grant", Description: "sign in again"}, "")
				got := map[string]any{
					"lost":       refreshCount(t, "lost_to_timeout") - lost,
					"signed out": refreshCount(t, "signed_out") - signedOut,
					"logged":     strings.Contains(e.logs.String(), "refresh lost to GitHub timeout"),
				}
				want := map[string]any{"lost": 0.0, "signed out": 1.0, "logged": false}
				if testCase.wantLost {
					want = map[string]any{"lost": 1.0, "signed out": 0.0, "logged": true}
				}
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("sign-out mismatch (-want +got):\n%s", diff)
				}
				if strings.Contains(e.logs.String(), "ghr_1") {
					t.Error("got the GitHub refresh token in the logs, want it kept out")
				}

				lost = refreshCount(t, "lost_to_timeout")
				_, _ = e.token(t, form)
				if got := refreshCount(t, "lost_to_timeout") - lost; got != 0 {
					t.Errorf("got a second refusal counted %v more lost, want 0", got)
				}
			})
		})
	}
}
