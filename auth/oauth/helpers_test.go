package oauth_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/xaroth/speki/auth/oauth"
	"github.com/xaroth/speki/auth/seal"
	"github.com/xaroth/speki/gh"
	"github.com/xaroth/speki/store"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

const (
	issuer         = "https://speki.example.com"
	verifier       = "a-client-verifier-that-is-long-enough-for-pkce"
	loopback       = "http://127.0.0.1/callback"
	claudeClient   = "https://claude.ai/oauth/client.json"
	claudeCallback = "https://claude.ai/api/mcp/auth_callback"
	consentCookie  = "__Host-speki_consent"
	txCookie       = "__Host-speki_tx_"
)

// fakeGitHub stands in for GitHub's user OAuth endpoints. GitHub refresh tokens work once.
type fakeGitHub struct {
	mu          sync.Mutex
	issued      int
	spent       map[string]bool
	accessTTL   time.Duration
	callback    string // where GitHub must send the user back
	exchangeErr error
	meErr       error
	me          gh.User                   // who Me says the token's user is
	refreshErr  error                     // returned before GitHub spends the token
	afterSpend  func(ctx context.Context) // runs once GitHub spent the token, before it answers
}

func (f *fakeGitHub) set(change func(f *fakeGitHub)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func (f *fakeGitHub) tokensLocked() gh.UserTokens {
	f.issued++
	return gh.UserTokens{
		AccessToken:    fmt.Sprintf("ghu_%d", f.issued),
		AccessExpires:  time.Now().Add(f.accessTTL),
		RefreshToken:   fmt.Sprintf("ghr_%d", f.issued),
		RefreshExpires: time.Now().Add(180 * 24 * time.Hour),
	}
}

func (f *fakeGitHub) Exchange(_ context.Context, _ gh.OAuthConfig, code, codeVerifier, redirectURI string) (gh.UserTokens, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exchangeErr != nil {
		return gh.UserTokens{}, f.exchangeErr
	}
	if code != "good" || codeVerifier == "" || redirectURI != f.callback {
		return gh.UserTokens{}, gh.ErrBadGrant
	}
	return f.tokensLocked(), nil
}

func (f *fakeGitHub) Refresh(ctx context.Context, _ gh.OAuthConfig, refreshToken string) (gh.UserTokens, error) {
	f.mu.Lock()
	if f.refreshErr != nil {
		defer f.mu.Unlock()
		return gh.UserTokens{}, f.refreshErr
	}
	if f.spent[refreshToken] {
		f.mu.Unlock()
		return gh.UserTokens{}, gh.ErrBadGrant
	}
	f.spent[refreshToken] = true
	hook := f.afterSpend
	f.mu.Unlock()
	if hook != nil {
		hook(ctx)
	}
	if ctx.Err() != nil { // the answer never reaches speki
		return gh.UserTokens{}, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokensLocked(), nil
}

func (f *fakeGitHub) Me(context.Context, string) (gh.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.meErr != nil {
		return gh.User{}, f.meErr
	}
	return f.me, nil
}

// fakeStore is a memory store whose operations can be made to fail.
type fakeStore struct {
	*store.Memory
	mu   sync.Mutex
	hook func(op, key string) error
}

func (s *fakeStore) onCall(hook func(op, key string) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hook = hook
}

func (s *fakeStore) call(op, key string) error {
	s.mu.Lock()
	hook := s.hook
	s.mu.Unlock()
	if hook == nil {
		return nil
	}
	return hook(op, key)
}

func (s *fakeStore) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	if err := s.call("setnx", key); err != nil {
		return false, err
	}
	return s.Memory.SetNX(ctx, key, value, ttl)
}

func (s *fakeStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := s.call("set", key); err != nil {
		return err
	}
	return s.Memory.Set(ctx, key, value, ttl)
}

func (s *fakeStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if err := s.call("get", key); err != nil {
		return nil, false, err
	}
	return s.Memory.Get(ctx, key)
}

var errStoreDown = errors.New("redis: connection refused")

// fakeMetadataHost serves client metadata documents, as a transport, so fetches never touch the network.
type fakeMetadataHost struct {
	mu      sync.Mutex
	docs    map[string]http.HandlerFunc
	fetches map[string]int
}

func (h *fakeMetadataHost) serve(u string, f http.HandlerFunc) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.docs[u] = f
}

func (h *fakeMetadataHost) count(u string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fetches[u]
}

func (h *fakeMetadataHost) RoundTrip(r *http.Request) (*http.Response, error) {
	h.mu.Lock()
	f := h.docs[r.URL.String()]
	h.fetches[r.URL.String()]++
	h.mu.Unlock()
	if f == nil {
		f = http.NotFound
	}
	rec := httptest.NewRecorder()
	f(rec, r)
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	return rec.Result(), nil
}

