package gh

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

// fakeGitHub serves the handful of endpoints the client uses.
type fakeGitHub struct {
	t          *testing.T
	tokenCalls atomic.Int32
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	app := strings.HasPrefix(auth, "Bearer ") && strings.Count(auth, ".") == 2 // a JWT
	install := auth == "token ghs_inst1" || auth == "Bearer ghs_inst1"
	write := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		if install { // each credential has its own budget
			w.Header().Set("X-RateLimit-Remaining", "3000")
		} else {
			w.Header().Set("X-RateLimit-Remaining", "4999")
		}
		_ = json.NewEncoder(w).Encode(v)
	}

	switch {
	case r.URL.Path == "/app" && app:
		write(map[string]any{"id": 1, "owner": map[string]any{"id": 42, "login": "eve-online-tools"}})
	case r.URL.Path == "/app/installations" && app:
		if r.URL.Query().Get("page") == "2" {
			write([]any{map[string]any{"id": 2, "account": map[string]any{"id": 7, "login": "other"}, "suspended_at": "2026-01-01T00:00:00Z"}})
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<http://%s/app/installations?page=2>; rel="next"`, r.Host))
		write([]any{map[string]any{"id": 1, "account": map[string]any{"id": 42, "login": "eve-online-tools"}, "suspended_at": nil}})
	case r.URL.Path == "/app/installations/1/access_tokens" && app:
		f.tokenCalls.Add(1)
		w.WriteHeader(http.StatusCreated)
		write(map[string]any{"token": "ghs_inst1", "expires_at": time.Now().Add(time.Hour)})
	case r.URL.Path == "/app/installations/9/access_tokens" && app:
		w.WriteHeader(http.StatusNotFound)
		write(map[string]any{"message": "Not Found"})
	case r.URL.Path == "/app/installations/8/access_tokens" && app:
		w.WriteHeader(http.StatusForbidden)
		write(map[string]any{"message": "You have exceeded a secondary rate limit"})
	case r.URL.Path == "/repos/eve-online-tools/kb/contents/docs" && install:
		write([]any{map[string]any{"type": "file", "name": "a.md"}})
	case r.URL.Path == "/repos/eve-online-tools/kb/git/ref/heads/feature/x y" && install:
		w.Header().Set("ETag", `"v2"`)
		write(map[string]any{"ref": "refs/heads/feature/x y", "object": map[string]any{"sha": "def456", "type": "commit"}})
	case r.URL.Path == "/installation/repositories" && install:
		write(map[string]any{"total_count": 1, "repositories": []any{map[string]any{
			"id": 100, "full_name": "eve-online-tools/kb", "default_branch": "main",
			"pushed_at": "2026-10-09T10:00:00Z",
		}}})
	case r.URL.Path == "/repos/eve-online-tools/kb/contents/speki.yaml" && install:
		if r.URL.Query().Get("ref") != "main" {
			f.t.Errorf("contents ref = %q", r.URL.Query().Get("ref"))
		}
		write(map[string]any{"type": "file", "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte("title: KB\n"))})
	case r.URL.Path == "/repos/eve-online-tools/kb/git/ref/heads/main" && install:
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		write(map[string]any{"ref": "refs/heads/main", "object": map[string]any{"sha": "abc123", "type": "commit"}})
	case r.URL.Path == "/repos/eve-online-tools/big/contents/speki.yaml" && install:
		write(map[string]any{"type": "file", "encoding": "none", "size": 2_000_000, "content": ""})
	case r.URL.Path == "/repos/eve-online-tools/kb" && install:
		write(map[string]any{"id": 100, "full_name": "eve-online-tools/kb"})
	case strings.HasPrefix(r.URL.Path, "/repos/") && install:
		w.WriteHeader(http.StatusNotFound)
		write(map[string]any{"message": "Not Found"})
	default:
		f.t.Errorf("unexpected request %s %s (auth %q)", r.Method, r.URL.Path, auth)
		w.WriteHeader(http.StatusTeapot)
	}
}

func newTestClient(t *testing.T) (*Client, *fakeGitHub) {
	t.Helper()
	fake := &fakeGitHub{t: t}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	c, err := New(Config{AppID: 1, PrivateKey: testKey(t), APIURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, fake
}

func TestNewRejectsBadKey(t *testing.T) {
	_, err := New(Config{AppID: 1, PrivateKey: []byte("not a key")})
	if err == nil {
		t.Fatal("New accepted a bad key")
	}
}

func TestAppOwner(t *testing.T) {
	c, _ := newTestClient(t)
	id, err := c.AppOwner(t.Context())
	if err != nil {
		t.Fatalf("AppOwner: %v", err)
	}
	if id != 42 {
		t.Errorf("owner = %d, want 42", id)
	}
}

func TestInstallationsPaginates(t *testing.T) {
	c, _ := newTestClient(t)
	got, err := c.Installations(t.Context())
	if err != nil {
		t.Fatalf("Installations: %v", err)
	}
	want := []Installation{
		{ID: 1, AccountID: 42},
		{ID: 2, AccountID: 7, Suspended: true},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("installations (-want +got):\n%s", diff)
	}
}

func TestRepositoriesAndFile(t *testing.T) {
	c, fake := newTestClient(t)

	repos, err := c.Repositories(t.Context(), 1)
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	want := []Repository{{ID: 100, FullName: "eve-online-tools/kb", DefaultBranch: "main", PushedAt: time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)}}
	if diff := cmp.Diff(want, repos); diff != "" {
		t.Errorf("repositories (-want +got):\n%s", diff)
	}

	content, err := c.File(t.Context(), 1, "eve-online-tools/kb", "main", "speki.yaml")
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if string(content) != "title: KB\n" {
		t.Errorf("content = %q", content)
	}

	_, err = c.File(t.Context(), 1, "eve-online-tools/kb", "main", "missing.yaml")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("missing file: err = %v, want ErrNotFound", err)
	}

	_, err = c.File(t.Context(), 1, "eve-online-tools/kb", "main", "docs")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("folder: err = %v, want ErrNotFound", err)
	}

	if n := fake.tokenCalls.Load(); n != 1 {
		t.Errorf("installation token fetched %d times, want once", n)
	}
}

func TestGoneInstallation(t *testing.T) {
	c, _ := newTestClient(t)
	_, err := c.Repositories(t.Context(), 9)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	_, err = c.InstallationTokens(9).Token(t.Context())
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("token: err = %v, want ErrNotFound", err)
	}
}

