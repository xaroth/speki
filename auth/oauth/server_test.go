package oauth_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/eve-online-tools/speki/auth/oauth"
)

func TestServerMetadata(t *testing.T) {
	t.Parallel()
	resource := map[string]any{
		"resource":                 issuer + "/mcp",
		"authorization_servers":    []any{issuer},
		"scopes_supported":         []any{"speki:read", "speki:write"},
		"bearer_methods_supported": []any{"header"},
		"resource_name":            "speki",
	}
	testCases := []struct {
		name   string
		target string
		want   map[string]any
	}{
		{
			name:   "authorization server",
			target: "/.well-known/oauth-authorization-server",
			want: map[string]any{
				"issuer":                                         issuer,
				"authorization_endpoint":                         issuer + "/oauth/authorize",
				"token_endpoint":                                 issuer + "/oauth/token",
				"registration_endpoint":                          issuer + "/oauth/register",
				"response_types_supported":                       []any{"code"},
				"grant_types_supported":                          []any{"authorization_code", "refresh_token"},
				"code_challenge_methods_supported":               []any{"S256"},
				"token_endpoint_auth_methods_supported":          []any{"none"},
				"scopes_supported":                               []any{"speki:read", "speki:write"},
				"client_id_metadata_document_supported":          true,
				"authorization_response_iss_parameter_supported": true,
			},
		},
		{name: "protected resource", target: "/.well-known/oauth-protected-resource", want: resource},
		{name: "protected resource for /mcp", target: "/.well-known/oauth-protected-resource/mcp", want: resource},
	}
	e := newEnv(t, withIssuer(issuer+"/")) // a trailing slash in the config doesn't reach the URLs
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			rec := e.do(t, testCase.target, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("got status %d, want 200", rec.Code)
			}
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(testCase.want, got); diff != "" {
				t.Errorf("metadata mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

var fontURL = regexp.MustCompile(`url\((/oauth/fonts/[^)]+)\)`)

// Every font the pages name is served; nothing else is.
func TestFonts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	page := e.do(t, authorizeURL(claudeClient, claudeCallback, nil), nil)
	urls := fontURL.FindAllStringSubmatch(page.Body.String(), -1)
	if len(urls) == 0 {
		t.Fatal("got no fonts on the page, want some")
	}
	type served struct {
		Status       int
		ContentType  string
		CacheControl string
		NoSniff      string
		Empty        bool
	}
	testCases := []struct {
		name   string
		target string
		want   served
	}{
		{name: "missing", target: "/oauth/fonts/missing.woff2", want: served{Status: 404, ContentType: "text/plain; charset=utf-8", NoSniff: "nosniff"}},
		{name: "not a font", target: "/oauth/fonts/..%2Fconsent.html", want: served{Status: 404, ContentType: "text/plain; charset=utf-8", NoSniff: "nosniff"}},
	}
	for _, m := range urls {
		testCases = append(testCases, struct {
			name   string
			target string
			want   served
		}{name: m[1], target: m[1], want: served{Status: 200, ContentType: "font/woff2", CacheControl: "public, max-age=86400", NoSniff: "nosniff"}})
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			rec := e.do(t, testCase.target, nil)
			got := served{
				Status:       rec.Code,
				ContentType:  rec.Header().Get("Content-Type"),
				CacheControl: rec.Header().Get("Cache-Control"),
				NoSniff:      rec.Header().Get("X-Content-Type-Options"),
				Empty:        rec.Code == http.StatusOK && rec.Body.Len() == 0,
			}
			if diff := cmp.Diff(testCase.want, got); diff != "" {
				t.Errorf("response mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

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

// The access the user picks, and the access the client asked for, decide the scopes of the tokens.
func TestSignIn(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name      string
		scope     string // the client's scope parameter
		access    string // the user's choice on the consent page
		wantScope string
	}{
		{name: "write", access: "write", wantScope: "speki:read speki:write"},
		{name: "read", access: "read", wantScope: "speki:read"},
		{name: "write claimed on a read-only request", scope: "speki:read", access: "write", wantScope: "speki:read"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			clientID := e.register(t, loopback)
			code := e.signIn(t, authorizeURL(clientID, loopback, url.Values{"scope": {testCase.scope}}), testCase.access)
			resp, rec := e.token(t, codeForm(clientID, code, loopback))
			if rec.Code != http.StatusOK || resp.Scope != testCase.wantScope {
				t.Fatalf("got %d with scope %q, want 200 with %q", rec.Code, resp.Scope, testCase.wantScope)
			}
			id, _ := e.identity(t, resp.AccessToken)
			if wantWrite := strings.Contains(testCase.wantScope, "speki:write"); id.Write != wantWrite {
				t.Errorf("got Write %v, want %v", id.Write, wantWrite)
			}
		})
	}
}

func BenchmarkHandler(b *testing.B) {
	e := newEnv(b)
	clientID := e.register(b, loopback)
	authorize := authorizeURL(clientID, loopback, nil)
	page := e.do(b, authorize, nil)
	form := consentForm(b, page, "allow", "write")
	consentC := cookie(page, consentCookie)
	toGitHub, state := e.toGitHub(b, authorize, "write")
	tx := cookie(toGitHub, txCookie+state)
	signedInClient, tokens := e.signedIn(b)
	registration, _ := json.Marshal(map[string]any{"client_name": "Claude Code", "redirect_uris": []string{loopback}})

	benchmarks := []struct {
		name string
		do   func(b *testing.B) int
		want int
	}{
		{name: "server metadata", want: http.StatusOK, do: func(b *testing.B) int {
			return e.do(b, "/.well-known/oauth-authorization-server", nil).Code
		}},
		{name: "register", want: http.StatusCreated, do: func(b *testing.B) int {
			return e.doJSON(b, "/oauth/register", string(registration)).Code
		}},
		{name: "authorize", want: http.StatusOK, do: func(b *testing.B) int { return e.do(b, authorize, nil).Code }},
		{name: "consent", want: http.StatusSeeOther, do: func(b *testing.B) int {
			return e.do(b, "/oauth/authorize", form, consentC).Code
		}},
		{name: "callback", want: http.StatusFound, do: func(b *testing.B) int {
			return e.do(b, "/oauth/github/callback?code=good&state="+state, nil, tx).Code
		}},
		{name: "font", want: http.StatusOK, do: func(b *testing.B) int {
			return e.do(b, "/oauth/fonts/alegreya.woff2", nil).Code
		}},
		{name: "refresh", want: http.StatusOK, do: func(b *testing.B) int {
			resp, rec := e.token(b, refreshForm(signedInClient, tokens.RefreshToken))
			tokens = resp // each refresh token works once
			return rec.Code
		}},
		{name: "middleware", want: http.StatusOK, do: func(b *testing.B) int {
			_, rec := e.identity(b, tokens.AccessToken)
			return rec.Code
		}},
	}
	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			for b.Loop() {
				if got := bm.do(b); got != bm.want {
					b.Fatalf("got status %d, want %d", got, bm.want)
				}
			}
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
