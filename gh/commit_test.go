package gh

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// fakeGraphQL answers createCommitOnBranch, choosing its answer by expectedHeadOid.
func fakeGraphQL(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" || r.Header.Get("Authorization") != "Bearer ghu_good" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		var req struct {
			Variables struct {
				Input struct {
					Branch struct {
						Repo   string `json:"repositoryNameWithOwner"`
						Branch string `json:"branchName"`
					} `json:"branch"`
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
		fail := func(typ, msg string) {
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": []any{map[string]any{"type": typ, "message": msg}}})
		}
		switch in.Head {
		case "good":
			content, _ := base64.StdEncoding.DecodeString(in.FileChanges.Additions[0].Contents)
			if in.Branch.Repo != "ours/kb" || in.Branch.Branch != "main" || in.Message.Headline != "Update a.md" ||
				in.Message.Body != "Speki-Client: test" || string(content) != "new" ||
				len(in.FileChanges.Deletions) != 1 || in.FileChanges.Deletions[0].Path != "old.md" {
				t.Errorf("commit input = %+v", in)
			}
			_, _ = w.Write([]byte(`{"data":{"createCommitOnBranch":{"commit":{"oid":"c2"}}}}`))
		case "moved":
			fail("STALE_DATA", `Expected branch to point to "moved" but it did not. Pull and try again.`)
		case "protected":
			fail("UNPROCESSABLE", "Repository rule violations found\n\nChanges must be made through a pull request.")
		case "secret":
			fail("UNPROCESSABLE", "Push cannot contain secrets: GitHub Personal Access Token")
		case "forbidden":
			fail("FORBIDDEN", "Resource not accessible by integration")
		case "empty":
			if a := in.FileChanges.Additions; len(a) != 1 || a[0].Contents != "" {
				t.Errorf("empty file sent as %+v", a)
			}
			raw, _ := json.Marshal(req)
			if !strings.Contains(string(raw), `"Contents":""`) {
				t.Errorf("no contents for an empty file")
			}
			_, _ = w.Write([]byte(`{"data":{"createCommitOnBranch":{"commit":{"oid":"c3"}}}}`))
		case "stale-secrets":
			fail("STALE_DATA", `Expected branch to point to "x" while committing secrets.md`)
		case "limited":
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": []any{
				map[string]any{"type": "UNPROCESSABLE", "message": "something"},
				map[string]any{"type": "RATE_LIMITED", "message": "API rate limit exceeded"},
			}})
		case "abuse":
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit.","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#about-secondary-rate-limits"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{AppID: 1, PrivateKey: testKey(t), APIURL: srv.URL}, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCommit(t *testing.T) {
	c := fakeGraphQL(t)
	req := func(head string) CommitRequest {
		return CommitRequest{
			Repo: "ours/kb", Branch: "main", ExpectedHead: head, Message: "Update a.md\n\nSpeki-Client: test",
			Changes: []FileChange{{Path: "a.md", Content: []byte("new")}, {Path: "old.md"}},
		}
	}
	sha, err := c.Commit(t.Context(), "ghu_good", req("good"))
	if err != nil || sha != "c2" {
		t.Fatalf("Commit = %q, %v", sha, err)
	}

	for head, want := range map[string]error{
		"moved":     ErrHeadMoved,
		"protected": ErrBranchRules,
		"forbidden": ErrForbidden,
		"missing":   ErrForbidden,
		// a path named secrets.md in a stale-head message isn't push protection
		"stale-secrets": ErrHeadMoved,
	} {
		_, err := c.Commit(t.Context(), "ghu_good", req(head))
		if !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", head, err, want)
		}
	}
	_, err = c.Commit(t.Context(), "ghu_good", req("secret"))
	var serr *SecretError
	if !errors.As(err, &serr) || serr.Detail == "" {
		t.Errorf("secret: %v", err)
	}
	_, err = c.Commit(t.Context(), "ghu_good", req("abuse"))
	var rerr *RateLimitError
	if !errors.As(err, &rerr) || rerr.RetryAfter.Seconds() != 30 {
		t.Errorf("rate limit: %v", err)
	}
	_, err = c.Commit(t.Context(), "ghu_good", req("limited"))
	if !errors.As(err, &rerr) {
		t.Errorf("RATE_LIMITED among several errors: %v", err)
	}
	empty := req("empty")
	empty.Changes = []FileChange{{Path: "e.md", Content: []byte{}}}
	if _, err := c.Commit(t.Context(), "ghu_good", empty); err != nil {
		t.Errorf("empty file: %v", err)
	}
	_, err = c.Commit(t.Context(), "ghu_revoked", req("good"))
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("revoked token: %v", err)
	}
}
