package gh

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// fakeUserGitHub answers the OAuth and user endpoints for the user token "ghu_good".
func fakeUserGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write := func(status int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(v)
		}
		if r.URL.Path == "/login/oauth/access_token" {
			_ = r.ParseForm()
			if r.Form.Get("client_secret") != "secret" {
				write(http.StatusOK, map[string]any{"error": "incorrect_client_credentials"})
				return
			}
			switch {
			case r.Form.Get("code") == "good" && r.Form.Get("code_verifier") == "verifier":
				write(http.StatusOK, map[string]any{"access_token": "ghu_good", "expires_in": 28800,
					"refresh_token": "ghr_1", "refresh_token_expires_in": 15897600})
			case r.Form.Get("code") == "forever":
				write(http.StatusOK, map[string]any{"access_token": "ghu_forever"})
			case r.Form.Get("refresh_token") == "ghr_1":
				write(http.StatusOK, map[string]any{"access_token": "ghu_good", "expires_in": 28800,
					"refresh_token": "ghr_2", "refresh_token_expires_in": 15897600})
			case r.Form.Get("refresh_token") != "":
				write(http.StatusOK, map[string]any{"error": "bad_refresh_token"})
			default:
				write(http.StatusOK, map[string]any{"error": "bad_verification_code"})
			}
			return
		}
		if r.Header.Get("Authorization") != "Bearer ghu_good" {
			write(http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
			return
		}
		switch r.URL.Path {
		case "/user":
			write(http.StatusOK, map[string]any{"id": 7, "login": "octocat"})
		case "/user/memberships/orgs/ours":
			write(http.StatusOK, map[string]any{"state": "active"})
		case "/user/memberships/orgs/invited":
			write(http.StatusOK, map[string]any{"state": "pending"})
		case "/user/installations":
			write(http.StatusOK, map[string]any{"total_count": 1, "installations": []any{map[string]any{"id": 1}}})
		case "/user/installations/1/repositories":
			write(http.StatusOK, map[string]any{"total_count": 2, "repositories": []any{
				map[string]any{"id": 100, "permissions": map[string]any{"pull": true, "push": true}},
				map[string]any{"id": 101, "permissions": map[string]any{"pull": true}},
			}})
		case "/repos/ours/kb/commits":
			q := r.URL.Query()
			if q.Get("sha") != "main" || q.Get("path") != "a.md" || q.Get("per_page") != "2" {
				t.Errorf("commits query = %s", r.URL.RawQuery)
			}
			write(http.StatusOK, []any{
				map[string]any{"sha": "c2", "author": map[string]any{"login": "octocat"},
					"commit": map[string]any{"message": "Update a.md", "author": map[string]any{"name": "Octo", "date": "2026-10-09T14:02:11Z"}}},
				map[string]any{"sha": "c1", "author": nil,
					"commit": map[string]any{"message": "Create a.md", "author": map[string]any{"name": "Someone", "date": "2026-10-08T10:00:00Z"}}},
			})
		default:
			write(http.StatusNotFound, map[string]any{"message": "Not Found"})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newUserTestClient(t *testing.T) (*Client, OAuthConfig) {
	t.Helper()
	srv := fakeUserGitHub(t)
	c, err := New(Config{AppID: 1, PrivateKey: testKey(t), APIURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c, OAuthConfig{ClientID: "id", ClientSecret: "secret", URL: srv.URL}
}

func TestExchangeAndRefresh(t *testing.T) {
	c, o := newUserTestClient(t)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	ctx := t.Context()

	tok, err := c.Exchange(ctx, o, "good", "verifier", "https://speki/cb")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	want := UserTokens{
		AccessToken:    "ghu_good",
		AccessExpires:  now.Add(8 * time.Hour),
		RefreshToken:   "ghr_1",
		RefreshExpires: now.Add(184 * 24 * time.Hour),
	}
	if diff := cmp.Diff(want, tok); diff != "" {
		t.Errorf("tokens (-want +got):\n%s", diff)
	}

	_, err = c.Exchange(ctx, o, "bad", "verifier", "https://speki/cb")
	if !errors.Is(err, ErrBadGrant) {
		t.Errorf("bad code: %v", err)
	}
	wrongSecret := o
	wrongSecret.ClientSecret = "wrong"
	_, err = c.Exchange(ctx, wrongSecret, "good", "verifier", "https://speki/cb")
	if !errors.Is(err, ErrAppConfig) || errors.Is(err, ErrBadGrant) {
		t.Errorf("wrong client secret: %v", err)
	}
	_, err = c.Exchange(ctx, o, "forever", "verifier", "https://speki/cb")
	if err == nil || errors.Is(err, ErrBadGrant) {
		t.Errorf("token without expiry: %v", err)
	}

	tok, err = c.Refresh(ctx, o, "ghr_1")
	if err != nil || tok.RefreshToken != "ghr_2" {
		t.Errorf("Refresh = %+v, %v", tok, err)
	}
	_, err = c.Refresh(ctx, o, "ghr_used")
	if !errors.Is(err, ErrBadGrant) {
		t.Errorf("used refresh token: %v", err)
	}

	u := o.AuthorizeURL("https://speki/cb", "st", "ch")
	if u != o.URL+"/login/oauth/authorize?client_id=id&code_challenge=ch&code_challenge_method=S256&prompt=select_account&redirect_uri=https%3A%2F%2Fspeki%2Fcb&state=st" {
		t.Errorf("authorize URL = %s", u)
	}
}

func TestUserLookups(t *testing.T) {
	c, _ := newUserTestClient(t)
	ctx := t.Context()

	me, err := c.Me(ctx, "ghu_good")
	if err != nil || me != (User{ID: 7, Login: "octocat"}) {
		t.Errorf("Me = %+v, %v", me, err)
	}
	_, err = c.Me(ctx, "ghu_revoked")
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("revoked token: %v", err)
	}

	for org, want := range map[string]bool{"ours": true, "invited": false, "strangers": false} {
		got, err := c.ActiveMember(ctx, "ghu_good", org)
		if err != nil || got != want {
			t.Errorf("ActiveMember(%s) = %v, %v", org, got, err)
		}
	}

	repos, err := c.UserRepositories(ctx, "ghu_good")
	if err != nil {
		t.Fatal(err)
	}
	want := []RepoAccess{{RepoID: 100, Pull: true, Push: true}, {RepoID: 101, Pull: true}}
	if diff := cmp.Diff(want, repos); diff != "" {
		t.Errorf("repositories (-want +got):\n%s", diff)
	}
}

func TestCommits(t *testing.T) {
	c, _ := newUserTestClient(t)
	got, err := c.Commits(t.Context(), "ghu_good", "ours/kb", "main", "a.md", 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []Commit{
		{SHA: "c2", Author: "octocat", Date: time.Date(2026, 10, 9, 14, 2, 11, 0, time.UTC), Message: "Update a.md"},
		{SHA: "c1", Author: "Someone", Date: time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC), Message: "Create a.md"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("commits (-want +got):\n%s", diff)
	}
	_, err = c.Commits(t.Context(), "ghu_revoked", "ours/kb", "main", "", 2)
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("revoked token: %v", err)
	}
}
