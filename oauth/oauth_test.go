package oauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eve-online-tools/speki/gh"
	"github.com/eve-online-tools/speki/seal"
	"github.com/eve-online-tools/speki/store"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

const issuer = "https://speki.example.com"

type fakeGitHub struct {
	mu        sync.Mutex
	refreshes map[string]bool // used refresh tokens
	next      int
	slow      chan struct{} // when set, Refresh waits for it
	entered   chan struct{} // when set, Refresh sends on it once it's called
	down      bool          // when set, Refresh fails as if GitHub were unreachable
	appConfig bool          // when set, GitHub refuses the app's client secret
	during    func()        // when set, Refresh calls it after GitHub spent the old token
}

func (f *fakeGitHub) tokens() gh.UserTokens {
	f.next++
	return gh.UserTokens{
		AccessToken:    fmt.Sprintf("ghu_%d", f.next),
		AccessExpires:  time.Now().Add(8 * time.Hour),
		RefreshToken:   fmt.Sprintf("ghr_%d", f.next),
		RefreshExpires: time.Now().Add(180 * 24 * time.Hour),
	}
}

func (f *fakeGitHub) Exchange(_ context.Context, _ gh.OAuthConfig, code, verifier, redirect string) (gh.UserTokens, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if code != "good" || verifier == "" || redirect != issuer+"/oauth/github/callback" {
		return gh.UserTokens{}, gh.ErrBadGrant
	}
	return f.tokens(), nil
}

func (f *fakeGitHub) Refresh(ctx context.Context, _ gh.OAuthConfig, refresh string) (gh.UserTokens, error) {
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.slow != nil {
		<-f.slow
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return gh.UserTokens{}, errors.New("connection refused")
	}
	if f.appConfig {
		return gh.UserTokens{}, fmt.Errorf("%w: incorrect_client_credentials", gh.ErrAppConfig)
	}
	if f.refreshes[refresh] {
		return gh.UserTokens{}, gh.ErrBadGrant // GitHub refresh tokens work once
	}
	f.refreshes[refresh] = true
	if f.during != nil {
		f.during()
	}
	if ctx.Err() != nil { // the answer never reaches speki
		return gh.UserTokens{}, ctx.Err()
	}
	return f.tokens(), nil
}

func (f *fakeGitHub) Me(context.Context, string) (gh.User, error) {
	return gh.User{ID: 7, Login: "octocat"}, nil
}

