package gh

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus"
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
	write := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		_ = json.NewEncoder(w).Encode(v)
	}
	app := strings.HasPrefix(auth, "Bearer ") && strings.Count(auth, ".") == 2 // a JWT
	install := auth == "token ghs_inst1" || auth == "Bearer ghs_inst1"

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

func newTestClient(t *testing.T) (*Client, *fakeGitHub, *prometheus.Registry) {
	t.Helper()
	fake := &fakeGitHub{t: t}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	reg := prometheus.NewRegistry()
	c, err := New(Config{AppID: 1, PrivateKey: testKey(t), APIURL: srv.URL}, reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, fake, reg
}

func TestNewRejectsBadKey(t *testing.T) {
	_, err := New(Config{AppID: 1, PrivateKey: []byte("not a key")}, prometheus.NewRegistry())
	if err == nil {
		t.Fatal("New accepted a bad key")
	}
}

func TestAppOwner(t *testing.T) {
	c, _, _ := newTestClient(t)
	id, err := c.AppOwner(t.Context())
	if err != nil {
		t.Fatalf("AppOwner: %v", err)
	}
	if id != 42 {
		t.Errorf("owner = %d, want 42", id)
	}
}

func TestInstallationsPaginates(t *testing.T) {
	c, _, _ := newTestClient(t)
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
	c, fake, _ := newTestClient(t)

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
	c, _, _ := newTestClient(t)
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
	c, _, _ := newTestClient(t)
	_, err := c.Repositories(t.Context(), 8)
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("403 on token creation: err = %v, want a plain error", err)
	}
}

func TestBranchHead(t *testing.T) {
	c, _, _ := newTestClient(t)

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
	c, _, _ := newTestClient(t)
	id, err := c.RepositoryID(t.Context(), 1, "eve-online-tools/kb")
	if err != nil || id != 100 {
		t.Errorf("RepositoryID = %d, %v", id, err)
	}
	_, err = c.RepositoryID(t.Context(), 1, "eve-online-tools/gone")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("missing repo: %v", err)
	}
}

func TestMetrics(t *testing.T) {
	c, _, reg := newTestClient(t)
	_, err := c.AppOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	m := c.metrics
	if got := testutil.ToFloat64(m.requests.WithLabelValues("app", "200")); got != 1 {
		t.Errorf("app requests = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.remaining.WithLabelValues("app")); got != 4999 {
		t.Errorf("remaining = %v, want 4999", got)
	}
	n, err := testutil.GatherAndCount(reg, "speki_github_requests_total")
	if err != nil || n == 0 {
		t.Errorf("metric not registered: %d, %v", n, err)
	}
}

func TestFileTooLarge(t *testing.T) {
	c, _, _ := newTestClient(t)
	_, err := c.File(t.Context(), 1, "eve-online-tools/big", "main", "speki.yaml")
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("a 2 MB file: %v, want ErrTooLarge", err)
	}
}