func TestRefusedTokenIsNotGone(t *testing.T) {
	c, _ := newTestClient(t)
	_, err := c.Repositories(t.Context(), 8)
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("403 on token creation: err = %v, want a plain error", err)
	}
}

func TestBranchHead(t *testing.T) {
	c, _ := newTestClient(t)

	head, err := c.BranchHead(t.Context(), 1, "eve-online-tools/kb", "main", "")
	if err != nil {
		t.Fatalf("BranchHead: %v", err)
	}
	if head.SHA != "abc123" || head.ETag != `"v1"` {
		t.Errorf("head = %+v", head)
	}

	again, err := c.BranchHead(t.Context(), 1, "eve-online-tools/kb", "main", head.ETag)
	if err != nil {
		t.Fatalf("conditional BranchHead: %v", err)
	}
	if again.SHA != "" || again.ETag != head.ETag {
		t.Errorf("unchanged head = %+v, want empty SHA and the same ETag", again)
	}

	odd, err := c.BranchHead(t.Context(), 1, "eve-online-tools/kb", "feature/x y", "")
	if err != nil || odd.SHA != "def456" {
		t.Errorf("branch with slash and space: %+v, %v", odd, err)
	}

	_, err = c.BranchHead(t.Context(), 1, "eve-online-tools/gone", "main", "")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("missing repo: err = %v, want ErrNotFound", err)
	}
}

func TestRepositoryID(t *testing.T) {
	c, _ := newTestClient(t)
	id, err := c.RepositoryID(t.Context(), 1, "eve-online-tools/kb")
	if err != nil || id != 100 {
		t.Errorf("RepositoryID = %d, %v", id, err)
	}
	_, err = c.RepositoryID(t.Context(), 1, "eve-online-tools/gone")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("missing repo: %v", err)
	}
}

