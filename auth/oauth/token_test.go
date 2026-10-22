package oauth_test

import (
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

	"github.com/xaroth/speki/auth/oauth"
	"github.com/xaroth/speki/gh"
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
		wantShared     bool   // the answer is the one the first refresh got
		wantLogin      string // empty is octocat
	}{
		{name: "refreshed", wantStatus: http.StatusOK, want: bothScopes},
		{name: "refreshed again with the same token", before: scope(""), wantStatus: http.StatusOK, want: bothScopes, wantShared: true},
		{name: "same scopes in another order", before: scope(""), form: scope("speki:write speki:read"), wantStatus: http.StatusOK, want: bothScopes, wantShared: true},
		{name: "narrowed to read", form: scope("speki:read"), wantStatus: http.StatusOK, want: readScope},
		{name: "write alone keeps read", form: scope("speki:write"), wantStatus: http.StatusOK, want: bothScopes},
		{name: "unknown scopes ignored", form: scope("offline_access speki:read"), wantStatus: http.StatusOK, want: readScope},

		{name: "user renamed on GitHub", github: func(f *fakeGitHub) { f.me.Login = "octokitten" }, wantStatus: http.StatusOK, want: bothScopes, wantLogin: "octokitten"},
		{name: "user lookup fails", github: func(f *fakeGitHub) { f.meErr = errors.New("connection reset") }, wantStatus: http.StatusOK, want: bothScopes},
		{name: "token of another user", github: func(f *fakeGitHub) { f.me = gh.User{ID: 8, Login: "mallory"} }, wantStatus: http.StatusOK, want: bothScopes},

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
				login := testCase.wantLogin
				if login == "" {
					login = "octocat"
				}
				want := oauth.Identity{UserID: 7, Login: login, GitHubToken: "ghu_2", Client: "Claude Code", Write: strings.Contains(resp.Scope, "speki:write")}
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("identity mismatch (-want +got):\n%s", diff)
				}
			})
		})
	}
}

// BenchmarkCodeGrant measures exchanging a code at the token endpoint; each code works once, so signing
// in for the next one is left out of the timing.
func BenchmarkCodeGrant(b *testing.B) {
	e := newEnv(b)
	clientID := e.register(b, loopback)
	authorize := authorizeURL(clientID, loopback, nil)
	for b.Loop() {
		b.StopTimer()
		code := e.signIn(b, authorize, "write")
		b.StartTimer()
		if _, rec := e.token(b, codeForm(clientID, code, loopback)); rec.Code != http.StatusOK {
			b.Fatalf("got status %d, want 200", rec.Code)
		}
	}
}
