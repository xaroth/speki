package service_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xaroth/speki/gh"
	"github.com/xaroth/speki/service"
)

const (
	kbRepo     = "ours/kb"
	hiddenRepo = "ours/secret" // the app sees it, the user doesn't
	userToken  = "ghu_test"
	branch     = "master" // go-git's default
)

// gitHub is a fake GitHub whose repositories are also real git repositories under gitURL, which speki
// clones and fetches. Commits through the API land in both.
type gitHub struct {
	*gh.Fake
	gitURL string

	tb    testing.TB
	mu    sync.Mutex // go-git repositories aren't safe for concurrent use
	repos map[string]*git.Repository
}

// newGitHub serves ours/kb with kbFiles to octocat, who may write to it, and ours/secret with
// hiddenFiles, which octocat can't see, unless hiddenFiles is nil. The app is owned by account 42.
func newGitHub(tb testing.TB, kbFiles, hiddenFiles map[string]string, opts ...gh.FakeOption) *gitHub {
	tb.Helper()
	g := &gitHub{gitURL: tb.TempDir(), tb: tb, repos: map[string]*git.Repository{}}
	pushed := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	repos := []gh.Repository{{ID: 100, FullName: kbRepo, DefaultBranch: branch, PushedAt: pushed}}
	contents := map[string]map[string]string{kbRepo: kbFiles}
	if hiddenFiles != nil {
		repos = append(repos, gh.Repository{ID: 101, FullName: hiddenRepo, DefaultBranch: branch, PushedAt: pushed})
		contents[hiddenRepo] = hiddenFiles
	}
	setup := []gh.FakeOption{
		gh.ExpectAppOwner(42),
		gh.ExpectInstallation(gh.Installation{ID: 1, AccountID: 42}, repos...),
		gh.ExpectUser(userToken, gh.User{ID: 7, Login: "octocat"}),
		gh.ExpectRole(userToken, kbRepo, gh.RoleWrite),
		gh.ExpectOAuthCode("github-code", "", userToken), // speki makes its own PKCE verifier
		gh.ExpectCommits(kbRepo, gh.Commit{SHA: "c1", Author: "octocat", Date: pushed,
			Message: "Add fleet notes\n\nSpeki-Client: Claude Code\n"}),
		gh.ExpectCommitHook(kbRepo, g.commit),
	}
	for name, files := range contents {
		g.repos[name] = g.init(name)
		changes := make([]gh.FileChange, 0, len(files))
		for path, content := range files {
			changes = append(changes, gh.FileChange{Path: path, Content: []byte(content)})
			setup = append(setup, gh.ExpectFile(name, path, []byte(content)))
		}
		sha := g.commit(gh.FakeCommit{Repo: name, Author: "octocat", Message: "push", Changes: changes})
		setup = append(setup, gh.ExpectBranch(name, branch, sha))
	}
	g.Fake = gh.NewFake(tb, append(setup, opts...)...)
	return g
}

func (g *gitHub) dir(repo string) string { return filepath.Join(g.gitURL, repo+".git") }

func (g *gitHub) init(repo string) *git.Repository {
	g.tb.Helper()
	r, err := git.PlainInit(g.dir(repo), false)
	if err != nil {
		g.tb.Fatal(err)
	}
	return r
}

// commit makes c in the git repository, and returns its SHA.
func (g *gitHub) commit(c gh.FakeCommit) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	wt, err := g.repos[c.Repo].Worktree()
	if err != nil {
		g.tb.Error(err)
		return ""
	}
	for _, change := range c.Changes {
		if change.Content == nil {
			_, err = wt.Remove(change.Path)
		} else {
			full := filepath.Join(g.dir(c.Repo), change.Path)
			err = os.MkdirAll(filepath.Dir(full), 0o755)
			if err == nil {
				err = os.WriteFile(full, change.Content, 0o644)
			}
			if err == nil {
				_, err = wt.Add(change.Path)
			}
		}
		if err != nil {
			g.tb.Error(err)
			return ""
		}
	}
	sha, err := wt.Commit(c.Message, &git.CommitOptions{Author: &object.Signature{Name: c.Author, Email: c.Author + "@example.com", When: time.Now()}})
	if err != nil {
		g.tb.Error(err)
		return ""
	}
	return sha.String()
}

