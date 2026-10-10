package gh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/go-github/v88/github"
)

// ErrUnauthorized means GitHub rejected the user's token: it expired or was revoked.
var ErrUnauthorized = errors.New("GitHub rejected the user's token")

// ErrBadGrant means GitHub refused a code or refresh token: used, expired or revoked.
var ErrBadGrant = errors.New("GitHub refused the code or refresh token")

// ErrAppConfig means GitHub refused the app's own OAuth settings: the client ID or secret, or the
// callback URL. Every sign-in and refresh fails until the operator fixes them.
var ErrAppConfig = errors.New("GitHub refused the app's OAuth settings: check github.client_id, github.client_secret and the app's callback URL")

// DefaultOAuthURL is where GitHub's OAuth endpoints live.
const DefaultOAuthURL = "https://github.com"

// OAuthConfig identifies the app for the user authorization flow.
type OAuthConfig struct {
	ClientID     string
	ClientSecret string
	// URL is GitHub's web address, for /login/oauth. Empty means DefaultOAuthURL.
	URL string
}

// UserTokens is what GitHub returns for a user authorization or a refresh.
type UserTokens struct {
	AccessToken    string
	AccessExpires  time.Time
	RefreshToken   string
	RefreshExpires time.Time
}

// AuthorizeURL returns the address that sends a user to GitHub to authorize the app.
func (o OAuthConfig) AuthorizeURL(redirectURI, state, codeChallenge string) string {
	q := url.Values{
		"client_id":             {o.ClientID},
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
	}
	return o.base() + "/login/oauth/authorize?" + q.Encode()
}

func (o OAuthConfig) base() string {
	if o.URL == "" {
		return DefaultOAuthURL
	}
	return strings.TrimSuffix(o.URL, "/")
}

// Exchange trades an authorization code for user tokens. Tokens that don't expire are refused: the app
// must have "Expire user authorization tokens" on.
func (c *Client) Exchange(ctx context.Context, o OAuthConfig, code, codeVerifier, redirectURI string) (UserTokens, error) {
	return c.tokenRequest(ctx, o, url.Values{
		"client_id":     {o.ClientID},
		"client_secret": {o.ClientSecret},
		"code":          {code},
		"code_verifier": {codeVerifier},
		"redirect_uri":  {redirectURI},
	})
}

