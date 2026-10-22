// Package ghtest is a fake GitHub for tests of code that uses gh. It lives apart from gh so the
// production binary doesn't link testing and httptest.
package ghtest

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
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xaroth/speki/gh"
)

// Fake is a gh.Client talking to a fake GitHub on httptest: the REST endpoints, GraphQL commits and the
// OAuth token endpoint speki uses. Its state is set up with Expect options and can be read back.
type Fake struct {
	*gh.Client

	URL        string         // API and OAuth base URL
	AppID      int64          // the app's ID; any JWT is accepted as the app's
	PrivateKey []byte         // PEM, for clients built from configuration
	OAuth      gh.OAuthConfig // the client ID and secret the fake accepts

	tb           testing.TB
	mu           sync.Mutex
	owner        int64
	installs     []*fakeInstallation
	repos        map[string]*fakeRepo // by full name
	users        map[string]*fakeUser // by token
	teams        map[string]string    // org/slug/login: state
	codes        map[string]fakeCode
	refresh      map[string]string // refresh token: access token; deleted once used
	budgets      map[string]int    // remaining rate limit budget per credential
	pageSize     int
	issued       int // tokens and SHAs handed out, to keep them unique
	routes       map[string]func(http.Handler) http.Handler
	commitErrors []map[string]string
	commitHooks  map[string]func(Commit) string // by repository full name
	requests     []string
}

type fakeInstallation struct {
	gh.Installation
	repos   []string
	refused bool
}

type fakeRepo struct {
	gh.Repository
	installation int64
	files        map[string][]byte
	heads        map[string]string
	history      []gh.Commit // newest first
}

type fakeUser struct {
	gh.User
	orgs  map[string]string  // org: state
	roles map[string]gh.Role // repository full name: role
}

type fakeCode struct{ verifier, token string }

// Option sets up a Fake.
type Option func(testing.TB, *Fake)

const fakeLimit = 5000

// fakeKey is generated once: RSA key generation is slow, more so with -race.
var fakeKey = sync.OnceValues(func() ([]byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), nil
})

// New starts a fake GitHub with opts applied and returns a gh.Client pointed at it. The server stops
// when the test ends. A request the fake doesn't know fails the test.
func New(tb testing.TB, opts ...Option) *Fake {
	tb.Helper()
	key, err := fakeKey()
	if err != nil {
		tb.Fatalf("fake GitHub key: %v", err)
	}
	f := &Fake{
		AppID:       1,
		PrivateKey:  key,
		tb:          tb,
		repos:       map[string]*fakeRepo{},
		users:       map[string]*fakeUser{},
		teams:       map[string]string{},
		codes:       map[string]fakeCode{},
		refresh:     map[string]string{},
		budgets:     map[string]int{},
		pageSize:    100,
		routes:      map[string]func(http.Handler) http.Handler{},
		commitHooks: map[string]func(Commit) string{},
	}
	for _, opt := range opts {
		opt(tb, f)
	}
	srv := httptest.NewServer(f.handler())
	tb.Cleanup(srv.Close)
	f.URL = srv.URL
	f.OAuth = gh.OAuthConfig{ClientID: "Iv1.fake", ClientSecret: "fake-secret", URL: srv.URL}
	f.Client, err = gh.New(gh.Config{AppID: f.AppID, PrivateKey: key, APIURL: srv.URL})
	if err != nil {
		tb.Fatalf("fake GitHub client: %v", err)
	}
	return f
}

// Apply applies opts to a running fake, for changes in the middle of a test, such as a push someone
// made on GitHub. The options apply together: no request sees some of them without the others.
func (f *Fake) Apply(opts ...Option) {
	f.tb.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, opt := range opts {
		opt(f.tb, f)
	}
}

// ExpectAppOwner sets the ID of the account that owns the app.
func ExpectAppOwner(id int64) Option {
	return func(_ testing.TB, f *Fake) { f.owner = id }
}

