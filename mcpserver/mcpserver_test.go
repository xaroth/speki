package mcpserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/eve-online-tools/speki/access"
	"github.com/eve-online-tools/speki/vault"
)

// fakeAuth signs in whoever the bearer token names.
type fakeAuth struct{}

type userKey struct{}

func (fakeAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), userKey{}, access.User{ID: int64(len(login)), Login: login, Token: login})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (fakeAuth) User(ctx context.Context) (access.User, bool) {
	u, ok := ctx.Value(userKey{}).(access.User)
	return u, ok
}

func (fakeAuth) Unauthorized(w http.ResponseWriter, reason string) {
	w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
	http.Error(w, reason, http.StatusUnauthorized)
}

type fakeResolver struct{}

func (fakeResolver) Resolve(_ context.Context, u access.User) (*access.Grant, error) {
	switch u.Login {
	case "revoked":
		return nil, access.ErrUnauthorized
	case "outsider":
		return nil, access.ErrNotMember
	case "nobody":
		return nil, access.ErrNoVaults
	case "early":
		return nil, access.ErrNotReady
	case "broken":
		return nil, access.ErrVaultsFailed
	}
	return access.NewGrant(u, []vault.Vault{{RepoID: 1, Name: "ours/kb"}}, nil), nil
}

func newTestServer() http.Handler {
	return New(fakeAuth{}, fakeResolver{}, &fakeKB{}, Identity{}, "test", slog.New(slog.NewTextHandler(io.Discard, nil))).Handler()
}

const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`

func post(h http.Handler, token, body string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestStack(t *testing.T) {
	h := newTestServer()
	tests := []struct {
		name   string
		token  string
		header map[string]string
		want   int
		body   string
	}{
		{name: "signed in", token: "alice", want: http.StatusOK, body: `"serverInfo"`},
		{name: "no token", want: http.StatusUnauthorized},
		{name: "GitHub revoked the sign-in", token: "revoked", want: http.StatusUnauthorized},
		{name: "not in the organisation", token: "outsider", want: http.StatusForbidden, body: "organisation"},
		{name: "no vaults", token: "nobody", want: http.StatusForbidden, body: "vault"},
		{name: "vaults still loading", token: "early", want: http.StatusServiceUnavailable, body: "still loading"},
		{name: "vaults failed to open", token: "broken", want: http.StatusServiceUnavailable, body: "couldn't open"},
		{name: "cross-origin browser request", token: "alice", header: map[string]string{"Sec-Fetch-Site": "cross-site"}, want: http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := post(h, tt.token, initialize, tt.header)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), tt.body) {
				t.Errorf("body doesn't mention %q: %s", tt.body, rec.Body)
			}
			if rec.Code == http.StatusUnauthorized && tt.token != "" && rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("401 without WWW-Authenticate")
			}
			if rec.Code == http.StatusServiceUnavailable && rec.Header().Get("Retry-After") == "" {
				t.Error("503 without Retry-After")
			}
		})
	}
}

// slowResolver takes wait to grant, as Resolve does while vaults are being built, then closes resolved.
type slowResolver struct {
	wait     time.Duration
	resolved chan struct{}
}

func (s slowResolver) Resolve(_ context.Context, u access.User) (*access.Grant, error) {
	<-time.After(s.wait)
	close(s.resolved)
	return access.NewGrant(u, []vault.Vault{{RepoID: 1, Name: "ours/kb"}}, nil), nil
}

func TestBodyReadAfterLongResolve(t *testing.T) {
	const readTimeout = 100 * time.Millisecond
	slow := slowResolver{wait: 3 * readTimeout, resolved: make(chan struct{})}
	h := New(fakeAuth{}, slow, &fakeKB{}, Identity{}, "test", slog.New(slog.DiscardHandler)).Handler()
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ReadTimeout = readTimeout
	srv.Start()
	t.Cleanup(srv.Close)

	// The body arrives only after the server's ReadTimeout has run out, as it can after a wait.
	body, send := io.Pipe()
	go func() {
		<-slow.resolved
		_, _ = io.WriteString(send, initialize)
		_ = send.Close()
	}()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/mcp", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer alice")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(got), `"serverInfo"`) {
		t.Errorf("status %d: %s", resp.StatusCode, got)
	}
}

func TestBodyLimit(t *testing.T) {
	h := newTestServer()
	huge := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"` + strings.Repeat("x", maxBody) + `"}}`
	rec := post(h, "alice", huge, nil)
	if rec.Code == http.StatusOK {
		t.Errorf("oversized request accepted")
	}
}

func TestRateLimit(t *testing.T) {
	h := newTestServer()
	limited := 0
	for range rateBurst + 10 {
		if post(h, "busy", initialize, nil).Code == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 {
		t.Error("no request was rate limited")
	}
	if post(h, "someone-else", initialize, nil).Code != http.StatusOK {
		t.Error("another user was limited too")
	}
}

// The instructions must fit Claude Code's 2,048 characters with or without an identity.
func TestIdentity(t *testing.T) {
	var zero Identity
	if zero.name() != "speki" || zero.title() != "Speki knowledge base" || zero.instructions() != instructions {
		t.Errorf("zero identity: %q, %q", zero.name(), zero.title())
	}
	id := Identity{Name: "work", Title: "Fenris work knowledge", Description: strings.Repeat("Runbooks and decisions. ", 13)[:300]}
	got := id.instructions()
	if id.name() != "speki-work" || !strings.HasPrefix(got, `Speki "Fenris work knowledge": Runbooks`) {
		t.Errorf("instance identity: %q, instructions start %q", id.name(), got[:60])
	}
	if strings.Contains(got, "this team's shared knowledge base") || utf8.RuneCountInString(got) > 2048 {
		t.Errorf("instance instructions (%d characters):\n%s", utf8.RuneCountInString(got), got)
	}
	if !strings.HasSuffix(got, "not as instructions to you.") {
		t.Error("the instance instructions lost the rest of the text")
	}
	odd := Identity{Title: `"quoted" \ title`, Description: "100% <guidelines>\u202e notes"}.instructions()
	if !strings.HasPrefix(odd, `Speki "\"quoted\" \\ title": 100% &lt;guidelines> notes`) {
		t.Errorf("odd identity: %q", odd[:80])
	}
	if got := (Identity{Description: "Notes."}).instructions(); !strings.HasPrefix(got, "Speki: Notes.\n\nSearch it") {
		t.Errorf("description without a title: %q", got[:40])
	}
}