// document serves a client metadata document for clientID.
func document(clientID string, redirectURIs ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"client_id": clientID, "client_name": "Claude", "redirect_uris": redirectURIs})
	}
}

func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

// logBuffer collects what the server logs.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

type env struct {
	server  *oauth.Server
	keyring *seal.Keyring
	issuer  string
	handler http.Handler
	github  *fakeGitHub
	store   *fakeStore
	docs    *fakeMetadataHost
	logs    *logBuffer
}

type setup struct {
	cfg       oauth.Config
	keys      *seal.Keyring
	transport bool // keep the real metadata transport
}

type option func(*setup)

func withIssuer(u string) option           { return func(s *setup) { s.cfg.Issuer = u } }
func withTitle(title string) option        { return func(s *setup) { s.cfg.Title = title } }
func withKeys(k *seal.Keyring) option      { return func(s *setup) { s.keys = k } }
func withMetadataHosts(h ...string) option { return func(s *setup) { s.cfg.ClientMetadataHosts = h } }
func withRealMetadataTransport() option    { return func(s *setup) { s.transport = true } }
func withCheckUser(f func(context.Context, string) error) option {
	return func(s *setup) { s.cfg.CheckUser = f }
}

func newKeys(tb testing.TB) *seal.Keyring {
	tb.Helper()
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	k, err := seal.SingleKey(base64.StdEncoding.EncodeToString(b))
	if err != nil {
		tb.Fatal(err)
	}
	return k
}

// newEnv returns an authorization server with GitHub, the store and client metadata hosts faked.
func newEnv(tb testing.TB, opts ...option) *env {
	tb.Helper()
	s := setup{cfg: oauth.Config{
		Issuer:              issuer,
		ClientMetadataHosts: []string{"claude.ai"},
		GitHub:              gh.OAuthConfig{ClientID: "Iv1", URL: "https://github.example"},
	}}
	for _, o := range opts {
		o(&s)
	}
	if s.keys == nil {
		s.keys = newKeys(tb)
	}
	e := &env{
		keyring: s.keys,
		issuer:  strings.TrimSuffix(s.cfg.Issuer, "/"),
		github: &fakeGitHub{
			spent:     map[string]bool{},
			me:        gh.User{ID: 7, Login: "octocat"},
			accessTTL: 8 * time.Hour,
			callback:  strings.TrimSuffix(s.cfg.Issuer, "/") + "/oauth/github/callback",
		},
		store: &fakeStore{Memory: store.NewMemory()},
		docs:  &fakeMetadataHost{docs: map[string]http.HandlerFunc{}, fetches: map[string]int{}},
		logs:  &logBuffer{},
	}
	e.docs.serve(claudeClient, document(claudeClient, claudeCallback, "http://127.0.0.1/callback"))
	e.server = oauth.New(s.cfg, s.keys, e.github, e.store, slog.New(slog.NewTextHandler(e.logs, nil)))
	if !s.transport {
		oauth.SetMetadataTransport(e.server, e.docs)
	}
	e.handler = e.server.Handler()
	return e
}

// do serves one request. A form is posted; without one the request is a GET.
func (e *env) do(tb testing.TB, target string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	tb.Helper()
	return e.doCtx(tb, tb.Context(), target, form, cookies...)
}

