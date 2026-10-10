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

const (
	fakeRepo      = "ours/kb"
	fakeUserToken = "ghu_test"
)

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
	if strings.HasPrefix(r.URL.Path, "/user") || strings.HasPrefix(r.URL.Path, "/repos/"+fakeRepo+"/commits") {
		if r.Header.Get("Authorization") != "Bearer "+fakeUserToken {
			w.WriteHeader(http.StatusUnauthorized)
			write(map[string]any{"message": "Bad credentials"})
			return
		}
	}
	switch r.URL.Path {
	case "/login/oauth/access_token":
		_ = r.ParseForm()
		if r.Form.Get("code") != "github-code" || r.Form.Get("client_secret") != "secret" {
			write(map[string]any{"error": "bad_verification_code"})
			return
		}
		write(map[string]any{"access_token": fakeUserToken, "expires_in": 28800,
			"refresh_token": "ghr_test", "refresh_token_expires_in": 15897600})
	case "/user":
		write(map[string]any{"id": 7, "login": "octocat"})
	case "/user/installations":
		write(map[string]any{"total_count": 1, "installations": []any{map[string]any{"id": 1}}})
	case "/user/installations/1/repositories":
		write(map[string]any{"total_count": 1, "repositories": []any{
			map[string]any{"id": 100, "permissions": map[string]any{"pull": true}},
		}})
	case "/repos/" + fakeRepo + "/commits":
		write([]any{map[string]any{"sha": "c1", "author": map[string]any{"login": "octocat"},
			"commit": map[string]any{"message": "Add fleet notes\n\nSpeki-Client: Claude Code\n",
				"author": map[string]any{"name": "Octo", "date": "2026-10-09T14:02:11Z"}}}})
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
		PublicURL:     "https://speki.example.com",
		Listen:        "127.0.0.1:0",
		MetricsListen: "127.0.0.1:0",
		GitHub: GitHub{
			AppID:        1,
			PrivateKey:   testKey(t),
			ClientID:     "Iv1.test",
			ClientSecret: "secret",
			APIURL:       fake.api.URL,
			OAuthURL:     fake.api.URL,
			GitURL:       strings.TrimSuffix(fake.gitURL, "/"),
		},
		Auth: Auth{Keys: testKeys()},
		Sync: Sync{DataDir: t.TempDir()},
	}
}

func testKeys() Secret {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return Secret("keys:\n  - id: test\n    secret: " + base64.StdEncoding.EncodeToString(b) + "\n    primary: true\n")
}