func newKeys(t *testing.T) *seal.Keyring {
	t.Helper()
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	k, err := seal.ParseKeyring([]byte("keys:\n  - id: k1\n    secret: " + base64.StdEncoding.EncodeToString(b) + "\n    primary: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

type harness struct {
	t       *testing.T
	s       *Server
	handler http.Handler
	github  *fakeGitHub
	checkFn func(context.Context, string) error
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, github: &fakeGitHub{refreshes: map[string]bool{}}}
	h.s = New(Config{
		Issuer:              issuer,
		ClientMetadataHosts: []string{"claude.ai"},
		GitHub:              gh.OAuthConfig{ClientID: "Iv1", URL: "https://github.example"},
		CheckUser: func(ctx context.Context, token string) error {
			if h.checkFn != nil {
				return h.checkFn(ctx, token)
			}
			return nil
		},
	}, newKeys(t), h.github, store.NewMemory(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.s.clients.fetch = func(_ context.Context, u string) ([]byte, error) {
		switch u {
		case "https://claude.ai/oauth/client.json":
			return json.Marshal(map[string]any{"client_id": u, "client_name": "Claude",
				"redirect_uris": []string{"https://claude.ai/api/mcp/auth_callback", "http://127.0.0.1/callback"}})
		case "https://claude.ai/oauth/evil.json":
			return json.Marshal(map[string]any{"client_id": u, "client_name": "Claude", "redirect_uris": []string{"https://evil.example/cb"}})
		}
		return nil, errors.New("not found")
	}
	h.handler = h.s.Handler()
	return h
}

func (h *harness) do(method, target string, body io.Reader, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(method, target, body)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func (h *harness) register(redirect string) string {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(`{"client_name":"Claude Code","redirect_uris":["`+redirect+`"]}`))
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		h.t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}
	var resp struct {
		ClientID string `json:"client_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return resp.ClientID
}

const verifier = "a-client-verifier-that-is-long-enough-for-pkce"

func authorizeURL(clientID, redirect string, extra url.Values) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"code_challenge":        {challenge(verifier)},
		"code_challenge_method": {"S256"},
		"state":                 {"client-state"},
		"resource":              {issuer + "/mcp"},
	}
	for k, v := range extra {
		q[k] = v
	}
	return "/oauth/authorize?" + q.Encode()
}

var consentField = regexp.MustCompile(`name="consent" value="([^"]+)"`)

func cookie(rec *httptest.ResponseRecorder, prefix string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if strings.HasPrefix(c.Name, prefix) && c.MaxAge >= 0 {
			return c
		}
	}
	return nil
}

// signIn runs the browser part of the flow and returns the client's code.
func (h *harness) signIn(clientID, redirect, access string) string {
	h.t.Helper()
	rec := h.do(http.MethodGet, authorizeURL(clientID, redirect, nil), nil)
	if rec.Code != http.StatusOK {
		h.t.Fatalf("authorize: %d %s", rec.Code, rec.Body)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'none'") || rec.Header().Get("X-Frame-Options") != "DENY" {
		h.t.Error("consent page can be framed")
	}
	// Chromium applies form-action to the redirect that follows the form, which leaves speki's origin
	// (to GitHub, or to the client on Cancel). Allowing only 'self' blocks sign-in.
	if strings.Contains(csp, "form-action") {
		h.t.Errorf("consent page CSP restricts where the form may lead: %s", csp)
	}
	m := consentField.FindStringSubmatch(rec.Body.String())
	if m == nil {
		h.t.Fatal("no consent field on the page")
	}
	form := url.Values{"consent": {m[1]}, "access": {access}, "decision": {"allow"}}
	rec = h.do(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode()), cookie(rec, consentCookie))
	if rec.Code != http.StatusSeeOther {
		h.t.Fatalf("consent: %d %s", rec.Code, rec.Body)
	}
	toGitHub, _ := url.Parse(rec.Header().Get("Location"))
	if toGitHub.Host != "github.example" || toGitHub.Query().Get("code_challenge_method") != "S256" {
		h.t.Fatalf("not sent to GitHub: %s", toGitHub)
	}
	state := toGitHub.Query().Get("state")
	rec = h.do(http.MethodGet, "/oauth/github/callback?code=good&state="+state, nil, cookie(rec, txCookie+state))
	if rec.Code != http.StatusFound {
		h.t.Fatalf("callback: %d %s", rec.Code, rec.Body)
	}
	back, _ := url.Parse(rec.Header().Get("Location"))
	if back.Query().Get("state") != "client-state" || back.Query().Get("iss") != issuer {
		h.t.Errorf("back to client: %s", back)
	}
	return back.Query().Get("code")
}

func (h *harness) tokens(form url.Values) (tokenResponse, *httptest.ResponseRecorder) {
	h.t.Helper()
	rec := h.do(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	var resp tokenResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return resp, rec
}

func (h *harness) identity(access string) (Identity, int) {
	h.t.Helper()
	var got Identity
	handler := h.s.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, _ = IdentityFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return got, rec.Code
}

func TestFullFlowWithRegisteredClient(t *testing.T) {
	h := newHarness(t)
	clientID := h.register("http://127.0.0.1:5555/callback")

	code := h.signIn(clientID, "http://127.0.0.1:6000/callback", "write") // loopback: any port
	if n := testutil.ToFloat64(h.s.signIns.WithLabelValues("ok")); n != 1 {
		t.Errorf("speki_signins_total{result=ok} = %v", n)
	}
	resp, rec := h.tokens(url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {"http://127.0.0.1:6000/callback"}, "code_verifier": {verifier}, "resource": {issuer + "/mcp"},
	})
	if rec.Code != http.StatusOK || resp.AccessToken == "" || resp.RefreshToken == "" || resp.Scope != "speki:read speki:write" {
		t.Fatalf("token: %d %s", rec.Code, rec.Body)
	}
	if resp.ExpiresIn <= 0 || resp.ExpiresIn > 3600 {
		t.Errorf("expires_in = %d", resp.ExpiresIn)
	}

	id, status := h.identity(resp.AccessToken)
	if status != http.StatusOK || id.Login != "octocat" || id.GitHubToken != "ghu_1" || !id.Write || id.Client != "Claude Code" {
		t.Errorf("identity = %+v (%d)", id, status)
	}

	// The code works once.
	_, rec = h.tokens(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}, "redirect_uri": {"http://127.0.0.1:6000/callback"}, "code_verifier": {verifier}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "already used") {
		t.Errorf("replayed code: %d %s", rec.Code, rec.Body)
	}

	// Refreshing gives a new pair; a second refresh with the same token, right after, gets the same answer.
	refresh := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {resp.RefreshToken}, "client_id": {clientID}}
	first, rec := h.tokens(refresh)
	if rec.Code != http.StatusOK || first.AccessToken == resp.AccessToken {
		t.Fatalf("refresh: %d %s", rec.Code, rec.Body)
	}
	again, rec := h.tokens(refresh)
	if rec.Code != http.StatusOK || again.AccessToken != first.AccessToken {
		t.Errorf("repeated refresh: %d, same answer %v", rec.Code, again.AccessToken == first.AccessToken)
	}
	if id, _ := h.identity(first.AccessToken); id.GitHubToken != "ghu_2" {
		t.Errorf("refreshed identity = %+v", id)
	}

	// Narrowing the scope works, widening doesn't.
	narrowed, rec := h.tokens(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {first.RefreshToken}, "client_id": {clientID}, "scope": {"speki:read"}})
	if rec.Code != http.StatusOK || narrowed.Scope != "speki:read" {
		t.Errorf("narrowed: %d %s", rec.Code, rec.Body)
	}
	_, rec = h.tokens(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {narrowed.RefreshToken}, "client_id": {clientID}, "scope": {"speki:read speki:write"}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_scope") {
		t.Errorf("widened: %d %s", rec.Code, rec.Body)
	}
}

// signedIn runs the whole flow and returns the client ID and its first tokens.
func (h *harness) signedIn() (string, tokenResponse) {
	h.t.Helper()
	clientID := h.register("http://127.0.0.1/callback")
	code := h.signIn(clientID, "http://127.0.0.1/callback", "write")
	resp, rec := h.tokens(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}, "redirect_uri": {"http://127.0.0.1/callback"}, "code_verifier": {verifier}})
	if rec.Code != http.StatusOK {
		h.t.Fatalf("token: %d %s", rec.Code, rec.Body)
	}
	return clientID, resp
}

func TestConcurrentRefreshesBothSucceed(t *testing.T) {
	h := newHarness(t)
	clientID, first := h.signedIn()
	h.github.slow = make(chan struct{})
	h.github.entered = make(chan struct{}, 2)
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {first.RefreshToken}, "client_id": {clientID}}

	type result struct {
		resp tokenResponse
		code int
	}
	results := make(chan result, 2)
	refresh := func() {
		resp, rec := h.tokens(form)
		results <- result{resp, rec.Code}
	}
	contended := make(chan struct{}, 10)
	h.s.store = watchedStore{Memory: store.NewMemory(), contended: contended}
	go refresh()
	<-h.github.entered // the first holds the lock and waits on GitHub
	go refresh()
	<-contended // the second found the lock taken, and waits for the first's result
	close(h.github.slow)
	a, b := <-results, <-results
	if a.code != http.StatusOK || b.code != http.StatusOK || a.resp.AccessToken != b.resp.AccessToken {
		t.Errorf("concurrent refreshes: %d, %d, same answer %v", a.code, b.code, a.resp.AccessToken == b.resp.AccessToken)
	}
}

