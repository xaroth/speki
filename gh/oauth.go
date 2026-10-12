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
)

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

// AuthorizeURL returns the address that sends a user to GitHub to authorize the app. GitHub always shows
// its account picker, so someone with a personal and a work account picks the right one.
func (o OAuthConfig) AuthorizeURL(redirectURI, state, codeChallenge string) string {
	q := url.Values{
		"prompt":                {"select_account"},
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
	now := c.now()
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