// ExpectInstallation adds an installation that can see repos. A repository's files and branches are
// set with ExpectFile and ExpectBranch.
func ExpectInstallation(in gh.Installation, repos ...gh.Repository) Option {
	return func(tb testing.TB, f *Fake) {
		tb.Helper()
		fi := &fakeInstallation{Installation: in}
		for _, r := range repos {
			if _, ok := f.repos[r.FullName]; ok {
				tb.Fatalf("fake GitHub: repository %s added twice", r.FullName)
			}
			f.repos[r.FullName] = &fakeRepo{Repository: r, installation: in.ID, files: map[string][]byte{}, heads: map[string]string{}}
			fi.repos = append(fi.repos, r.FullName)
		}
		f.installs = append(f.installs, fi)
	}
}

// ExpectInstallation_Refused makes GitHub refuse the installation's tokens with a 403, as it does when
// rate limited. Add the installation with ExpectInstallation first. An installation that was never
// added is gone: its tokens are a 404.
//
//nolint:revive // Expect<Thing>_<Case> is the naming for shared expectations
func ExpectInstallation_Refused(id int64) Option {
	return func(tb testing.TB, f *Fake) {
		tb.Helper()
		in := f.install(id)
		if in == nil {
			tb.Fatalf("fake GitHub: no installation %d; add it with ExpectInstallation first", id)
		}
		in.refused = true
	}
}

// ExpectFile puts a file in a repository added with ExpectInstallation.
func ExpectFile(repo, path string, content []byte) Option {
	return func(tb testing.TB, f *Fake) {
		tb.Helper()
		f.repo(tb, repo).files[path] = content
	}
}

// ExpectBranch points a branch of a repository at sha. Its ETag is the quoted SHA.
func ExpectBranch(repo, branch, sha string) Option {
	return func(tb testing.TB, f *Fake) {
		tb.Helper()
		f.repo(tb, repo).heads[branch] = sha
	}
}

// ExpectCommits sets a repository's history, newest first. Listing from one of its SHAs starts there. An
// author that's the login of a user added with ExpectUser is linked to the account; others are git author
// names only.
func ExpectCommits(repo string, commits ...gh.Commit) Option {
	return func(tb testing.TB, f *Fake) {
		tb.Helper()
		f.repo(tb, repo).history = commits
	}
}

// ExpectUser adds a user with a token. Tokens the fake doesn't know are rejected with a 401.
func ExpectUser(token string, u gh.User) Option {
	return func(_ testing.TB, f *Fake) {
		f.users[token] = &fakeUser{User: u, orgs: map[string]string{}, roles: map[string]gh.Role{}}
	}
}

// ExpectOrgMembership sets the token's user's membership state in org: "active" or "pending".
func ExpectOrgMembership(token, org, state string) Option {
	return func(tb testing.TB, f *Fake) {
		tb.Helper()
		f.user(tb, token).orgs[org] = state
	}
}

// ExpectTeamMembership sets login's membership state in the team org/slug, visible to every user.
func ExpectTeamMembership(org, slug, login, state string) Option {
	return func(_ testing.TB, f *Fake) { f.teams[org+"/"+slug+"/"+login] = state }
}

// ExpectRole gives the token's user a role in a repository. Users see the installations and
// repositories they have a role in, and commit to those where it's gh.RoleWrite or more.
func ExpectRole(token, repo string, role gh.Role) Option {
	return func(tb testing.TB, f *Fake) {
		tb.Helper()
		f.user(tb, token).roles[repo] = role
	}
}

// ExpectOAuthCode makes an authorization code, with its PKCE verifier, good for accessToken. The code
// works any number of times. An empty verifier accepts any, for callers that make their own. Each answer carries a new refresh token, which works once and is good for
// the same access token.
func ExpectOAuthCode(code, verifier, accessToken string) Option {
	return func(_ testing.TB, f *Fake) { f.codes[code] = fakeCode{verifier: verifier, token: accessToken} }
}