// A client that disconnects while speki waits for GitHub retries with the same token. GitHub has spent
// the old refresh token by then, so the retry only works if speki finished the refresh anyway.
func TestRefreshSurvivesADroppedConnection(t *testing.T) {
	h := newHarness(t)
	clientID, first := h.signedIn()
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {first.RefreshToken}, "client_id": {clientID}}

	ctx, cancel := context.WithCancel(t.Context())
	h.github.during = cancel
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.handler.ServeHTTP(httptest.NewRecorder(), req)
	h.github.during = nil

	resp, rec := h.tokens(form)
	if rec.Code != http.StatusOK || resp.AccessToken == "" {
		t.Errorf("retry after a dropped connection: %d %s", rec.Code, rec.Body)
	}
}

func TestRefreshWithBadAppConfigIsTemporary(t *testing.T) {
	h := newHarness(t)
	clientID, first := h.signedIn()
	h.github.appConfig = true
	_, rec := h.tokens(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {first.RefreshToken}, "client_id": {clientID}})
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "invalid_grant") {
		t.Errorf("bad app config: %d %s", rec.Code, rec.Body)
	}
}

func TestRefreshWhileGitHubIsDownIsTemporary(t *testing.T) {
	h := newHarness(t)
	clientID, first := h.signedIn()
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {first.RefreshToken}, "client_id": {clientID}}

	h.github.down = true
	_, rec := h.tokens(form)
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "invalid_grant") {
		t.Errorf("GitHub down: %d %s", rec.Code, rec.Body)
	}
	h.github.down = false
	if _, rec := h.tokens(form); rec.Code != http.StatusOK {
		t.Errorf("after GitHub is back: %d %s", rec.Code, rec.Body)
	}
}

func TestRefreshRejects(t *testing.T) {
	h := newHarness(t)
	clientID, first := h.signedIn()

	foreign, err := h.s.keys.Seal(seal.Refresh, refreshValue{ClientID: clientID, Audience: "https://other.example/mcp", Scopes: []string{ScopeRead}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, rec := h.tokens(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {foreign}, "client_id": {clientID}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "another server") {
		t.Errorf("foreign refresh token: %d %s", rec.Code, rec.Body)
	}

	// Asking for write alone still keeps read.
	resp, rec := h.tokens(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {first.RefreshToken}, "client_id": {clientID}, "scope": {"speki:write"}})
	if rec.Code != http.StatusOK || resp.Scope != "speki:read speki:write" {
		t.Errorf("write-only request: %d %q", rec.Code, resp.Scope)
	}
}

func TestOverlongRequestIsRefused(t *testing.T) {
	h := newHarness(t)
	clientID := h.register("http://127.0.0.1/callback")
	rec := h.do(http.MethodGet, authorizeURL(clientID, "http://127.0.0.1/callback", url.Values{"state": {strings.Repeat("s", maxState+1)}}), nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("long state: %d", rec.Code)
	}
}

func TestReadOnlySignIn(t *testing.T) {
	h := newHarness(t)
	clientID := h.register("http://127.0.0.1/callback")
	code := h.signIn(clientID, "http://127.0.0.1/callback", "read")
	resp, _ := h.tokens(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}, "redirect_uri": {"http://127.0.0.1/callback"}, "code_verifier": {verifier}})
	if resp.Scope != "speki:read" {
		t.Errorf("scope = %q", resp.Scope)
	}
	if id, _ := h.identity(resp.AccessToken); id.Write {
		t.Error("read-only token has write access")
	}
}

func TestMetadataDocumentClient(t *testing.T) {
	h := newHarness(t)
	clientID := "https://claude.ai/oauth/client.json"
	code := h.signIn(clientID, "https://claude.ai/api/mcp/auth_callback", "write")
	_, rec := h.tokens(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}, "redirect_uri": {"https://claude.ai/api/mcp/auth_callback"}, "code_verifier": {verifier}})
	if rec.Code != http.StatusOK {
		t.Errorf("token: %d %s", rec.Code, rec.Body)
	}
}

func TestAuthorizeRejects(t *testing.T) {
	h := newHarness(t)
	local := h.register("http://127.0.0.1/callback")
	tests := []struct {
		name       string
		target     string
		wantStatus int
		wantError  string // OAuth error in the redirect, or text on the error page
	}{
		{"unknown client", authorizeURL("made-up", "http://127.0.0.1/callback", nil), http.StatusBadRequest, "isn&#39;t known"},
		{"metadata host not allowed", authorizeURL("https://evil.example/client.json", "https://evil.example/cb", nil), http.StatusBadRequest, "isn&#39;t known"},
		{"metadata redirect on another host", authorizeURL("https://claude.ai/oauth/evil.json", "https://evil.example/cb", nil), http.StatusBadRequest, "isn&#39;t known"},
		{"redirect not registered", authorizeURL(local, "https://evil.example/cb", nil), http.StatusBadRequest, "broken"},
		{"loopback with another path", authorizeURL(local, "http://127.0.0.1/other", nil), http.StatusBadRequest, "broken"},
		{"no PKCE", authorizeURL(local, "http://127.0.0.1/callback", url.Values{"code_challenge": {""}}), http.StatusFound, "invalid_request"},
		{"plain PKCE", authorizeURL(local, "http://127.0.0.1/callback", url.Values{"code_challenge_method": {"plain"}}), http.StatusFound, "invalid_request"},
		{"other resource", authorizeURL(local, "http://127.0.0.1/callback", url.Values{"resource": {"https://other.example/mcp"}}), http.StatusFound, "invalid_target"},
		{"token response type", authorizeURL(local, "http://127.0.0.1/callback", url.Values{"response_type": {"token"}}), http.StatusFound, "unsupported_response_type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := h.do(http.MethodGet, tt.target, nil)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body)
			}
			if rec.Code == http.StatusFound {
				loc, _ := url.Parse(rec.Header().Get("Location"))
				if loc.Query().Get("error") != tt.wantError || loc.Host != "127.0.0.1" {
					t.Errorf("redirect = %s", loc)
				}
				return
			}
			if !strings.Contains(rec.Body.String(), tt.wantError) {
				t.Errorf("page doesn't mention %q: %s", tt.wantError, rec.Body)
			}
		})
	}
}

