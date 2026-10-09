package service

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// fakeGitHub serves the GitHub API for one installation with one repository, which lives in a real git
// repository on disk under gitURL.
type fakeGitHub struct {
	t      *testing.T
	api    *httptest.Server
	gitURL string
	repo   *git.Repository
	dir    string
}

const fakeRepo = "ours/kb"

func newFakeGitHub(t *testing.T, files map[string]string) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{t: t, gitURL: t.TempDir()}
	f.dir = filepath.Join(f.gitURL, fakeRepo+".git")
	var err error
	f.repo, err = git.PlainInit(f.dir, false)
	if err != nil {
		t.Fatal(err)
	}
	f.push(files)
	f.api = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.api.Close)
	return f
}

func (f *fakeGitHub) push(files map[string]string) {
	f.t.Helper()
	wt, err := f.repo.Worktree()
	if err != nil {
		f.t.Fatal(err)
	}
	for name, content := range files {
		full := filepath.Join(f.dir, name)
		err = os.MkdirAll(filepath.Dir(full), 0o755)
		if err == nil {
			err = os.WriteFile(full, []byte(content), 0o644)
		}
		if err == nil {
			_, err = wt.Add(name)
		}
		if err != nil {
			f.t.Fatal(err)
		}
	}
	_, err = wt.Commit("push", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@x", When: time.Now()}})
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	write := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch r.URL.Path {
	case "/app":
		write(map[string]any{"id": 1, "owner": map[string]any{"id": 42}})
	case "/app/installations":
		write([]any{map[string]any{"id": 1, "account": map[string]any{"id": 42}}})
	case "/app/installations/1/access_tokens":
		w.WriteHeader(http.StatusCreated)
		write(map[string]any{"token": "ghs_test", "expires_at": time.Now().Add(time.Hour)})
	case "/installation/repositories":
		write(map[string]any{"total_count": 1, "repositories": []any{map[string]any{
			"id": 100, "full_name": fakeRepo, "default_branch": "master", "pushed_at": "2026-10-09T10:00:00Z",
		}}})
	case "/repos/" + fakeRepo + "/contents/speki.yaml":
		content, err := os.ReadFile(filepath.Join(f.dir, "speki.yaml"))
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			write(map[string]any{"message": "Not Found"})
			return
		}
		write(map[string]any{"type": "file", "encoding": "base64", "content": base64.StdEncoding.EncodeToString(content)})
	case "/repos/" + fakeRepo + "/git/ref/heads/master":
		head, err := f.repo.Head()
		if err != nil {
			f.t.Error(err)
			return
		}
		write(map[string]any{"object": map[string]any{"sha": head.Hash().String()}})
	default:
		f.t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func testKey(t *testing.T) Secret {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return Secret(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

// testConfig returns a configuration that talks to fake and keeps its data in a temporary directory.
func testConfig(t *testing.T, fake *fakeGitHub) Config {
	t.Helper()
	return Config{
		Listen:        "127.0.0.1:0",
		MetricsListen: "127.0.0.1:0",
		GitHub: GitHub{
			AppID:      1,
			PrivateKey: testKey(t),
			APIURL:     fake.api.URL,
			GitURL:     strings.TrimSuffix(fake.gitURL, "/"),
		},
		Sync: Sync{DataDir: t.TempDir()},
	}
}
