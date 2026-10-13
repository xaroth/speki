package oauth_test

import (
	"context"
	"errors"
	"fmt"
	"html"
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

const (
	stepChoose = `<li aria-current="step">Choose access</li>`
	stepGitHub = `<li aria-current="step">Sign in with GitHub</li>`
)

// pageHeaders are the headers every sign-in page carries.
var pageHeaders = map[string]string{
	"Content-Type":            "text/html; charset=utf-8",
	"Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; font-src 'self'; frame-ancestors 'none'",
	"X-Frame-Options":         "DENY",
	"Cache-Control":           "no-store",
	"Referrer-Policy":         "no-referrer",
}

func checkPageHeaders(tb testing.TB, rec *httptest.ResponseRecorder) {
	tb.Helper()
	got := map[string]string{}
	for k := range pageHeaders {
		got[k] = rec.Header().Get(k)
	}
	if diff := cmp.Diff(pageHeaders, got); diff != "" {
		tb.Errorf("page headers mismatch (-want +got):\n%s", diff)
	}
}

// checkErrorPage checks an error page's status, title and the step it marks.
func checkErrorPage(tb testing.TB, rec *httptest.ResponseRecorder, status int, title, step string) {
	tb.Helper()
	if rec.Code != status {
		tb.Fatalf("got status %d, want %d: %s", rec.Code, status, rec.Body)
	}
	checkPageHeaders(tb, rec)
	for _, want := range []string{"<h1>" + html.EscapeString(title) + "</h1>", step} {
		if !strings.Contains(rec.Body.String(), want) {
			tb.Errorf("got page without %q:\n%s", want, rec.Body)
		}
	}
}

// checkRedirect checks a redirect to base with exactly the query params.
func checkRedirect(tb testing.TB, rec *httptest.ResponseRecorder, status int, base string, params url.Values) {
	tb.Helper()
	if rec.Code != status {
		tb.Fatalf("got status %d, want %d: %s", rec.Code, status, rec.Body)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		tb.Fatal(err)
	}
	got := loc.Query()
	loc.RawQuery = ""
	if loc.String() != base {
		tb.Errorf("got redirect to %s, want %s", loc, base)
	}
	if diff := cmp.Diff(params, got); diff != "" {
		tb.Errorf("redirect params mismatch (-want +got):\n%s", diff)
	}
}

type wantCookie struct {
	Name     string
	MaxAge   int
	Path     string
	Secure   bool
	HTTPOnly bool
	SameSite http.SameSite
}

func checkCookies(tb testing.TB, rec *httptest.ResponseRecorder, want ...wantCookie) {
	tb.Helper()
	var got []wantCookie
	for _, c := range rec.Result().Cookies() {
		got = append(got, wantCookie{Name: c.Name, MaxAge: c.MaxAge, Path: c.Path, Secure: c.Secure, HTTPOnly: c.HttpOnly, SameSite: c.SameSite})
	}
	if diff := cmp.Diff(want, got); diff != "" {
		tb.Errorf("cookies mismatch (-want +got):\n%s", diff)
	}
}

func setCookie(name string) wantCookie {
	return wantCookie{Name: name, MaxAge: 600, Path: "/", Secure: true, HTTPOnly: true, SameSite: http.SameSiteLaxMode}
}

func clearedCookie(name string) wantCookie {
	return wantCookie{Name: name, MaxAge: -1, Path: "/", Secure: true, HTTPOnly: true, SameSite: http.SameSiteLaxMode}
}

func TestAuthorize(t *testing.T) {
	t.Parallel()
	const otherDoc = "https://claude.ai/oauth/other.json"
	registered := func(redirectURI string, extra url.Values) func(string) string {
		return func(clientID string) string { return authorizeURL(clientID, redirectURI, extra) }
	}
	hosted := func(clientID, redirectURI string) func(string) string {
		return func(string) string { return authorizeURL(clientID, redirectURI, nil) }
	}
	clientError := func(code, description string) url.Values {
		return url.Values{"error": {code}, "error_description": {description}, "state": {"client-state"}, "iss": {issuer}}
	}
	testCases := []struct {
		name   string
		opts   []option
		doc    http.HandlerFunc // served at otherDoc
		target func(clientID string) string

		wantGolden   string     // a consent page
		wantTitle    string     // an error page
		wantRedirect url.Values // an error sent back to the client
	}{
		{name: "registered client, loopback on any port", target: registered("http://127.0.0.1:6000/callback", nil), wantGolden: "consent_local.html"},
		{name: "redirect_uri left out, one registered", target: registered("", nil), wantGolden: "consent_local_default.html"},
		{name: "unknown scopes ignored", target: registered(loopback, url.Values{"scope": {"openid offline_access"}}), wantGolden: "consent_local_default.html"},
		{name: "read-only request", target: registered(loopback, url.Values{"scope": {"offline_access speki:read"}}), wantGolden: "consent_read_only.html"},
		{name: "metadata document client", target: hosted(claudeClient, claudeCallback), wantGolden: "consent_hosted.html"},
		{name: "named instance", opts: []option{withTitle("Home <notes>")}, target: registered(loopback, nil), wantGolden: "consent_titled.html"},

		{name: "state too long", target: registered(loopback, url.Values{"state": {strings.Repeat("s", 513)}}), wantTitle: "This sign-in link is broken."},
		{name: "unknown client", target: hosted("made-up", loopback), wantTitle: "This program isn't known to speki."},
		{name: "metadata host not allowed", target: hosted("https://evil.example/client.json", "https://evil.example/cb"), wantTitle: "This program isn't known to speki."},
		{name: "metadata URL with a query", target: hosted(claudeClient+"?n=1", claudeCallback), wantTitle: "This program isn't known to speki."},
		{name: "metadata document missing", target: hosted(otherDoc, claudeCallback), wantTitle: "This program isn't known to speki."},
		{name: "metadata host failing", doc: status(http.StatusServiceUnavailable), target: hosted(otherDoc, claudeCallback), wantTitle: "This program isn't known to speki."},
		{name: "metadata document for another client", doc: document(claudeClient, claudeCallback), target: hosted(otherDoc, claudeCallback), wantTitle: "This program isn't known to speki."},
		{name: "metadata redirect on another host", doc: document(otherDoc, "https://evil.example/cb"), target: hosted(otherDoc, "https://evil.example/cb"), wantTitle: "This program isn't known to speki."},
		{
			name: "metadata document too large",
			doc: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, `{"client_id":%q,"redirect_uris":[%q],"client_name":%q}`, otherDoc, claudeCallback, strings.Repeat("x", 64<<10))
			},
			target:    hosted(otherDoc, claudeCallback),
			wantTitle: "This program isn't known to speki.",
		},
		{name: "redirect not registered", target: registered("https://evil.example/cb", nil), wantTitle: "This sign-in link is broken."},
		{name: "loopback redirect with another path", target: registered("http://127.0.0.1/other", nil), wantTitle: "This sign-in link is broken."},

		{name: "token response type", target: registered(loopback, url.Values{"response_type": {"token"}}), wantRedirect: clientError("unsupported_response_type", "only the code flow is supported")},
		{name: "no PKCE challenge", target: registered(loopback, url.Values{"code_challenge": {""}}), wantRedirect: clientError("invalid_request", "PKCE with S256 is required")},
		{name: "PKCE challenge not base64url", target: registered(loopback, url.Values{"code_challenge": {strings.Repeat("!", 43)}}), wantRedirect: clientError("invalid_request", "PKCE with S256 is required")},
		{name: "plain PKCE", target: registered(loopback, url.Values{"code_challenge_method": {"plain"}}), wantRedirect: clientError("invalid_request", "PKCE with S256 is required")},
		{name: "another resource", target: registered(loopback, url.Values{"resource": {"https://other.example/mcp"}}), wantRedirect: clientError("invalid_target", "tokens are only issued for "+issuer+"/mcp")},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, testCase.opts...)
			if testCase.doc != nil {
				e.docs.serve(otherDoc, testCase.doc)
			}
			clientID := e.register(t, loopback)
			rec := e.do(t, testCase.target(clientID), nil)
			switch {
			case testCase.wantGolden != "":
				if rec.Code != http.StatusOK {
					t.Fatalf("got status %d, want 200: %s", rec.Code, rec.Body)
				}
				checkPageHeaders(t, rec)
				checkCookies(t, rec, setCookie(consentCookie))
				golden(t, testCase.wantGolden, rec.Body.String())
			case testCase.wantTitle != "":
				checkErrorPage(t, rec, http.StatusBadRequest, testCase.wantTitle, stepChoose)
				checkCookies(t, rec)
			default:
				checkRedirect(t, rec, http.StatusFound, loopback, testCase.wantRedirect)
			}
		})
	}
}