func TestConsentAndCallbackRejects(t *testing.T) {
	h := newHarness(t)
	clientID := h.register("http://127.0.0.1/callback")
	page := h.do(http.MethodGet, authorizeURL(clientID, "http://127.0.0.1/callback", nil), nil)
	consent := consentField.FindStringSubmatch(page.Body.String())[1]
	good := cookie(page, consentCookie)

	form := url.Values{"consent": {consent}, "access": {"write"}, "decision": {"allow"}}
	if rec := h.do(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode())); rec.Code != http.StatusBadRequest {
		t.Errorf("consent without cookie: %d", rec.Code)
	}
	other := &http.Cookie{Name: consentCookie, Value: "someone-elses"}
	if rec := h.do(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode()), other); rec.Code != http.StatusBadRequest {
		t.Errorf("consent with another browser's cookie: %d", rec.Code)
	}
	deny := url.Values{"consent": {consent}, "decision": {"deny"}}
	rec := h.do(http.MethodPost, "/oauth/authorize", strings.NewReader(deny.Encode()), good)
	if loc, _ := url.Parse(rec.Header().Get("Location")); loc == nil || loc.Query().Get("error") != "access_denied" {
		t.Errorf("deny: %d %s", rec.Code, rec.Header().Get("Location"))
	}

	if rec := h.do(http.MethodGet, "/oauth/github/callback?code=good&state=nonce", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("callback without transaction: %d", rec.Code)
	}

	h.checkFn = func(context.Context, string) error { return &Refusal{Reason: "not in the required organisation"} }
	page = h.do(http.MethodGet, authorizeURL(clientID, "http://127.0.0.1/callback", nil), nil)
	consent = consentField.FindStringSubmatch(page.Body.String())[1]
	rec = h.do(http.MethodPost, "/oauth/authorize", strings.NewReader(url.Values{"consent": {consent}, "decision": {"allow"}}.Encode()), cookie(page, consentCookie))
	state, _ := url.Parse(rec.Header().Get("Location"))
	nonce := state.Query().Get("state")
	rec = h.do(http.MethodGet, "/oauth/github/callback?code=good&state="+nonce, nil, cookie(rec, txCookie+nonce))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "required organisation") {
		t.Errorf("refused user: %d %s", rec.Code, rec.Body)
	}
}

func TestTokenRejects(t *testing.T) {
	h := newHarness(t)
	clientID := h.register("http://127.0.0.1/callback")
	code := h.signIn(clientID, "http://127.0.0.1/callback", "write")
	other := h.register("http://127.0.0.1/other")

	tests := []struct {
		name string
		form url.Values
		want string
	}{
		{"wrong verifier", url.Values{"code_verifier": {"wrong"}}, "PKCE"},
		{"other client", url.Values{"client_id": {other}}, "another client"},
		{"other redirect", url.Values{"redirect_uri": {"http://127.0.0.1/other"}}, "redirect_uri"},
		{"other resource", url.Values{"resource": {"https://other.example/mcp"}}, "invalid_target"},
		{"garbage code", url.Values{"code": {"v1.k1.xyz"}}, "invalid or expired"},
		{"unknown grant", url.Values{"grant_type": {"password"}}, "unsupported_grant_type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}, "redirect_uri": {"http://127.0.0.1/callback"}, "code_verifier": {verifier}}
			for k, v := range tt.form {
				form[k] = v
			}
			_, rec := h.tokens(form)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tt.want) {
				t.Errorf("%d %s", rec.Code, rec.Body)
			}
		})
	}
	// None of those used the code up.
	_, rec := h.tokens(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}, "redirect_uri": {"http://127.0.0.1/callback"}, "code_verifier": {verifier}})
	if rec.Code != http.StatusOK {
		t.Errorf("valid exchange after failed ones: %d %s", rec.Code, rec.Body)
	}
}

func TestExpiredCode(t *testing.T) {
	h := newHarness(t)
	clientID := h.register("http://127.0.0.1/callback")
	h.s.now = func() time.Time { return time.Now().Add(-5 * time.Minute) } // the code is sealed in the past
	code := h.signIn(clientID, "http://127.0.0.1/callback", "write")
	h.s.now = time.Now
	_, rec := h.tokens(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}, "redirect_uri": {"http://127.0.0.1/callback"}, "code_verifier": {verifier}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "expired") {
		t.Errorf("expired code: %d %s", rec.Code, rec.Body)
	}
}

func TestMiddlewareRejects(t *testing.T) {
	h := newHarness(t)
	for name, token := range map[string]string{
		"none":    "",
		"garbage": "abc",
	} {
		_, status := h.identity(token)
		if status != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, status)
		}
	}

	// A speki token issued for another server.
	foreign, err := h.s.keys.Seal(seal.Access, accessValue{UserID: 1, Audience: "https://other.example/mcp"}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, status := h.identity(foreign); status != http.StatusUnauthorized {
		t.Errorf("foreign audience: %d", status)
	}

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	rec := httptest.NewRecorder()
	h.s.Middleware(http.NotFoundHandler()).ServeHTTP(rec, req)
	if !strings.Contains(rec.Header().Get("WWW-Authenticate"), "resource_metadata=") {
		t.Errorf("401 without resource metadata: %q", rec.Header().Get("WWW-Authenticate"))
	}
}