// Refresh trades a refresh token for new user tokens. GitHub refresh tokens work once.
func (c *Client) Refresh(ctx context.Context, o OAuthConfig, refreshToken string) (UserTokens, error) {
	return c.tokenRequest(ctx, o, url.Values{
		"client_id":     {o.ClientID},
		"client_secret": {o.ClientSecret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
}

func (c *Client) tokenRequest(ctx context.Context, o OAuthConfig, form url.Values) (UserTokens, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.base()+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return UserTokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.web.Do(req)
	if err != nil {
		return UserTokens{}, fmt.Errorf("GitHub token request: %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken           string `json:"access_token"`
		ExpiresIn             int64  `json:"expires_in"`
		RefreshToken          string `json:"refresh_token"`
		RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
		Error                 string `json:"error"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body)
	if err != nil {
		return UserTokens{}, fmt.Errorf("GitHub token response (%s): %w", resp.Status, err)
	}
	switch body.Error {
	case "":
	case "bad_verification_code", "bad_refresh_token":
		return UserTokens{}, fmt.Errorf("%w: %s", ErrBadGrant, body.Error)
	case "incorrect_client_credentials", "redirect_uri_mismatch":
		return UserTokens{}, fmt.Errorf("%w: %s", ErrAppConfig, body.Error)
	default:
		return UserTokens{}, fmt.Errorf("GitHub token response: %s", body.Error)
	}
	if body.AccessToken == "" {
		return UserTokens{}, fmt.Errorf("GitHub token response without a token (%s)", resp.Status)
	}
	if body.ExpiresIn == 0 || body.RefreshToken == "" {
		return UserTokens{}, errors.New(`GitHub returned a token that doesn't expire: turn on "Expire user authorization tokens" for the app`)
	}
	now := time.Now()
	t := UserTokens{
		AccessToken:   body.AccessToken,
		AccessExpires: now.Add(time.Duration(body.ExpiresIn) * time.Second),
		RefreshToken:  body.RefreshToken,
	}
	if body.RefreshTokenExpiresIn > 0 { // otherwise unknown: the zero time
		t.RefreshExpires = now.Add(time.Duration(body.RefreshTokenExpiresIn) * time.Second)
	}
	return t, nil
}

// User is a GitHub account.
type User struct {
	ID    int64
	Login string
}

// RepoAccess is what a user may do with one repository.
type RepoAccess struct {
	RepoID int64
	Pull   bool
	Push   bool
}

// userClient returns an API client authenticated as the user.
func (c *Client) userClient(token string) (*github.Client, error) {
	return newGitHub(c.apiURL, c.metrics.observe("user", authTransport{token: token, next: c.base}))
}

type authTransport struct {
	token string
	next  http.RoundTripper
}

func (a authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+a.token)
	return a.next.RoundTrip(r)
}

// userError turns a 401 into ErrUnauthorized.
func userError(err error) error {
	if gerr, ok := errors.AsType[*github.ErrorResponse](err); ok && gerr.Response != nil && gerr.Response.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%w: %w", ErrUnauthorized, err)
	}
	return notFound(err)
}

// Me returns the user a token belongs to.
func (c *Client) Me(ctx context.Context, token string) (User, error) {
	client, err := c.userClient(token)
	if err != nil {
		return User{}, err
	}
	u, _, err := client.Users.Get(ctx, "")
	if err != nil {
		return User{}, fmt.Errorf("get user: %w", userError(err))
	}
	return User{ID: u.GetID(), Login: u.GetLogin()}, nil
}

// ActiveMember reports whether the token's user is an active member of org. Pending invitations don't
// count.
func (c *Client) ActiveMember(ctx context.Context, token, org string) (bool, error) {
	client, err := c.userClient(token)
	if err != nil {
		return false, err
	}
	m, _, err := client.Organizations.GetOrgMembership(ctx, "", org)
	if errors.Is(notFound(err), ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("membership of %s: %w", org, userError(err))
	}
	return m.GetState() == "active", nil
}

// UserRepositories returns what the token's user may do in each repository the app's installations can
// see, keyed by installation.
func (c *Client) UserRepositories(ctx context.Context, token string) ([]RepoAccess, error) {
	client, err := c.userClient(token)
	if err != nil {
		return nil, err
	}
	var installations []int64
	opt := &github.ListOptions{PerPage: 100}
	for {
		page, resp, err := client.Apps.ListUserInstallations(ctx, opt)
		if err != nil {
			return nil, fmt.Errorf("user installations: %w", userError(err))
		}
		for _, in := range page {
			installations = append(installations, in.GetID())
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}

	var out []RepoAccess
	for _, id := range installations {
		opt := &github.ListOptions{PerPage: 100}
		for {
			page, resp, err := client.Apps.ListUserRepos(ctx, id, opt)
			if err != nil {
				return nil, fmt.Errorf("repositories of installation %d: %w", id, userError(err))
			}
			for _, r := range page.Repositories {
				p := r.GetPermissions()
				out = append(out, RepoAccess{RepoID: r.GetID(), Pull: p.GetPull(), Push: p.GetPush()})
			}
			if resp.NextPage == 0 {
				break
			}
			opt.Page = resp.NextPage
		}
	}
	return out, nil
}

// Commit is one commit in a repository's history.
type Commit struct {
	SHA     string
	Author  string // GitHub login, or the git author name when the commit isn't linked to an account
	Date    time.Time
	Message string
}

// Commits returns the newest commits on branch, at most limit, as the token's user sees them. With a
// path, only commits that changed it.
func (c *Client) Commits(ctx context.Context, token, repo, branch, path string, limit int) ([]Commit, error) {
	client, err := c.userClient(token)
	if err != nil {
		return nil, err
	}
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	page, _, err := client.Repositories.ListCommits(ctx, owner, name, &github.CommitsListOptions{
		SHA:     branch,
		Path:    path,
		PerPage: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("commits of %s: %w", repo, userError(err))
	}
	out := make([]Commit, 0, len(page))
	for _, rc := range page {
		author := rc.GetAuthor().GetLogin()
		if author == "" {
			author = rc.GetCommit().GetAuthor().GetName()
		}
		out = append(out, Commit{
			SHA:     rc.GetSHA(),
			Author:  author,
			Date:    rc.GetCommit().GetAuthor().GetDate().Time,
			Message: rc.GetCommit().GetMessage(),
		})
	}
	return out, nil
}