func TestConsent(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		prepare func(form url.Values, c *http.Cookie) (url.Values, *http.Cookie)
		wait    time.Duration
		deny    bool
		// an error page, if set
		wantTitle string
	}{
		{name: "allow"},
		{name: "deny", deny: true},
		{name: "no cookie", prepare: func(f url.Values, _ *http.Cookie) (url.Values, *http.Cookie) { return f, nil }, wantTitle: "This sign-in expired."},
		{
			name: "another browser's cookie",
			prepare: func(f url.Values, _ *http.Cookie) (url.Values, *http.Cookie) {
				return f, &http.Cookie{Name: consentCookie, Value: "someone-elses"}
			},
			wantTitle: "This sign-in expired.",
		},
		{
			name: "not a consent value",
			prepare: func(f url.Values, c *http.Cookie) (url.Values, *http.Cookie) {
				f.Set("consent", "v1.k1.xyz")
				return f, c
			},
			wantTitle: "This sign-in expired.",
		},
		{name: "expired", wait: 10*time.Minute + skew + time.Second, wantTitle: "This sign-in expired."},
		{
			name: "form too large",
			prepare: func(f url.Values, c *http.Cookie) (url.Values, *http.Cookie) {
				f.Set("padding", strings.Repeat("x", 64<<10))
				return f, c
			},
			wantTitle: "This sign-in link is broken.",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				clientID := e.register(t, loopback)
				page := e.do(t, authorizeURL(clientID, loopback, nil), nil)
				decision := "allow"
				if testCase.deny {
					decision = "deny"
				}
				form, c := consentForm(t, page, decision, "write"), cookie(page, consentCookie)
				if testCase.prepare != nil {
					form, c = testCase.prepare(form, c)
				}
				time.Sleep(testCase.wait)
				var cookies []*http.Cookie
				if c != nil {
					cookies = append(cookies, c)
				}
				rec := e.do(t, "/oauth/authorize", form, cookies...)

				switch {
				case testCase.wantTitle != "":
					checkErrorPage(t, rec, http.StatusBadRequest, testCase.wantTitle, stepChoose)
				case testCase.deny:
					checkRedirect(t, rec, http.StatusFound, loopback, url.Values{
						"error": {"access_denied"}, "error_description": {"the user declined"}, "state": {"client-state"}, "iss": {issuer},
					})
					checkCookies(t, rec, clearedCookie(consentCookie))
				default:
					loc, _ := url.Parse(rec.Header().Get("Location"))
					state, challenge := loc.Query().Get("state"), loc.Query().Get("code_challenge")
					if len(state) != 43 || len(challenge) != 43 {
						t.Errorf("got state %q and challenge %q, want 43 random characters each", state, challenge)
					}
					checkRedirect(t, rec, http.StatusSeeOther, "https://github.example/login/oauth/authorize", url.Values{
						"client_id":             {"Iv1"},
						"redirect_uri":          {issuer + "/oauth/github/callback"},
						"prompt":                {"select_account"},
						"code_challenge_method": {"S256"},
						"state":                 {state},
						"code_challenge":        {challenge},
					})
					checkCookies(t, rec, clearedCookie(consentCookie), setCookie(txCookie+state))
				}
			})
		})
	}
}

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