func TestRegisterRejects(t *testing.T) {
	h := newHarness(t)
	for name, body := range map[string]string{
		"https redirect": `{"client_name":"x","redirect_uris":["https://evil.example/cb"]}`,
		"no redirects":   `{"client_name":"x","redirect_uris":[]}`,
		"not json":       `{`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
}

func TestMetadata(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodGet, "/.well-known/oauth-authorization-server", nil)
	var meta map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &meta)
	if meta["issuer"] != issuer || meta["client_id_metadata_document_supported"] != true {
		t.Errorf("server metadata = %v", meta)
	}
	rec = h.do(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil)
	if !strings.Contains(rec.Body.String(), issuer+"/mcp") {
		t.Errorf("resource metadata = %s", rec.Body)
	}
}

func TestPublicAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"93.184.216.34": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.0.0.1": false, "192.168.1.1": false, "169.254.169.254": false,
		"100.64.0.1": false, "0.0.0.0": false, "::1": false, "::": false, "fe80::1": false,
		"::ffff:127.0.0.1": false, "64:ff9b::7f00:1": false, "fd00::1": false,
		"0.1.2.3": false, "192.0.0.1": false, "198.18.0.1": false, "240.0.0.1": false, "::127.0.0.1": false,
		"2002:7f00:1::": false, "64:ff9b:1::1": false, "100::1": false, "2001:db8::1": false,
	} {
		if got := publicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("publicAddr(%s) = %v", addr, got)
		}
	}
}

func TestOneLine(t *testing.T) {
	if got := oneLine("Claude\nCo-authored-by: x\u0085"); got != "ClaudeCo-authored-by: x" {
		t.Errorf("oneLine = %q", got)
	}
	if got := oneLine("Claude\u202edoC\u2066x\U000e0041"); got != "ClaudedoCx" {
		t.Errorf("oneLine kept characters that hide text: %q", got)
	}
	if got := oneLine("team \U0001f468\u200d\U0001f469"); got != "team \U0001f468\u200d\U0001f469" {
		t.Errorf("oneLine dropped a zero-width joiner: %q", got)
	}
}

// authorizeTo runs authorize with extra parameters and returns the response.
func (h *harness) authorizeTo(clientID string, extra url.Values) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(http.MethodGet, authorizeURL(clientID, "http://127.0.0.1/callback", extra), nil)
}

func TestAuthorizeChecksScopesAndChallenge(t *testing.T) {
	h := newHarness(t)
	clientID := h.register("http://127.0.0.1/callback")

	rec := h.authorizeTo(clientID, url.Values{"code_challenge": {"x"}})
	back, _ := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusFound || back.Query().Get("error") != "invalid_request" {
		t.Errorf("short challenge: %d %s", rec.Code, rec.Header().Get("Location"))
	}

	// Scopes speki doesn't know are ignored; only speki:read without speki:write limits the choice.
	for scope, write := range map[string]bool{"openid offline_access": true, "offline_access speki:read": false, "speki:read speki:write": true} {
		page := h.authorizeTo(clientID, url.Values{"scope": {scope}})
		if page.Code != http.StatusOK || strings.Contains(page.Body.String(), `value="write"`) != write {
			t.Errorf("scope %q: %d, write offered: %v", scope, page.Code, !write)
		}
	}

	// A client asking only for speki:read gets no write option, and no write access even if the form
	// claims it.
	page := h.authorizeTo(clientID, url.Values{"scope": {"speki:read"}})
	if strings.Contains(page.Body.String(), `value="write"`) {
		t.Error("write offered to a read-only request")
	}
	consent := consentField.FindStringSubmatch(page.Body.String())[1]
	rec = h.do(http.MethodPost, "/oauth/authorize", strings.NewReader(url.Values{"consent": {consent}, "access": {"write"}, "decision": {"allow"}}.Encode()), cookie(page, consentCookie))
	loc, _ := url.Parse(rec.Header().Get("Location"))
	nonce := loc.Query().Get("state")
	rec = h.do(http.MethodGet, "/oauth/github/callback?code=good&state="+nonce, nil, cookie(rec, txCookie+nonce))
	back, _ = url.Parse(rec.Header().Get("Location"))
	resp, _ := h.tokens(url.Values{"grant_type": {"authorization_code"}, "code": {back.Query().Get("code")}, "client_id": {clientID}, "redirect_uri": {"http://127.0.0.1/callback"}, "code_verifier": {verifier}})
	if resp.Scope != "speki:read" {
		t.Errorf("scope = %q, want speki:read", resp.Scope)
	}
}

func TestTokenNeedsRedirectURI(t *testing.T) {
	h := newHarness(t)
	clientID := h.register("http://127.0.0.1/callback")
	code := h.signIn(clientID, "http://127.0.0.1/callback", "write")
	_, rec := h.tokens(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}, "code_verifier": {verifier}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "redirect_uri") {
		t.Errorf("without redirect_uri: %d %s", rec.Code, rec.Body)
	}

	// A client that named no redirect URI at authorize (it has only one) needn't name it here either.
	page := h.do(http.MethodGet, strings.Replace(authorizeURL(clientID, "", nil), "redirect_uri=&", "", 1), nil)
	consent := consentField.FindStringSubmatch(page.Body.String())
	if consent == nil {
		t.Fatalf("authorize without redirect_uri: %d", page.Code)
	}
	rec = h.do(http.MethodPost, "/oauth/authorize", strings.NewReader(url.Values{"consent": {consent[1]}, "decision": {"allow"}}.Encode()), cookie(page, consentCookie))
	loc, _ := url.Parse(rec.Header().Get("Location"))
	nonce := loc.Query().Get("state")
	rec = h.do(http.MethodGet, "/oauth/github/callback?code=good&state="+nonce, nil, cookie(rec, txCookie+nonce))
	back, _ := url.Parse(rec.Header().Get("Location"))
	_, rec = h.tokens(url.Values{"grant_type": {"authorization_code"}, "code": {back.Query().Get("code")}, "client_id": {clientID}, "code_verifier": {verifier}})
	if rec.Code != http.StatusOK {
		t.Errorf("no redirect_uri at either end: %d %s", rec.Code, rec.Body)
	}
}