// An installation call counts once, as installation, and its token request as app; the app's budget
// isn't overwritten by the installation's.
func TestMetricsByKind(t *testing.T) {
	c, _ := newTestClient(t)
	tests := []struct {
		kind, status string
		want         float64
	}{{"app", "201", 1}, {"app", "200", 0}, {"installation", "200", 1}}
	before := make([]float64, len(tests)) // the counters are shared by the whole test binary
	for i, tt := range tests {
		before[i] = testutil.ToFloat64(githubRequests.WithLabelValues(tt.kind, tt.status))
	}
	githubRemaining.WithLabelValues("app").Set(4999)
	_, err := c.RepositoryID(t.Context(), 1, "eve-online-tools/kb")
	if err != nil {
		t.Fatal(err)
	}
	for i, tt := range tests {
		if got := testutil.ToFloat64(githubRequests.WithLabelValues(tt.kind, tt.status)) - before[i]; got != tt.want {
			t.Errorf("%s %s requests = %v, want %v", tt.kind, tt.status, got, tt.want)
		}
	}
	if got := testutil.ToFloat64(githubRemaining.WithLabelValues("app")); got == 3000 {
		t.Error("the app's budget shows the installation's")
	}
	if got := testutil.ToFloat64(githubRemaining.WithLabelValues("installation")); got != 3000 {
		t.Errorf("installation remaining = %v, want 3000", got)
	}
}

// The installation gauge shows the lowest recent budget, and a budget below 10% is warned about once.
func TestRateBudget(t *testing.T) {
	var logged bytes.Buffer
	m := newMetrics(slog.New(slog.NewTextHandler(&logged, nil)))
	header := func(remaining string) http.Header {
		return http.Header{"X-Ratelimit-Remaining": {remaining}, "X-Ratelimit-Limit": {"5000"}}
	}
	m.record("installation", 1, header("100"))
	m.record("installation", 2, header("4000"))
	if got := testutil.ToFloat64(githubRemaining.WithLabelValues("installation")); got != 100 {
		t.Errorf("installation remaining = %v, want the lowest, 100", got)
	}
	m.record("installation", 1, header("50"))
	if n := strings.Count(logged.String(), "budget is low"); n != 1 || !strings.Contains(logged.String(), "installation=1") {
		t.Errorf("%d warnings:\n%s", n, logged.String())
	}
	m.record("installation", 2, header("20"))
	if n := strings.Count(logged.String(), "budget is low"); n != 2 || !strings.Contains(logged.String(), "installation=2") {
		t.Errorf("each installation is warned about once; got %d warnings:\n%s", n, logged.String())
	}

	m.mu.Lock()
	m.installs[1] = reading{remaining: 50, at: time.Now().Add(-budgetWindow - time.Second)}
	m.installs[2] = reading{remaining: 20, at: time.Now(), reset: time.Now().Add(-time.Second)}
	m.mu.Unlock()
	m.record("installation", 3, header("3900"))
	if got := testutil.ToFloat64(githubRemaining.WithLabelValues("installation")); got != 3900 {
		t.Errorf("installation remaining = %v after the low readings aged out or were refilled, want 3900", got)
	}
}

// A failed installation token request counts as the app's, not as an installation call, and its
// response body is closed.
func TestTokenFailureCounts(t *testing.T) {
	c, _ := newTestClient(t)
	app403 := testutil.ToFloat64(githubRequests.WithLabelValues("app", "403"))
	installErrors := testutil.ToFloat64(githubRequests.WithLabelValues("installation", "error"))
	_, err := c.RepositoryID(t.Context(), 8, "eve-online-tools/kb")
	if err == nil {
		t.Fatal("RepositoryID succeeded without a token")
	}
	if got := testutil.ToFloat64(githubRequests.WithLabelValues("app", "403")) - app403; got != 1 {
		t.Errorf("app 403 requests = %v, want 1", got)
	}
	if got := testutil.ToFloat64(githubRequests.WithLabelValues("installation", "error")) - installErrors; got != 0 {
		t.Errorf("installation errors = %v, want 0", got)
	}
	body := &closeCounter{Reader: strings.NewReader("{}")}
	err = &ghinstallation.HTTPError{Response: &http.Response{StatusCode: http.StatusForbidden, Body: body}}
	closeTokenError(err)
	closeTokenError(fmt.Errorf("wrapped: %w", err))
	closeTokenError(&ghinstallation.HTTPError{Response: &http.Response{}}) // no body: no panic
	if body.closed == 0 {
		t.Error("the token error's body wasn't closed")
	}
	if signedByApp(&http.Request{Header: http.Header{"Authorization": {"Bearer ghu_user"}}}) {
		t.Error("a user token counted as the app's JWT")
	}
}

type closeCounter struct {
	io.Reader
	closed int
}

func (c *closeCounter) Close() error {
	c.closed++
	return nil
}

func TestFileTooLarge(t *testing.T) {
	c, _ := newTestClient(t)
	_, err := c.File(t.Context(), 1, "eve-online-tools/big", "main", "speki.yaml")
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("a 2 MB file: %v, want ErrTooLarge", err)
	}
}
