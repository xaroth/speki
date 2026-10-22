package oauth_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/xaroth/speki/auth/oauth"
)

func TestMiddleware(t *testing.T) {
	t.Parallel()
	const challenge = `Bearer resource_metadata="` + issuer + `/.well-known/oauth-protected-resource/mcp"`
	testCases := []struct {
		name       string
		token      func(t *testing.T, e *env) string
		wantStatus int
		want       oauth.Identity
	}{
		{
			name: "write token",
			token: func(t *testing.T, e *env) string {
				_, resp := e.signedIn(t)
				return resp.AccessToken
			},
			wantStatus: http.StatusOK,
			want:       oauth.Identity{UserID: 7, Login: "octocat", GitHubToken: "ghu_1", Client: "Claude Code", Write: true},
		},
		{name: "no token", token: func(*testing.T, *env) string { return "" }, wantStatus: http.StatusUnauthorized},
		{name: "not a speki token", token: func(*testing.T, *env) string { return "abc" }, wantStatus: http.StatusUnauthorized},
		{
			name: "expired",
			token: func(t *testing.T, e *env) string {
				_, resp := e.signedIn(t)
				time.Sleep(time.Hour + skew + time.Second)
				return resp.AccessToken
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "issued by another speki with the same keys",
			token: func(t *testing.T, e *env) string {
				other := newEnv(t, withIssuer("https://other.example"), withKeys(e.keyring))
				_, resp := other.signedIn(t)
				return resp.AccessToken
			},
			wantStatus: http.StatusUnauthorized,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				got, rec := e.identity(t, testCase.token(t, e))
				if rec.Code != testCase.wantStatus {
					t.Fatalf("got status %d, want %d", rec.Code, testCase.wantStatus)
				}
				if diff := cmp.Diff(testCase.want, got); diff != "" {
					t.Errorf("identity mismatch (-want +got):\n%s", diff)
				}
				if rec.Code == http.StatusUnauthorized && !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), challenge) {
					t.Errorf("got WWW-Authenticate %q, want it to start with %q", rec.Header().Get("WWW-Authenticate"), challenge)
				}
			})
		})
	}
}

func TestUnauthorized(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	rec := httptest.NewRecorder()
	e.server.Unauthorized(rec, "GitHub signed you out")
	want := `Bearer resource_metadata="` + issuer + `/.well-known/oauth-protected-resource/mcp", error="invalid_token"`
	if got := rec.Header().Get("WWW-Authenticate"); got != want {
		t.Errorf("got WWW-Authenticate %q, want %q", got, want)
	}
	if rec.Code != http.StatusUnauthorized || rec.Body.String() != "GitHub signed you out\n" {
		t.Errorf("got %d %q, want 401 with the reason", rec.Code, rec.Body)
	}
}