// headMessage returns the message of the last commit to repo.
func (g *gitHub) headMessage(repo string) string {
	g.tb.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.repos[repo]
	head, err := r.Head()
	if err != nil {
		g.tb.Fatal(err)
	}
	c, err := r.CommitObject(head.Hash())
	if err != nil {
		g.tb.Fatal(err)
	}
	return c.Message
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// validConfig has every required setting, and nothing else.
func validConfig(tb testing.TB) service.Config {
	tb.Helper()
	return service.Config{
		PublicURL: "https://speki.example.com",
		GitHub: service.GitHub{
			AppID:        1,
			PrivateKey:   string(gh.NewFake(tb).PrivateKey), // made once per process: RSA keys are slow
			ClientID:     "Iv1",
			ClientSecret: "s",
		},
		Auth: service.Auth{Keys: testKeys()},
	}
}

// testConfig returns a configuration that talks to g, listens on free ports and keeps its data in a
// temporary directory.
func testConfig(tb testing.TB, g *gitHub) service.Config {
	tb.Helper()
	return service.Config{
		PublicURL:     "https://speki.example.com",
		Listen:        "127.0.0.1:0",
		MetricsListen: "127.0.0.1:0",
		GitHub: service.GitHub{
			AppID:        g.AppID,
			PrivateKey:   string(g.PrivateKey),
			ClientID:     g.OAuth.ClientID,
			ClientSecret: g.OAuth.ClientSecret,
			APIURL:       g.URL,
			OAuthURL:     g.URL,
			GitURL:       g.gitURL,
		},
		Auth: service.Auth{Keys: testKeys()},
		Sync: service.Sync{DataDir: tb.TempDir()},
	}
}

func testKeys() string {
	return "keys:\n  - id: test\n    secret: " + testSecret() + "\n    primary: true\n"
}

func testSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// newMu serializes service.New, which sets SQLite's process-wide memory limits without a lock.
var newMu sync.Mutex

// create is service.New, safe to call from parallel tests.
func create(cfg service.Config, logger *slog.Logger) (*service.Service, error) {
	newMu.Lock()
	defer newMu.Unlock()
	return service.New(cfg, logger)
}

func newService(tb testing.TB, cfg service.Config) *service.Service {
	tb.Helper()
	s, err := create(cfg, discardLogger())
	if err != nil {
		tb.Fatalf("New: %v", err)
	}
	return s
}

// run runs s until the test ends or stop is called. stop returns what Run returned.
func run(tb testing.TB, s *service.Service) (stop func() error) {
	tb.Helper()
	ctx, cancel := context.WithCancel(tb.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	stop = sync.OnceValue(func() error {
		cancel()
		return <-done
	})
	tb.Cleanup(func() {
		if err := stop(); err != nil {
			tb.Errorf("Run: %v", err)
		}
	})
	return stop
}

// startService runs a service until the test ends, and returns once its vaults are served.
func startService(tb testing.TB, cfg service.Config) *service.Service {
	tb.Helper()
	s := newService(tb, cfg)
	run(tb, s)
	s.WaitServing()
	return s
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func get(tb testing.TB, target string) response {
	tb.Helper()
	req, err := http.NewRequestWithContext(tb.Context(), http.MethodGet, target, nil)
	if err != nil {
		tb.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		tb.Fatal(err)
	}
	return response{status: resp.StatusCode, header: resp.Header, body: body}
}

type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

func connectMCP(tb testing.TB, s *service.Service, token string) *mcp.ClientSession {
	tb.Helper()
	public, _ := s.URLs()
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "1"}, nil)
	cs, err := client.Connect(tb.Context(), &mcp.StreamableClientTransport{
		Endpoint:             public + "/mcp",
		HTTPClient:           &http.Client{Transport: bearer(token)},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = cs.Close() })
	return cs
}

// callTool calls a tool that must succeed, and returns its text.
func callTool(tb testing.TB, cs *mcp.ClientSession, name string, args any) string {
	tb.Helper()
	text, isError := tryTool(tb, cs, name, args)
	if isError {
		tb.Fatalf("%s: %s", name, text)
	}
	return text
}

// tryTool calls a tool, and returns its text and whether it's an error.
func tryTool(tb testing.TB, cs *mcp.ClientSession, name string, args any) (string, bool) {
	tb.Helper()
	res, err := cs.CallTool(tb.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		tb.Fatalf("%s: %v", name, err)
	}
	return res.Content[0].(*mcp.TextContent).Text, res.IsError
}

var consentField = regexp.MustCompile(`name="consent" value="([^"]+)"`)

// signedIn is what a client holds after signing in.
type signedIn struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ClientID     string `json:"-"`
}