// ExpectCommit_Error makes every commit fail with a GraphQL error of type typ, such as "FORBIDDEN".
//
//nolint:revive // Expect<Thing>_<Case> is the naming for shared expectations
func ExpectCommit_Error(typ, message string) Option {
	return func(_ testing.TB, f *Fake) {
		f.commitErrors = append(f.commitErrors, map[string]string{"type": typ, "message": message})
	}
}

// Commit is a commit made through the fake's GraphQL API, as an ExpectCommitHook hook sees it.
type Commit struct {
	Repo    string
	Branch  string
	Author  string // the committing user's login
	Message string
	Changes []gh.FileChange // deletions first; nil Content deletes the path
}

// ExpectCommitHook calls hook for every commit made to repo, before it lands, and moves the branch to
// the SHA hook returns rather than a made-up one. It lets the branch follow a real repository that
// clients fetch from: hook commits the changes there and returns that commit's SHA. It runs with the
// fake locked, so hooks never run at once.
func ExpectCommitHook(repo string, hook func(Commit) string) Option {
	return func(tb testing.TB, f *Fake) {
		tb.Helper()
		f.repo(tb, repo)
		f.commitHooks[repo] = hook
	}
}

// ExpectPageSize caps how many items a list returns per page, so tests can page with few items.
func ExpectPageSize(n int) Option {
	return func(_ testing.TB, f *Fake) { f.pageSize = n }
}

// ExpectRateLimit sets the rate limit budget left for an installation, or for the app when
// installationID is 0. Budgets start at 5000 and each request uses one.
func ExpectRateLimit(installationID int64, remaining int) Option {
	return func(_ testing.TB, f *Fake) { f.budgets[budgetKey(installationID)] = remaining }
}

// ExpectRoute wraps the fake's handler for one route, "METHOD /path" with the path as sent, for
// answers the fake doesn't model or to hold a request: call next to get the fake's own answer.
func ExpectRoute(route string, wrap func(next http.Handler) http.Handler) Option {
	return func(_ testing.TB, f *Fake) { f.routes[route] = wrap }
}

// Requests returns the requests made so far, as "METHOD /path?query".
func (f *Fake) Requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

// FileContent returns a file of a repository as it is now.
func (f *Fake) FileContent(repo, path string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.repos[repo]
	if !ok {
		return nil, false
	}
	content, ok := r.files[path]
	return content, ok
}

// BranchSHA returns the commit a branch of a repository points to now.
func (f *Fake) BranchSHA(repo, branch string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.repos[repo]; ok {
		return r.heads[branch]
	}
	return ""
}

func (f *Fake) install(id int64) *fakeInstallation {
	for _, in := range f.installs {
		if in.ID == id {
			return in
		}
	}
	return nil
}

func (f *Fake) repo(tb testing.TB, name string) *fakeRepo {
	tb.Helper()
	r, ok := f.repos[name]
	if !ok {
		tb.Fatalf("fake GitHub: no repository %s; add it with ExpectInstallation first", name)
	}
	return r
}

func (f *Fake) user(tb testing.TB, token string) *fakeUser {
	tb.Helper()
	u, ok := f.users[token]
	if !ok {
		tb.Fatalf("fake GitHub: no user with token %s; add it with ExpectUser first", token)
	}
	return u
}

func budgetKey(installationID int64) string {
	if installationID == 0 {
		return "app"
	}
	return "installation/" + strconv.FormatInt(installationID, 10)
}

// credential is who a request is from: the app, an installation or a user. The zero value is nobody.
type credential struct {
	app          bool
	installation int64
	user         *fakeUser
	budget       string
}

func (f *Fake) credential(r *http.Request) credential {
	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if scheme != "Bearer" && scheme != "token" {
		return credential{}
	}
	if strings.Count(token, ".") == 2 {
		return credential{app: true, budget: budgetKey(0)}
	}
	if s, ok := strings.CutPrefix(token, "ghs_fake_"); ok {
		id, _ := strconv.ParseInt(s, 10, 64)
		if f.install(id) != nil {
			return credential{installation: id, budget: budgetKey(id)}
		}
	}
	if u, ok := f.users[token]; ok {
		return credential{user: u, budget: "user/" + token}
	}
	return credential{}
}

