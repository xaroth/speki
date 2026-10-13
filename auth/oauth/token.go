package oauth

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/xaroth/speki/auth/seal"
	"github.com/xaroth/speki/gh"
)

// accessValue is what a speki access token holds.
type accessValue struct {
	UserID   int64    `json:"uid"`
	Login    string   `json:"login"`
	Token    string   `json:"token"` // GitHub access token
	Scopes   []string `json:"scopes"`
	Client   string   `json:"client"`
	Audience string   `json:"aud"`
}

// refreshValue is what a speki refresh token holds.
type refreshValue struct {
	UserID   int64     `json:"uid"`
	Login    string    `json:"login"`
	Refresh  string    `json:"refresh"` // GitHub refresh token
	Scopes   []string  `json:"scopes"`
	ClientID string    `json:"client_id"`
	Client   string    `json:"client"`
	Audience string    `json:"aud"`
	SignedIn time.Time `json:"signed_in"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	err := r.ParseForm()
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the form couldn't be read")
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.codeGrant(w, r)
	case "refresh_token":
		s.refreshGrant(w, r)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
	}
}

func (s *Server) codeGrant(w http.ResponseWriter, r *http.Request) {
	f := r.PostForm
	var c codeValue
	err := s.keys.Open(seal.PurposeCode, f.Get("code"), &c)
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the code is invalid or expired")
		return
	}
	switch {
	case f.Get("client_id") != c.ClientID:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the code was issued to another client")
		return
	case (c.RedirectGiven || f.Get("redirect_uri") != "") && f.Get("redirect_uri") != c.RedirectURI:
		// Required when authorize was given one (RFC 6749 4.1.3), and must match whenever it's sent.
		oauthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri is missing or doesn't match")
		return
	case c.Resource != s.resource():
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the code was issued for another server")
		return
	case subtle.ConstantTimeCompare([]byte(challenge(f.Get("code_verifier"))), []byte(c.CodeChallenge)) != 1:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the PKCE verifier doesn't match")
		return
	case f.Get("resource") != "" && f.Get("resource") != s.resource():
		oauthError(w, http.StatusBadRequest, "invalid_target", "tokens are only issued for "+s.resource())
		return
	}
	fresh, err := s.store.SetNX(r.Context(), "code:"+c.ID, []byte{1}, usedCodeMemory)
	if err != nil {
		s.tokenServerError(w, r, err)
		return
	}
	if !fresh {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the code was already used")
		return
	}
	resp, err := s.issue(c.User, c.Tokens, c.Scopes, c.ClientID, c.ClientName, c.SignedIn)
	if err != nil {
		s.tokenServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// refreshGrant refreshes with GitHub and hands out a new pair. GitHub's refresh tokens work once, so a
// stolen speki refresh token dies when the real client uses it. Concurrent refreshes of one token (two
// windows, or a retry after a lost response) must both succeed: one refreshes under a lock and keeps the
// result briefly, encrypted with a key only the old refresh token gives; the other picks it up. Only
// GitHub refusing the refresh token is invalid_grant, which signs the user out; anything else is
// temporary.
func (s *Server) refreshGrant(w http.ResponseWriter, r *http.Request) {
	f := r.PostForm
	old := f.Get("refresh_token")
	var rv refreshValue
	err := s.keys.Open(seal.PurposeRefresh, old, &rv)
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid or expired")
		return
	}
	switch {
	case rv.Audience != s.resource():
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token was issued for another server")
		return
	case f.Get("client_id") != rv.ClientID:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token was issued to another client")
		return
	}
	scopes, ok := narrow(rv.Scopes, f.Get("scope"))
	if !ok {
		oauthError(w, http.StatusBadRequest, "invalid_scope", "a refresh can't add scopes, or drop speki:read")
		return
	}

	resp, err := s.refresh(r.Context(), rv, scopes, old)
	var rerr *refreshError
	switch {
	case errors.Is(err, errStoppedWaiting):
	case errors.As(err, &rerr):
		if rerr.retryAfter != "" {
			w.Header().Set("Retry-After", rerr.retryAfter)
		}
		oauthError(w, rerr.status, rerr.code, rerr.description)
	case err != nil:
		s.tokenServerError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, resp)
	}
}

// issue seals a new access and refresh token pair.
func (s *Server) issue(
	user gh.User,
	tokens gh.UserTokens,
	scopes []string,
	clientID, client string,
	signedIn time.Time,
) (tokenResponse, error) {
	now := s.now()
	accessExpires := minTime(now.Add(accessLifetime), tokens.AccessExpires)
	access, err := s.keys.Seal(seal.PurposeAccess, accessValue{
		UserID:   user.ID,
		Login:    user.Login,
		Token:    tokens.AccessToken,
		Scopes:   scopes,
		Client:   client,
		Audience: s.resource(),
	}, accessExpires)
	if err != nil {
		return tokenResponse{}, err
	}
	refresh, err := s.keys.Seal(seal.PurposeRefresh, refreshValue{
		UserID:   user.ID,
		Login:    user.Login,
		Refresh:  tokens.RefreshToken,
		Scopes:   scopes,
		ClientID: clientID,
		Client:   client,
		Audience: s.resource(),
		SignedIn: signedIn,
	}, minTime(signedIn.Add(signInLifetime), tokens.RefreshExpires))
	if err != nil {
		return tokenResponse{}, err
	}
	return tokenResponse{
		AccessToken:  access,
		TokenType:    "Bearer",
		ExpiresIn:    int64(accessExpires.Sub(now).Seconds()),
		RefreshToken: refresh,
		Scope:        strings.Join(scopes, " "),
	}, nil
}

// narrow returns the scopes asked for, which must be a subset of granted; speki:read is always kept.
// Scopes speki doesn't know are ignored, as at authorize. A request naming none of speki's keeps all.
func narrow(granted []string, requested string) ([]string, bool) {
	var asked []string
	for s := range strings.FieldsSeq(requested) {
		if s == ScopeRead || s == ScopeWrite {
			asked = append(asked, s)
		}
	}
	if len(asked) == 0 {
		return granted, true
	}
	out := []string{ScopeRead}
	for _, s := range asked {
		if !slices.Contains(granted, s) {
			return nil, false
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out, true
}

func minTime(a, b time.Time) time.Time {
	if b.IsZero() || a.Before(b) {
		return a
	}
	return b
}

func (s *Server) tokenServerError(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.ErrorContext(r.Context(), "token request failed", "err", err)
	oauthError(w, http.StatusInternalServerError, "server_error", "try again")
}