// signIn registers a client and goes through the browser flow with write access.
func signIn(tb testing.TB, s *service.Service) signedIn {
	tb.Helper()
	base, _ := s.URLs()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	type response struct {
		header  http.Header
		cookies []*http.Cookie
		body    []byte
	}
	do := func(method, target, contentType string, body io.Reader, cookies []*http.Cookie, want int) response {
		tb.Helper()
		req, err := http.NewRequestWithContext(tb.Context(), method, target, body)
		if err != nil {
			tb.Fatal(err)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		for _, c := range cookies {
			req.AddCookie(c) // __Host- cookies are Secure, so a cookie jar wouldn't send them over plain HTTP
		}
		resp, err := noRedirect.Do(req)
		if err != nil {
			tb.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		got, err := io.ReadAll(resp.Body)
		if err != nil {
			tb.Fatal(err)
		}
		if resp.StatusCode != want {
			tb.Fatalf("%s %s: got %d, want %d: %s", method, target, resp.StatusCode, want, got)
		}
		return response{header: resp.Header, cookies: resp.Cookies(), body: got}
	}
	form := func(v url.Values) io.Reader { return strings.NewReader(v.Encode()) }
	const formType = "application/x-www-form-urlencoded"

	const redirect = "http://127.0.0.1:5555/callback"
	reg := do(http.MethodPost, base+"/oauth/register", "application/json",
		strings.NewReader(`{"client_name":"e2e","redirect_uris":["`+redirect+`"]}`), nil, http.StatusCreated)
	var client struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(reg.body, &client); err != nil {
		tb.Fatal(err)
	}

	verifier := "a-client-verifier-that-is-long-enough-for-pkce"
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {client.ClientID},
		"redirect_uri":          {redirect},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"state":                 {"st"},
		"resource":              {"https://speki.example.com/mcp"},
	}
	page := do(http.MethodGet, base+"/oauth/authorize?"+q.Encode(), "", nil, nil, http.StatusOK)
	m := consentField.FindSubmatch(page.body)
	if m == nil {
		tb.Fatal("got no consent field on the consent page")
	}

	toGitHub := do(http.MethodPost, base+"/oauth/authorize", formType,
		form(url.Values{"consent": {string(m[1])}, "access": {"write"}, "decision": {"allow"}}), page.cookies, http.StatusSeeOther)
	loc, err := url.Parse(toGitHub.header.Get("Location"))
	if err != nil {
		tb.Fatal(err)
	}
	back := do(http.MethodGet, base+"/oauth/github/callback?code=github-code&state="+loc.Query().Get("state"),
		"", nil, toGitHub.cookies, http.StatusFound)
	loc, err = url.Parse(back.header.Get("Location"))
	if err != nil {
		tb.Fatal(err)
	}

	tok := do(http.MethodPost, base+"/oauth/token", formType, form(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {loc.Query().Get("code")},
		"client_id":     {client.ClientID},
		"redirect_uri":  {redirect},
		"code_verifier": {verifier},
	}), nil, http.StatusOK)
	var tokens signedIn
	if err := json.Unmarshal(tok.body, &tokens); err != nil || tokens.AccessToken == "" {
		tb.Fatalf("got token response %s, want an access token", tok.body)
	}
	tokens.ClientID = client.ClientID
	return tokens
}