func TestStoredRefreshNeverWidens(t *testing.T) {
	h := newHarness(t)
	clientID, first := h.signedIn()
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {first.RefreshToken}, "client_id": {clientID}}
	if _, rec := h.tokens(form); rec.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", rec.Code, rec.Body)
	}
	form.Set("scope", ScopeWrite+" "+ScopeRead) // the same scopes in another order get the stored result
	if _, rec := h.tokens(form); rec.Code != http.StatusOK {
		t.Errorf("same scopes, other order: %d %s", rec.Code, rec.Body)
	}
	form.Set("scope", ScopeRead)
	resp, rec := h.tokens(form) // the stored result has both scopes
	if rec.Code == http.StatusOK && strings.Contains(resp.Scope, ScopeWrite) {
		t.Errorf("a read-only refresh got %q", resp.Scope)
	}
	if strings.Contains(rec.Body.String(), "invalid_grant") {
		t.Errorf("answered invalid_grant, which signs the user out: %s", rec.Body)
	}
}

func TestRegistrationStaysUsable(t *testing.T) {
	h := newHarness(t)
	var uris []string
	for i := range 4 {
		uris = append(uris, fmt.Sprintf("http://127.0.0.1/%d/%s", i, strings.Repeat("x", 180)))
	}
	body, _ := json.Marshal(map[string]any{"client_name": strings.Repeat("n", 64), "redirect_uris": uris})
	rec := h.do(http.MethodPost, "/oauth/register", strings.NewReader(string(body)))
	var resp struct {
		ClientID string `json:"client_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code == http.StatusCreated && len(resp.ClientID) > maxClientID {
		t.Errorf("registered a client ID of %d bytes, which authorize refuses", len(resp.ClientID))
	}
}

func TestCallbackChecks(t *testing.T) {
	h := newHarness(t)
	clientID := h.register("http://127.0.0.1/callback")
	toGitHub := func() (*httptest.ResponseRecorder, string) {
		page := h.authorizeTo(clientID, nil)
		consent := consentField.FindStringSubmatch(page.Body.String())[1]
		rec := h.do(http.MethodPost, "/oauth/authorize", strings.NewReader(url.Values{"consent": {consent}, "decision": {"allow"}}.Encode()), cookie(page, consentCookie))
		loc, _ := url.Parse(rec.Header().Get("Location"))
		return rec, loc.Query().Get("state")
	}

	// A transaction cookie for another sign-in doesn't fit this one.
	recA, nonceA := toGitHub()
	_, nonceB := toGitHub()
	other := cookie(recA, txCookie+nonceA)
	other.Name = txCookie + nonceB
	if rec := h.do(http.MethodGet, "/oauth/github/callback?code=good&state="+nonceB, nil, other); rec.Code != http.StatusBadRequest {
		t.Errorf("another sign-in's transaction: %d", rec.Code)
	}

	// GitHub failing during the access check shows a generic page, not GitHub's words.
	h.checkFn = func(context.Context, string) error {
		return errors.New("user installations: GET https://api.github.com/user/installations: 502 Bad Gateway")
	}
	rec, nonce := toGitHub()
	rec = h.do(http.MethodGet, "/oauth/github/callback?code=good&state="+nonce, nil, cookie(rec, txCookie+nonce))
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "api.github.com") {
		t.Errorf("GitHub failure: %d %s", rec.Code, rec.Body)
	}
}

func TestExpiredValues(t *testing.T) {
	h := newHarness(t)
	past := time.Now().Add(-time.Hour)
	clientID, _ := h.signedIn()

	access, _ := h.s.keys.Seal(seal.Access, accessValue{UserID: 7, Login: "octocat", Token: "ghu_1", Audience: issuer + "/mcp"}, past)
	if _, status := h.identity(access); status != http.StatusUnauthorized {
		t.Errorf("expired access token: %d", status)
	}
	refresh, _ := h.s.keys.Seal(seal.Refresh, refreshValue{UserID: 7, Refresh: "ghr_x", ClientID: clientID, Audience: issuer + "/mcp"}, past)
	if _, rec := h.tokens(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}}); rec.Code != http.StatusBadRequest {
		t.Errorf("expired refresh token: %d", rec.Code)
	}
	consent, _ := h.s.keys.Seal(seal.Consent, consentValue{CookieHash: hash("c")}, past)
	rec := h.do(http.MethodPost, "/oauth/authorize", strings.NewReader(url.Values{"consent": {consent}, "decision": {"allow"}}.Encode()), &http.Cookie{Name: consentCookie, Value: "c"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expired consent: %d", rec.Code)
	}
	tx, _ := h.s.keys.Seal(seal.Transaction, transaction{Nonce: "n"}, past)
	rec = h.do(http.MethodGet, "/oauth/github/callback?code=good&state=n", nil, &http.Cookie{Name: txCookie + "n", Value: tx})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expired transaction: %d", rec.Code)
	}
}

// A client that hangs up doesn't fail the fetch for others, and a network failure is only remembered for
// a few seconds.
func TestMetadataFetchFailures(t *testing.T) {
	h := newHarness(t)
	fail := false
	h.s.clients.fetch = func(ctx context.Context, u string) ([]byte, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if fail {
			return nil, errors.New("connection reset")
		}
		return json.Marshal(map[string]any{"client_id": u, "client_name": "Claude", "redirect_uris": []string{"https://claude.ai/cb"}})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/oauth/authorize?client_id=https://claude.ai/c.json", nil)
	h.handler.ServeHTTP(httptest.NewRecorder(), req)
	page := h.do(http.MethodGet, authorizeURL("https://claude.ai/c.json", "https://claude.ai/cb", nil), nil)
	if page.Code != http.StatusOK {
		t.Errorf("after a cancelled request: %d", page.Code)
	}

	fail = true
	h.authorizeTo("https://claude.ai/d.json", nil)
	f, ok := h.s.clients.failed["https://claude.ai/d.json"]
	if !ok || time.Until(f.expires) > metadataRetryTime {
		t.Errorf("network failure remembered until %v", f.expires)
	}
}

// A document that was fetched before is refetched in the background, with fetch slots of its own, and
// still used for a while when its host can't be reached. A document that's gone isn't used any more.
func TestMetadataKnownClients(t *testing.T) {
	h := newHarness(t)
	const known = "https://claude.ai/c.json"
	var mu sync.Mutex
	var fail error
	hang := make(chan struct{})
	h.s.clients.fetch = func(_ context.Context, u string) ([]byte, error) {
		if u != known {
			<-hang
			return nil, errors.New("timeout")
		}
		mu.Lock()
		defer mu.Unlock()
		if fail != nil {
			return nil, fail
		}
		return json.Marshal(map[string]any{"client_id": u, "client_name": "Claude", "redirect_uris": []string{"https://claude.ai/cb"}})
	}
	failWith := func(err error) {
		mu.Lock()
		fail = err
		mu.Unlock()
		h.s.clients.mu.Lock()
		delete(h.s.clients.failed, known)
		h.s.clients.mu.Unlock()
	}
	expire := func(ago time.Duration) {
		h.s.clients.mu.Lock()
		c := h.s.clients.cache[known]
		c.expires = time.Now().Add(-ago)
		h.s.clients.cache[known] = c
		h.s.clients.mu.Unlock()
	}
	waitFor := func(what string, done func(cached bool, c cachedClient, failed bool) bool) {
		t.Helper()
		for start := time.Now(); time.Since(start) < 5*time.Second; time.Sleep(time.Millisecond) {
			h.s.clients.mu.Lock()
			c, cached := h.s.clients.cache[known]
			_, failed := h.s.clients.failed[known]
			h.s.clients.mu.Unlock()
			if done(cached, c, failed) {
				return
			}
		}
		t.Fatalf("waiting for %s", what)
	}
	signIn := func() int {
		return h.do(http.MethodGet, authorizeURL(known, "https://claude.ai/cb", nil), nil).Code
	}
	if code := signIn(); code != http.StatusOK {
		t.Fatalf("first sign-in: %d", code)
	}

	var wg sync.WaitGroup
	for i := range metadataFetches {
		wg.Go(func() { h.authorizeTo(fmt.Sprintf("https://claude.ai/made-up-%d.json", i), nil) })
	}
	defer func() { close(hang); wg.Wait() }()
	for len(h.s.clients.fetching) < metadataFetches {
		time.Sleep(time.Millisecond)
	}
	expire(time.Second)
	if code := signIn(); code != http.StatusOK {
		t.Errorf("sign-in with an expired document: %d", code)
	}
	waitFor("the refetch while the other slots are taken", func(_ bool, c cachedClient, _ bool) bool {
		return time.Now().Before(c.expires)
	})

	failWith(errors.New("connection reset"))
	expire(time.Second)
	if code := signIn(); code != http.StatusOK {
		t.Errorf("sign-in while the host is unreachable: %d", code)
	}
	waitFor("the failed refetch", func(_ bool, _ cachedClient, failed bool) bool { return failed })
	if code := signIn(); code != http.StatusOK {
		t.Errorf("sign-in with the failure remembered: %d", code)
	}
	expire(metadataStaleTime + time.Second)
	if code := signIn(); code == http.StatusOK {
		t.Error("sign-in with a document an hour past its time succeeded while the host is unreachable")
	}

	failWith(nil)
	expire(metadataStaleTime + time.Second)
	if code := signIn(); code != http.StatusOK {
		t.Errorf("sign-in once the host is back: %d", code)
	}
	failWith(fmt.Errorf("%w: status 404 Not Found", errNoDocument))
	expire(time.Second)
	signIn()
	waitFor("the missing document to be dropped", func(cached bool, _ cachedClient, _ bool) bool { return !cached })
	if code := signIn(); code == http.StatusOK {
		t.Error("sign-in with a document that's gone succeeded")
	}
	if f := h.s.clients.failed[known]; time.Until(f.expires) <= metadataRetryTime {
		t.Errorf("a missing document is remembered until %v", f.expires)
	}
}

// Only a 404 or 410 says a metadata document is gone; other refusals are blips.
func TestMetadataStatuses(t *testing.T) {
	for code, gone := range map[int]bool{404: true, 410: true, 403: false, 401: false, 302: false, 503: false} {
		err := statusError(&http.Response{StatusCode: code, Status: http.StatusText(code)})
		if err == nil || errors.Is(err, errNoDocument) != gone {
			t.Errorf("%d: %v", code, err)
		}
	}
	if err := statusError(&http.Response{StatusCode: http.StatusOK}); err != nil {
		t.Errorf("200: %v", err)
	}
}

// A request waiting on a shared fetch gives up when its client leaves.
func TestMetadataWaitEndsWithTheRequest(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	defer close(release)
	h.s.clients.fetch = func(context.Context, string) ([]byte, error) {
		<-release
		return nil, errors.New("timeout")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := h.s.clients.resolve(ctx, "https://claude.ai/slow.json")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Errorf("resolve: %v after %v", err, time.Since(start))
	}
}

// Requests for one document share a fetch, and wait briefly for a free slot instead of failing.
func TestMetadataFetchesAreShared(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	var fetches atomic.Int32
	h.s.clients.fetch = func(_ context.Context, u string) ([]byte, error) {
		fetches.Add(1)
		<-release
		return json.Marshal(map[string]any{"client_id": u, "client_name": "Claude", "redirect_uris": []string{"https://claude.ai/cb"}})
	}
	var wg sync.WaitGroup
	codes := make(chan int, 8)
	for range 8 {
		wg.Go(func() {
			codes <- h.do(http.MethodGet, authorizeURL("https://claude.ai/shared.json", "https://claude.ai/cb", nil), nil).Code
		})
	}
	for fetches.Load() == 0 {
		runtime.Gosched()
	}
	close(release)
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusOK {
			t.Errorf("a sign-in got %d", c)
		}
	}
	if n := fetches.Load(); n != 1 {
		t.Errorf("%d fetches for one document", n)
	}
}

// Scopes speki doesn't know are ignored on refresh too, so a client resending its original scope string
// keeps working.
func TestRefreshIgnoresUnknownScopes(t *testing.T) {
	h := newHarness(t)
	clientID, first := h.signedIn()
	resp, rec := h.tokens(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {first.RefreshToken}, "client_id": {clientID}, "scope": {"offline_access speki:read"}})
	if rec.Code != http.StatusOK || resp.Scope != ScopeRead {
		t.Errorf("refresh with an unknown scope: %d %s", rec.Code, rec.Body)
	}
}

func TestMetadataURLs(t *testing.T) {
	h := newHarness(t)
	fetches := 0
	h.s.clients.fetch = func(context.Context, string) ([]byte, error) {
		fetches++
		return []byte(`{"client_id":"someone else"}`), nil // a bad document: remembered for a while
	}
	if rec := h.authorizeTo("https://claude.ai/oauth/client.json?n=1", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("metadata URL with a query: %d", rec.Code)
	}
	for range 3 {
		h.authorizeTo("https://claude.ai/oauth/missing.json", nil)
	}
	if fetches != 1 {
		t.Errorf("a failing metadata URL was fetched %d times", fetches)
	}
}

// The real fetcher refuses to connect to addresses that aren't public, whatever the host name says.
func TestMetadataFetcherRefusesPrivateAddresses(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	fetch := newMetadataFetcher([]string{u.Hostname()})
	_, err := fetch(t.Context(), srv.URL+"/client.json")
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Errorf("fetch from loopback: %v", err)
	}
}

// flakyStore fails the operations named in fail, and passes the rest to a memory store.
type flakyStore struct {
	*store.Memory
	fail map[string]bool
}

func (f flakyStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if f.fail["get"] {
		return nil, false, errors.New("redis: connection refused")
	}
	return f.Memory.Get(ctx, key)
}

func (f flakyStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if f.fail["set"] {
		return errors.New("redis: connection refused")
	}
	return f.Memory.Set(ctx, key, value, ttl)
}

// With the store away, a refresh fails as temporary: the client keeps its refresh token.
func TestRefreshWithoutTheStore(t *testing.T) {
	h := newHarness(t)
	clientID, first := h.signedIn()
	mem := store.NewMemory()
	h.s.store = flakyStore{Memory: mem, fail: map[string]bool{"get": true}}
	_, rec := h.tokens(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {first.RefreshToken}, "client_id": {clientID}})
	if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "invalid_grant") {
		t.Errorf("store away: %d %s", rec.Code, rec.Body)
	}
}

// When the result of a refresh can't be kept, the lock stays until it runs out, so a second request
// with the same token waits instead of refreshing with a GitHub token that's spent.
func TestRefreshKeepsTheLockWhenTheResultIsLost(t *testing.T) {
	h := newHarness(t)
	clientID, first := h.signedIn()
	mem := store.NewMemory()
	h.s.store = flakyStore{Memory: mem, fail: map[string]bool{"set": true}}
	if _, rec := h.tokens(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {first.RefreshToken}, "client_id": {clientID}}); rec.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", rec.Code, rec.Body)
	}
	if _, held, _ := mem.Get(t.Context(), "refresh-lock:"+hash(first.RefreshToken)); !held {
		t.Error("the lock was released although the result wasn't kept")
	}
}

// A request with a refresh token whose result was lost is told to try again later, without asking GitHub
// with the spent token.
func TestRefreshResultLost(t *testing.T) {
	h := newHarness(t)
	clientID, first := h.signedIn()
	mem := store.NewMemory()
	h.s.store = &failingSets{Memory: mem, left: 2}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {first.RefreshToken}, "client_id": {clientID}}
	if _, rec := h.tokens(form); rec.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", rec.Code, rec.Body)
	}
	if _, held, _ := mem.Get(t.Context(), "refresh-lock:"+hash(first.RefreshToken)); held {
		t.Error("the lock was kept although the marker was written")
	}
	_, rec := h.tokens(form)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "result was lost") {
		t.Errorf("second refresh: %d %s", rec.Code, rec.Body)
	}
}

// A marker that lands between the first look and the lock gets the same answer.
func TestRefreshResultLostWhileLocking(t *testing.T) {
	h := newHarness(t)
	clientID, first := h.signedIn()
	h.s.store = markingStore{Memory: store.NewMemory()}
	_, rec := h.tokens(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {first.RefreshToken}, "client_id": {clientID}})
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "result was lost") {
		t.Errorf("refresh: %d %s", rec.Code, rec.Body)
	}
}

// markingStore writes a lost-result marker as a lock is taken.
type markingStore struct{ *store.Memory }

func (m markingStore) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	if rest, ok := strings.CutPrefix(key, "refresh-lock:"); ok {
		_ = m.Set(ctx, "refresh-result:"+rest, []byte(resultLost), time.Minute)
	}
	return m.Memory.SetNX(ctx, key, value, ttl)
}

// failingSets fails the first left Set calls.
type failingSets struct {
	*store.Memory
	left int
}

func (f *failingSets) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if f.left > 0 {
		f.left--
		return errors.New("redis: i/o timeout")
	}
	return f.Memory.Set(ctx, key, value, ttl)
}

// watchedStore says when a SetNX finds its key taken: someone is waiting on a lock.
type watchedStore struct {
	*store.Memory
	contended chan<- struct{}
}

func (w watchedStore) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	ok, err := w.Memory.SetNX(ctx, key, value, ttl)
	if err == nil && !ok {
		select {
		case w.contended <- struct{}{}:
		default:
		}
	}
	return ok, err
}