// visible returns the repository if the credential can see it.
func (c credential) visible(f *Fake, name string) *fakeRepo {
	r, ok := f.repos[name]
	switch {
	case !ok:
		return nil
	case c.installation != 0 && r.installation == c.installation, c.user != nil && c.user.roles[name] > gh.RoleNone:
		return r
	}
	return nil
}

type fakeHandler func(w *fakeWriter, r *http.Request, c credential)

type fakeWriter struct {
	http.ResponseWriter
	f      *Fake
	r      *http.Request
	budget string
}

// json writes v with status, and the credential's rate limit headers as GitHub sends them.
func (w *fakeWriter) json(status int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	if w.budget != "" {
		remaining, ok := w.f.budgets[w.budget]
		if !ok {
			remaining = fakeLimit
		}
		remaining = max(remaining-1, 0)
		w.f.budgets[w.budget] = remaining
		h.Set("X-RateLimit-Limit", strconv.Itoa(fakeLimit))
		h.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (w *fakeWriter) message(status int, msg string) {
	w.json(status, map[string]any{"message": msg})
}

// page writes the items of one page and links to the next, as GitHub does. wrap shapes the page; nil
// writes a bare array.
func (w *fakeWriter) page(items []any, wrap func(total int, page []any) any) {
	q := w.r.URL.Query()
	size := w.f.pageSize
	if n, err := strconv.Atoi(q.Get("per_page")); err == nil && n > 0 {
		size = min(size, n)
	}
	page := max(1, atoi(q.Get("page")))
	start := min((page-1)*size, len(items))
	end := min(start+size, len(items))
	if end < len(items) {
		next := *w.r.URL
		q.Set("page", strconv.Itoa(page+1))
		next.RawQuery = q.Encode()
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s>; rel="next"`, w.r.Host, next.RequestURI()))
	}
	var body any = items[start:end]
	if wrap != nil {
		body = wrap(len(items), items[start:end])
	}
	w.json(http.StatusOK, body)
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func (f *Fake) handler() http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, need func(credential) bool, h fakeHandler) {
		mux.HandleFunc(pattern, func(rw http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			c := f.credential(r)
			w := &fakeWriter{ResponseWriter: rw, f: f, r: r, budget: c.budget}
			if need != nil && !need(c) {
				w.message(http.StatusUnauthorized, "Bad credentials")
				return
			}
			h(w, r, c)
		})
	}
	app := func(c credential) bool { return c.app }
	installation := func(c credential) bool { return c.installation != 0 }
	user := func(c credential) bool { return c.user != nil }
	repoReader := func(c credential) bool { return c.installation != 0 || c.user != nil }

	route("GET /app", app, f.serveApp)
	route("GET /app/installations", app, f.serveInstallations)
	route("POST /app/installations/{id}/access_tokens", app, f.serveInstallationToken)
	route("GET /installation/repositories", installation, f.serveInstallationRepos)
	route("GET /repos/{owner}/{repo}", repoReader, f.serveRepo)
	route("GET /repos/{owner}/{repo}/contents/{path...}", repoReader, f.serveContents)
	route("GET /repos/{owner}/{repo}/git/ref/heads/{branch...}", repoReader, f.serveBranch)
	route("GET /repos/{owner}/{repo}/commits", repoReader, f.serveCommits)
	route("GET /user", user, f.serveUser)
	route("GET /user/memberships/orgs/{org}", user, f.serveOrgMembership)
	route("GET /orgs/{org}/teams/{slug}/memberships/{login}", user, f.serveTeamMembership)
	route("GET /user/installations", user, f.serveUserInstallations)
	route("GET /user/installations/{id}/repositories", user, f.serveUserRepos)
	route("POST /graphql", user, f.serveGraphQL)
	route("POST /login/oauth/access_token", nil, f.serveOAuthToken)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.tb.Errorf("fake GitHub: unexpected request %s %s", r.Method, r.URL)
		http.NotFound(w, r)
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
		wrap := f.routes[r.Method+" "+r.URL.Path]
		f.mu.Unlock()
		if wrap != nil {
			wrap(mux).ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (f *Fake) serveApp(w *fakeWriter, _ *http.Request, _ credential) {
	w.json(http.StatusOK, map[string]any{"id": f.AppID, "owner": map[string]any{"id": f.owner}})
}

func (f *Fake) serveInstallations(w *fakeWriter, _ *http.Request, _ credential) {
	var items []any
	for _, in := range f.installs {
		item := map[string]any{"id": in.ID, "account": map[string]any{"id": in.AccountID}}
		if in.Suspended {
			item["suspended_at"] = "2026-01-01T00:00:00Z"
		}
		items = append(items, item)
	}
	w.page(items, nil)
}

func (f *Fake) serveInstallationToken(w *fakeWriter, r *http.Request, _ credential) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	in := f.install(id)
	switch {
	case in == nil:
		w.message(http.StatusNotFound, "Not Found")
	case in.refused:
		w.message(http.StatusForbidden, "You have exceeded a secondary rate limit")
	default:
		w.json(http.StatusCreated, map[string]any{"token": fmt.Sprintf("ghs_fake_%d", id), "expires_at": time.Now().Add(time.Hour)})
	}
}

func repoJSON(r *fakeRepo) map[string]any {
	out := map[string]any{"id": r.ID, "full_name": r.FullName, "default_branch": r.DefaultBranch}
	if !r.PushedAt.IsZero() {
		out["pushed_at"] = r.PushedAt.Format(time.RFC3339)
	}
	return out
}

func reposPage(total int, page []any) any {
	return map[string]any{"total_count": total, "repositories": page}
}

func (f *Fake) serveInstallationRepos(w *fakeWriter, _ *http.Request, c credential) {
	var items []any
	for _, name := range f.install(c.installation).repos {
		items = append(items, repoJSON(f.repos[name]))
	}
	w.page(items, reposPage)
}

func (f *Fake) repoOf(w *fakeWriter, r *http.Request, c credential) *fakeRepo {
	repo := c.visible(f, r.PathValue("owner")+"/"+r.PathValue("repo"))
	if repo == nil {
		w.message(http.StatusNotFound, "Not Found")
	}
	return repo
}

func (f *Fake) serveRepo(w *fakeWriter, r *http.Request, c credential) {
	if repo := f.repoOf(w, r, c); repo != nil {
		w.json(http.StatusOK, repoJSON(repo))
	}
}

// serveContents answers for the files as they are now, whatever the ref.
func (f *Fake) serveContents(w *fakeWriter, r *http.Request, c credential) {
	repo := f.repoOf(w, r, c)
	if repo == nil {
		return
	}
	path := r.PathValue("path")
	if content, ok := repo.files[path]; ok {
		file := map[string]any{"type": "file", "path": path, "size": len(content), "encoding": "base64",
			"content": base64.StdEncoding.EncodeToString(content)}
		if len(content) > 1<<20 { // GitHub sends no content over 1 MB
			file["encoding"], file["content"] = "none", ""
		}
		w.json(http.StatusOK, file)
		return
	}
	var entries []any
	for name := range repo.files {
		if rest, ok := strings.CutPrefix(name, path+"/"); ok {
			entries = append(entries, map[string]any{"type": "file", "name": rest, "path": name})
		}
	}
	if entries == nil {
		w.message(http.StatusNotFound, "Not Found")
		return
	}
	w.json(http.StatusOK, entries)
}

func (f *Fake) serveBranch(w *fakeWriter, r *http.Request, c credential) {
	repo := f.repoOf(w, r, c)
	if repo == nil {
		return
	}
	branch := r.PathValue("branch")
	sha, ok := repo.heads[branch]
	if !ok {
		w.message(http.StatusNotFound, "Not Found")
		return
	}
	etag := strconv.Quote(sha)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	w.json(http.StatusOK, map[string]any{"ref": "refs/heads/" + branch, "object": map[string]any{"sha": sha, "type": "commit"}})
}

func (f *Fake) serveCommits(w *fakeWriter, r *http.Request, c credential) {
	repo := f.repoOf(w, r, c)
	if repo == nil {
		return
	}
	logins := map[string]bool{}
	for _, u := range f.users {
		logins[u.Login] = true
	}
	// The history is one line of commits: a SHA in it starts there, anything else is the branch head.
	history := repo.history
	for i, cm := range history {
		if cm.SHA == r.URL.Query().Get("sha") {
			history = history[i:]
			break
		}
	}
	var items []any
	for _, cm := range history {
		var author any
		if logins[cm.Author] {
			author = map[string]any{"login": cm.Author}
		}
		items = append(items, map[string]any{"sha": cm.SHA, "author": author, "commit": map[string]any{
			"message": cm.Message,
			"author":  map[string]any{"name": cm.Author, "date": cm.Date.Format(time.RFC3339)},
		}})
	}
	w.page(items, nil)
}

func (f *Fake) serveUser(w *fakeWriter, _ *http.Request, c credential) {
	w.json(http.StatusOK, map[string]any{"id": c.user.ID, "login": c.user.Login})
}

func (f *Fake) serveOrgMembership(w *fakeWriter, r *http.Request, c credential) {
	state, ok := c.user.orgs[r.PathValue("org")]
	if !ok {
		w.message(http.StatusNotFound, "Not Found")
		return
	}
	w.json(http.StatusOK, map[string]any{"state": state})
}

func (f *Fake) serveTeamMembership(w *fakeWriter, r *http.Request, _ credential) {
	state, ok := f.teams[r.PathValue("org")+"/"+r.PathValue("slug")+"/"+r.PathValue("login")]
	if !ok {
		w.message(http.StatusNotFound, "Not Found")
		return
	}
	w.json(http.StatusOK, map[string]any{"state": state})
}

// userRepos returns the repositories of an installation the user has a role in.
func (f *Fake) userRepos(in *fakeInstallation, u *fakeUser) []string {
	var out []string
	for _, name := range in.repos {
		if u.roles[name] > gh.RoleNone {
			out = append(out, name)
		}
	}
	return out
}

func (f *Fake) serveUserInstallations(w *fakeWriter, _ *http.Request, c credential) {
	var items []any
	for _, in := range f.installs {
		if len(f.userRepos(in, c.user)) > 0 {
			items = append(items, map[string]any{"id": in.ID, "account": map[string]any{"id": in.AccountID}})
		}
	}
	w.page(items, func(total int, page []any) any {
		return map[string]any{"total_count": total, "installations": page}
	})
}

func (f *Fake) serveUserRepos(w *fakeWriter, r *http.Request, c credential) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	in := f.install(id)
	if in == nil {
		w.message(http.StatusNotFound, "Not Found")
		return
	}
	var items []any
	for _, name := range f.userRepos(in, c.user) {
		role := c.user.roles[name]
		item := repoJSON(f.repos[name])
		// GitHub sets every permission the role includes.
		item["permissions"] = map[string]any{
			"pull": role >= gh.RoleRead, "triage": role >= gh.RoleTriage, "push": role >= gh.RoleWrite,
			"maintain": role >= gh.RoleMaintain, "admin": role >= gh.RoleAdmin,
		}
		items = append(items, item)
	}
	w.page(items, reposPage)
}

// serveGraphQL answers createCommitOnBranch, committing to the repository's files.
func (f *Fake) serveGraphQL(w *fakeWriter, r *http.Request, c credential) {
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
					Additions []struct {
						Path     string  `json:"path"`
						Contents *string `json:"contents"`
					} `json:"additions"`
					Deletions []struct {
						Path string `json:"path"`
					} `json:"deletions"`
				} `json:"fileChanges"`
			} `json:"input"`
		} `json:"variables"`
	}
	fail := func(typ, msg string) {
		w.json(http.StatusOK, map[string]any{"data": map[string]any{"createCommitOnBranch": nil},
			"errors": []any{map[string]any{"type": typ, "message": msg}}})
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.message(http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	if len(f.commitErrors) > 0 {
		w.json(http.StatusOK, map[string]any{"data": map[string]any{"createCommitOnBranch": nil}, "errors": f.commitErrors})
		return
	}
	in := req.Variables.Input
	repo := c.visible(f, in.Branch.Repo)
	if repo == nil {
		fail("NOT_FOUND", fmt.Sprintf("Could not resolve to a Repository with the name '%s'.", in.Branch.Repo))
		return
	}
	if c.user.roles[in.Branch.Repo] < gh.RoleWrite {
		fail("FORBIDDEN", "Resource not accessible by integration")
		return
	}
	head, ok := repo.heads[in.Branch.Branch]
	if !ok {
		fail("NOT_FOUND", fmt.Sprintf("Could not resolve to a Ref with the name 'refs/heads/%s'.", in.Branch.Branch))
		return
	}
	if head != in.Head {
		fail("STALE_DATA", fmt.Sprintf("Expected branch to point to %q but it did not. Pull and try again.", in.Head))
		return
	}
	changes := map[string][]byte{}
	var ordered []gh.FileChange
	for _, d := range in.FileChanges.Deletions {
		ordered = append(ordered, gh.FileChange{Path: d.Path})
	}
	for _, a := range in.FileChanges.Additions {
		if a.Contents == nil {
			fail("", "Variable $input of type CreateCommitOnBranchInput! was provided invalid value for fileChanges.additions.contents")
			return
		}
		content, err := base64.StdEncoding.DecodeString(*a.Contents)
		if err != nil {
			fail("", "contents must be base64: "+err.Error())
			return
		}
		changes[a.Path] = content
		ordered = append(ordered, gh.FileChange{Path: a.Path, Content: content})
	}
	msg := in.Message.Headline
	if in.Message.Body != "" {
		msg += "\n\n" + in.Message.Body
	}
	var sha string
	if hook := f.commitHooks[in.Branch.Repo]; hook != nil {
		sha = hook(Commit{Repo: in.Branch.Repo, Branch: in.Branch.Branch, Author: c.user.Login, Message: msg, Changes: ordered})
	} else {
		f.issued++
		sha = fmt.Sprintf("%040x", f.issued)
	}
	for _, d := range in.FileChanges.Deletions {
		delete(repo.files, d.Path)
	}
	for path, content := range changes {
		repo.files[path] = content
	}
	repo.heads[in.Branch.Branch] = sha
	repo.history = append([]gh.Commit{{SHA: sha, Author: c.user.Login, Date: time.Now().UTC().Truncate(time.Second), Message: msg}}, repo.history...)
	w.json(http.StatusOK, map[string]any{"data": map[string]any{"createCommitOnBranch": map[string]any{"commit": map[string]any{"oid": sha}}}})
}

func (f *Fake) serveOAuthToken(w *fakeWriter, r *http.Request, _ credential) {
	_ = r.ParseForm()
	fail := func(code string) { w.json(http.StatusOK, map[string]any{"error": code}) }
	if r.Form.Get("client_id") != f.OAuth.ClientID || r.Form.Get("client_secret") != f.OAuth.ClientSecret {
		fail("incorrect_client_credentials")
		return
	}
	var access string
	if old := r.Form.Get("refresh_token"); old != "" {
		var ok bool
		access, ok = f.refresh[old]
		if !ok {
			fail("bad_refresh_token")
			return
		}
		delete(f.refresh, old)
	} else {
		code, ok := f.codes[r.Form.Get("code")]
		if !ok || (code.verifier != "" && code.verifier != r.Form.Get("code_verifier")) {
			fail("bad_verification_code")
			return
		}
		access = code.token
	}
	f.issued++
	refresh := fmt.Sprintf("ghr_fake_%d", f.issued)
	f.refresh[refresh] = access
	w.json(http.StatusOK, map[string]any{"access_token": access, "expires_in": 28800,
		"refresh_token": refresh, "refresh_token_expires_in": 15897600})
}
