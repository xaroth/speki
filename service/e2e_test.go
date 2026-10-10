package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestSignInAndSearch runs speki against a fake GitHub: a client registers, the user signs in through
// the browser flow, and an MCP client uses the token to find and read a document.
func TestSignInAndSearch(t *testing.T) {
	fake := newFakeGitHub(t, map[string]string{
		"speki.yaml":   "title: KB\ndescription: Test vault.\nguidelines: One page per topic.\n",
		"fleet.md":     "---\ntitle: Fleet doctrine\ntags: [fleet]\n---\n# Fleet doctrine\n\nWe fly zeppelins.\n",
		"ops/roles.md": "# Roles\n\nWho does what.\n",
	})
	s, err := New(testConfig(t, fake), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	<-s.discovered
	s.library.WaitIdle()

	base := "http://" + s.publicAddr.String()
	token := signIn(t, base)

	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "1"}, nil)
	cs, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:             base + "/mcp",
		HTTPClient:           &http.Client{Transport: bearer(token)},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()

	text := callTool(t, cs, "guide", nil)
	if !strings.Contains(text, "### ours/kb") || !strings.Contains(text, "One page per topic.") || !strings.Contains(text, "read only") {
		t.Errorf("guide:\n%s", text)
	}

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "search", Arguments: map[string]any{"query": "zeppelin"}})
	if err != nil || res.IsError {
		t.Fatalf("search: %v %+v", err, res)
	}
	var found struct {
		Results []struct {
			Vault, Path, Version string
		}
	}
	raw, _ := json.Marshal(res.StructuredContent)
	_ = json.Unmarshal(raw, &found)
	if len(found.Results) != 1 || found.Results[0].Vault != fakeRepo || found.Results[0].Path != "fleet.md" {
		t.Fatalf("search results: %s", raw)
	}

	text = callTool(t, cs, "read", map[string]any{"vault": fakeRepo, "path": "fleet.md"})
	if !strings.Contains(text, "version "+found.Results[0].Version) || !strings.Contains(text, "<document>\n---\ntitle: Fleet doctrine\n") {
		t.Errorf("read:\n%s", text)
	}

	text = callTool(t, cs, "list", map[string]any{"vault": fakeRepo})
	if !strings.Contains(text, "ops/\n") || !strings.Contains(text, "fleet.md  Fleet doctrine") {
		t.Errorf("list:\n%s", text)
	}

	text = callTool(t, cs, "history", map[string]any{"vault": fakeRepo, "path": "fleet.md"})
	if !strings.Contains(text, "octocat via Claude Code: Add fleet notes") {
		t.Errorf("history:\n%s", text)
	}

	res, err = cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "read", Arguments: map[string]any{"vault": "ours/other", "path": "fleet.md"}})
	if err != nil || !res.IsError {
		t.Errorf("read from an unknown vault: %v %+v", err, res)
	}
}

type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args any) string {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if res.IsError {
		t.Fatalf("%s: %s", name, text)
	}
	return text
}

var consentField = regexp.MustCompile(`name="consent" value="([^"]+)"`)

// signIn registers a client and goes through the browser flow as a read-only user, returning the
// access token.
func signIn(t *testing.T, base string) string {
	t.Helper()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	type response struct {
		header  http.Header
		cookies []*http.Cookie
		body    []byte
	}
	do := func(method, target string, form url.Values, cookies []*http.Cookie, want int) response {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), method, target, strings.NewReader(form.Encode()))
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		for _, c := range cookies {
			req.AddCookie(c) // __Host- cookies are Secure, so a cookie jar wouldn't send them over plain HTTP
		}
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("%s %s = %d, want %d: %s", method, target, resp.StatusCode, want, body)
		}
		return response{header: resp.Header, cookies: resp.Cookies(), body: body}
	}

	const redirect = "http://127.0.0.1:5555/callback"
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/oauth/register",
		strings.NewReader(`{"client_name":"e2e","redirect_uris":["`+redirect+`"]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		ClientID string `json:"client_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&reg)
	_ = resp.Body.Close()

	verifier := "a-client-verifier-that-is-long-enough-for-pkce"
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {reg.ClientID},
		"redirect_uri":          {redirect},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"state":                 {"st"},
		"resource":              {"https://speki.example.com/mcp"},
	}
	page := do(http.MethodGet, base+"/oauth/authorize?"+q.Encode(), nil, nil, http.StatusOK)
	m := consentField.FindSubmatch(page.body)
	if m == nil {
		t.Fatal("no consent field")
	}

	toGitHub := do(http.MethodPost, base+"/oauth/authorize",
		url.Values{"consent": {string(m[1])}, "access": {"read"}, "decision": {"allow"}}, page.cookies, http.StatusSeeOther)
	loc, _ := url.Parse(toGitHub.header.Get("Location"))
	back := do(http.MethodGet, base+"/oauth/github/callback?code=github-code&state="+loc.Query().Get("state"),
		nil, toGitHub.cookies, http.StatusFound)
	loc, _ = url.Parse(back.header.Get("Location"))

	tok := do(http.MethodPost, base+"/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {loc.Query().Get("code")},
		"client_id":     {reg.ClientID},
		"redirect_uri":  {redirect},
		"code_verifier": {verifier},
	}, nil, http.StatusOK)
	var tokens struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(tok.body, &tokens)
	if tokens.AccessToken == "" {
		t.Fatal("no access token")
	}
	return tokens.AccessToken
}
