package oauth_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/xaroth/speki/auth/oauth"
	"github.com/xaroth/speki/gh"
)

func TestCallback(t *testing.T) {
	t.Parallel()
	refuse := func(context.Context, string) error { return &oauth.Refusal{Reason: "not in the required organisation"} }
	checkFails := func(context.Context, string) error {
		return errors.New("user installations: GET https://api.github.com/user/installations: 502 Bad Gateway")
	}
	testCases := []struct {
		name   string
		opts   []option
		github func(f *fakeGitHub)
		query  func(state string) string
		cookie func(rec *httptest.ResponseRecorder, state string, other *httptest.ResponseRecorder, otherState string) *http.Cookie
		wait   time.Duration

		wantStatus   int
		wantTitle    string     // an error page
		wantLogin    bool       // the error page names the GitHub account
		wantText     string     // also on the error page
		wantRedirect url.Values // sent back to the client; a code is checked for, not compared
	}{
		{name: "signed in", wantStatus: http.StatusFound, wantRedirect: url.Values{"state": {"client-state"}, "iss": {issuer}}},
		{
			name:       "declined on GitHub",
			query:      func(state string) string { return "error=access_denied&state=" + state },
			wantStatus: http.StatusFound,
			wantRedirect: url.Values{
				"error": {"access_denied"}, "error_description": {"GitHub authorization was declined"}, "state": {"client-state"}, "iss": {issuer},
			},
		},
		{
			name:       "app misconfigured",
			github:     func(f *fakeGitHub) { f.exchangeErr = fmt.Errorf("%w: incorrect_client_credentials", gh.ErrAppConfig) },
			wantStatus: http.StatusBadGateway,
			wantTitle:  "Speki can't sign you in right now.",
		},
		{
			name:       "code refused",
			query:      func(state string) string { return "code=bad&state=" + state },
			wantStatus: http.StatusBadGateway,
			wantTitle:  "GitHub didn't accept the sign-in.",
		},
		{
			name:       "user lookup fails",
			github:     func(f *fakeGitHub) { f.meErr = errors.New("502 Bad Gateway") },
			wantStatus: http.StatusBadGateway,
			wantTitle:  "GitHub didn't answer.",
		},
		{name: "user refused", opts: []option{withCheckUser(refuse)}, wantStatus: http.StatusForbidden, wantTitle: "You can't use this speki.", wantLogin: true, wantText: "not in the required organisation"},
		{
			name:       "user check fails",
			opts:       []option{withCheckUser(checkFails)},
			wantStatus: http.StatusBadGateway,
			wantTitle:  "Speki couldn't check your access with GitHub.",
			wantLogin:  true,
		},
		{name: "no state", query: func(string) string { return "code=good" }, wantStatus: http.StatusBadRequest, wantTitle: "This sign-in expired."},
		{
			name:       "no transaction",
			cookie:     func(*httptest.ResponseRecorder, string, *httptest.ResponseRecorder, string) *http.Cookie { return nil },
			wantStatus: http.StatusBadRequest,
			wantTitle:  "This sign-in expired.",
		},
		{
			name: "another sign-in's transaction",
			cookie: func(_ *httptest.ResponseRecorder, state string, other *httptest.ResponseRecorder, otherState string) *http.Cookie {
				c := cookie(other, txCookie+otherState)
				c.Name = txCookie + state
				return c
			},
			wantStatus: http.StatusBadRequest,
			wantTitle:  "This sign-in expired.",
		},
		{name: "expired", wait: 10*time.Minute + skew + time.Second, wantStatus: http.StatusBadRequest, wantTitle: "This sign-in expired."},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t, testCase.opts...)
				if testCase.github != nil {
					e.github.set(testCase.github)
				}
				clientID := e.register(t, loopback)
				authorize := authorizeURL(clientID, loopback, nil)
				rec, state := e.toGitHub(t, authorize, "write")
				other, otherState := e.toGitHub(t, authorize, "write")
				c := cookie(rec, txCookie+state)
				if testCase.cookie != nil {
					c = testCase.cookie(rec, state, other, otherState)
				}
				query := "code=good&state=" + state
				if testCase.query != nil {
					query = testCase.query(state)
				}
				time.Sleep(testCase.wait)
				var cookies []*http.Cookie
				if c != nil {
					cookies = append(cookies, c)
				}
				rec = e.do(t, "/oauth/github/callback?"+query, nil, cookies...)

				if testCase.wantTitle != "" {
					checkErrorPage(t, rec, testCase.wantStatus, testCase.wantTitle, stepGitHub)
					if got := strings.Contains(rec.Body.String(), "<strong>@octocat</strong>"); got != testCase.wantLogin {
						t.Errorf("got login shown %v, want %v", got, testCase.wantLogin)
					}
					if !strings.Contains(rec.Body.String(), testCase.wantText) {
						t.Errorf("got page without %q", testCase.wantText)
					}
					if strings.Contains(rec.Body.String(), "api.github.com") {
						t.Error("got GitHub's error on the page, want it kept from the browser")
					}
					return
				}
				want := testCase.wantRedirect
				if code := rec.Header().Get("Location"); want.Get("error") == "" {
					loc, _ := url.Parse(code)
					if loc.Query().Get("code") == "" {
						t.Fatalf("got redirect %s, want a code", loc)
					}
					want = url.Values{"code": loc.Query()["code"], "state": want["state"], "iss": want["iss"]}
				}
				checkRedirect(t, rec, testCase.wantStatus, loopback, want)
				checkCookies(t, rec, clearedCookie(txCookie+state))
			})
		})
	}
}