func (e *env) doCtx(tb testing.TB, ctx context.Context, target string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	tb.Helper()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if form != nil {
		req = httptest.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func (e *env) doJSON(tb testing.TB, target, body string) *httptest.ResponseRecorder {
	tb.Helper()
	req := httptest.NewRequestWithContext(tb.Context(), http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func (e *env) register(tb testing.TB, redirectURIs ...string) string {
	tb.Helper()
	body, _ := json.Marshal(map[string]any{"client_name": "Claude Code", "redirect_uris": redirectURIs})
	rec := e.doJSON(tb, "/oauth/register", string(body))
	if rec.Code != http.StatusCreated {
		tb.Fatalf("register: got %d %s, want 201", rec.Code, rec.Body)
	}
	var resp struct {
		ClientID string `json:"client_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return resp.ClientID
}

func challenge(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorizeURL builds a valid authorization request; extra replaces parameters, and an empty value drops one.
func authorizeURL(clientID, redirectURI string, extra url.Values) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"code_challenge":        {challenge(verifier)},
		"code_challenge_method": {"S256"},
		"state":                 {"client-state"},
		"resource":              {issuer + "/mcp"},
	}
	maps.Copy(q, extra)
	for k, v := range q {
		if len(v) == 1 && v[0] == "" {
			delete(q, k)
		}
	}
	return "/oauth/authorize?" + q.Encode()
}

// authorizeURL builds a valid authorization request to this server for a loopback client.
func (e *env) authorizeURL(clientID string) string {
	return authorizeURL(clientID, loopback, url.Values{"resource": {e.issuer + "/mcp"}})
}

// skew is how long past their expiry sealed values still open.
const skew = time.Minute

var consentField = regexp.MustCompile(`name="consent" value="([^"]+)"`)

func cookie(rec *httptest.ResponseRecorder, prefix string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if strings.HasPrefix(c.Name, prefix) && c.MaxAge >= 0 {
			return c
		}
	}
	return nil
}

// consentForm is the consent page's form with the user's choice.
func consentForm(tb testing.TB, page *httptest.ResponseRecorder, decision, access string) url.Values {
	tb.Helper()
	m := consentField.FindStringSubmatch(page.Body.String())
	if m == nil {
		tb.Fatalf("authorize: got %d with no consent field, want the consent page: %s", page.Code, page.Body)
	}
	return url.Values{"consent": {m[1]}, "decision": {decision}, "access": {access}}
}

// toGitHub runs authorize and consent, and returns the consent response and the state sent to GitHub.
func (e *env) toGitHub(tb testing.TB, authorize, access string) (*httptest.ResponseRecorder, string) {
	tb.Helper()
	page := e.do(tb, authorize, nil)
	rec := e.do(tb, "/oauth/authorize", consentForm(tb, page, "allow", access), cookie(page, consentCookie))
	if rec.Code != http.StatusSeeOther {
		tb.Fatalf("consent: got %d %s, want 303", rec.Code, rec.Body)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	return rec, loc.Query().Get("state")
}

// signIn runs the browser part of the flow and returns the client's code.
func (e *env) signIn(tb testing.TB, authorize, access string) string {
	tb.Helper()
	rec, state := e.toGitHub(tb, authorize, access)
	rec = e.do(tb, "/oauth/github/callback?code=good&state="+state, nil, cookie(rec, txCookie+state))
	if rec.Code != http.StatusFound {
		tb.Fatalf("callback: got %d %s, want 302", rec.Code, rec.Body)
	}
	back, _ := url.Parse(rec.Header().Get("Location"))
	return back.Query().Get("code")
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

type oauthError struct {
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

func (e *env) token(tb testing.TB, form url.Values) (tokenResponse, *httptest.ResponseRecorder) {
	tb.Helper()
	rec := e.do(tb, "/oauth/token", form)
	var resp tokenResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return resp, rec
}

func codeForm(clientID, code, redirectURI string) url.Values {
	f := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}, "code_verifier": {verifier}}
	if redirectURI != "" {
		f.Set("redirect_uri", redirectURI)
	}
	return f
}

func refreshForm(clientID, refreshToken string) url.Values {
	return url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {clientID}}
}

// signedIn registers a client, signs in with write access and returns the client ID and its first tokens.
func (e *env) signedIn(tb testing.TB) (string, tokenResponse) {
	tb.Helper()
	clientID := e.register(tb, loopback)
	code := e.signIn(tb, e.authorizeURL(clientID), "write")
	resp, rec := e.token(tb, codeForm(clientID, code, loopback))
	if rec.Code != http.StatusOK {
		tb.Fatalf("token: got %d %s, want 200", rec.Code, rec.Body)
	}
	return clientID, resp
}

// identity sends an MCP request with access and returns who the server saw, and the status.
func (e *env) identity(tb testing.TB, access string) (oauth.Identity, *httptest.ResponseRecorder) {
	tb.Helper()
	var got oauth.Identity
	h := e.server.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, _ = oauth.IdentityFrom(r.Context())
	}))
	req := httptest.NewRequestWithContext(tb.Context(), http.MethodPost, "/mcp", nil)
	if access != "" {
		req.Header.Set("Authorization", "Bearer "+access)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return got, rec
}

// storeKey is the store key speki uses for a refresh token's lock or result.
func storeKey(kind, refreshToken string) string {
	sum := sha256.Sum256([]byte(refreshToken))
	return kind + ":" + base64.RawURLEncoding.EncodeToString(sum[:])
}

// golden compares got with testdata/name, after replacing the sealed consent value, which differs each run.
func golden(tb testing.TB, name, got string) {
	tb.Helper()
	got = consentField.ReplaceAllString(got, `name="consent" value="SEALED"`)
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			tb.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		tb.Fatal(err)
	}
	if diff := cmp.Diff(string(want), got); diff != "" {
		tb.Errorf("%s mismatch (-want +got):\n%s", name, diff)
	}
}
