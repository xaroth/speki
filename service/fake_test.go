package service

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

	block chan struct{} // when set, the installation list waits for it to close

	mu        sync.Mutex      // commits, and refresh tokens, arrive from several instances at once
	refreshed map[string]bool // GitHub refresh tokens work once
	refreshes int
}

const (
	fakeRepo      = "ours/kb"
	fakeUserToken = "ghu_test"
)

func newFakeGitHub(t *testing.T, files map[string]string) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{t: t, gitURL: t.TempDir(), refreshed: map[string]bool{}}
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
	if strings.HasPrefix(r.URL.Path, "/user") || strings.HasPrefix(r.URL.Path, "/repos/"+fakeRepo+"/commits") || r.URL.Path == "/graphql" {
		if r.Header.Get("Authorization") != "Bearer "+fakeUserToken {
			w.WriteHeader(http.StatusUnauthorized)
			write(map[string]any{"message": "Bad credentials"})
			return
		}
	}
	switch r.URL.Path {
	case "/login/oauth/access_token":
		_ = r.ParseForm()
		if refresh := r.Form.Get("refresh_token"); refresh != "" {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.refreshed[refresh] {
				write(map[string]any{"error": "bad_refresh_token"})
				return
			}
			f.refreshed[refresh] = true
			f.refreshes++
			write(map[string]any{"access_token": fakeUserToken, "expires_in": 28800,
				"refresh_token": fmt.Sprintf("ghr_test_%d", f.refreshes), "refresh_token_expires_in": 15897600})
			return
		}
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
			map[string]any{"id": 100, "permissions": map[string]any{"pull": true, "push": true}},
		}})
	case "/repos/" + fakeRepo + "/commits":
		write([]any{map[string]any{"sha": "c1", "author": map[string]any{"login": "octocat"},
			"commit": map[string]any{"message": "Add fleet notes\n\nSpeki-Client: Claude Code\n",
				"author": map[string]any{"name": "Octo", "date": "2026-10-09T14:02:11Z"}}}})
	case "/graphql":
		f.commit(w, r)
	case "/app":
		write(map[string]any{"id": 1, "owner": map[string]any{"id": 42}})
	case "/app/installations":
		if f.block != nil {
			select {
			case <-f.block:
			case <-r.Context().Done():
			}
		}
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

// commit answers GraphQL createCommitOnBranch by committing to the repository on disk.
func (f *fakeGitHub) commit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Variables struct {
			Input struct {
				Head    string `json:"expectedHeadOid"`
				Message struct {
					Headline string `json:"headline"`
					Body     string `json:"body"`
				} `json:"message"`
				FileChanges struct {
					Additions []struct{ Path, Contents string } `json:"additions"`
					Deletions []struct{ Path string }           `json:"deletions"`
				} `json:"fileChanges"`
			} `json:"input"`
		} `json:"variables"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	in := req.Variables.Input
	w.Header().Set("Content-Type", "application/json")
	f.mu.Lock()
	defer f.mu.Unlock()
	head, err := f.repo.Head()
	if err != nil {
		f.t.Error(err)
		return
	}
	if in.Head != head.Hash().String() {
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": []any{map[string]any{"type": "STALE_DATA", "message": "Expected branch to point to " + in.Head}}})
		return
	}
	wt, err := f.repo.Worktree()
	if err != nil {
		f.t.Error(err)
		return
	}
	for _, a := range in.FileChanges.Additions {
		content, _ := base64.StdEncoding.DecodeString(a.Contents)
		full := filepath.Join(f.dir, a.Path)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		_ = os.WriteFile(full, content, 0o644)
		_, _ = wt.Add(a.Path)
	}
	for _, d := range in.FileChanges.Deletions {
		_, _ = wt.Remove(d.Path)
	}
	sha, err := wt.Commit(in.Message.Headline+"\n\n"+in.Message.Body, &git.CommitOptions{Author: &object.Signature{Name: "octocat", Email: "o@x", When: time.Now()}})
	if err != nil {
		f.t.Error(err)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"createCommitOnBranch": map[string]any{"commit": map[string]any{"oid": sha.String()}}}})
}
