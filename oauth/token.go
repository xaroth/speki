package oauth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/eve-online-tools/speki/gh"
	"github.com/eve-online-tools/speki/seal"
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

const (
	refreshLockTime      = 10 * time.Second
	githubRefreshTimeout = 8 * time.Second // shorter than the lock
	refreshResultTime    = 30 * time.Second
)

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
	err := s.keys.Open(seal.Code, f.Get("code"), &c)
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
	case c.Resource != s.Resource():
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the code was issued for another server")
		return
	case subtle.ConstantTimeCompare([]byte(challenge(f.Get("code_verifier"))), []byte(c.CodeChallenge)) != 1:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the PKCE verifier doesn't match")
		return
	case f.Get("resource") != "" && f.Get("resource") != s.Resource():
		oauthError(w, http.StatusBadRequest, "invalid_target", "tokens are only issued for "+s.Resource())
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
// stolen speki refresh token dies when the real client uses it. Two refreshes of one token at once (two
// windows of one client, or a retry after a lost response) must both succeed: one refreshes under a lock,
// the result is kept for a short while, encrypted with a key only the old refresh token gives, and the
// other picks it up. Only GitHub refusing the refresh token is an invalid_grant, which signs the user
// out; anything else is a temporary error.
func (s *Server) refreshGrant(w http.ResponseWriter, r *http.Request) {
	f := r.PostForm
	old := f.Get("refresh_token")
	var rv refreshValue
	err := s.keys.Open(seal.Refresh, old, &rv)
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid or expired")
		return
	}
	switch {
	case rv.Audience != s.Resource():
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

	ctx := r.Context()
	key := hash(old)
	deadline := time.Now().Add(refreshLockTime)
	for {
		resp, found, err := s.refreshResult(ctx, key, old)
		if err != nil {
			s.tokenServerError(w, r, err)
			return
		}
		if found {
			s.writeRefreshResult(w, resp, scopes)
			return
		}
		owner := []byte(randomToken())
		lockedAt := time.Now() // before asking: the lock's time starts when the store sets it
		locked, err := s.store.SetNX(ctx, "refresh-lock:"+key, owner, refreshLockTime)
		if err != nil {
			s.tokenServerError(w, r, err)
			return
		}
		if locked {
			s.refreshLocked(w, r, refreshLock{key: key, owner: owner, at: lockedAt}, rv, scopes, old)
			return
		}
		if time.Now().After(deadline) {
			w.Header().Set("Retry-After", "1")
			oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "another refresh is in progress; try again")
			return
		}
		select { // someone else is refreshing: wait for their result, or for the lock
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// refreshLock is a held lock on refreshing one token.
type refreshLock struct {
	key   string
	owner []byte
	at    time.Time // when it was taken, at the latest
}

// refreshLocked does the refresh, holding the lock for the old token.
func (s *Server) refreshLocked(w http.ResponseWriter, r *http.Request, lock refreshLock, rv refreshValue, scopes []string, old string) {
	ctx := r.Context()
	key := lock.key
	keepLock := false // set when the result couldn't be kept: the lock then runs out instead
	defer func() {
		if !keepLock {
			_ = s.store.Release(context.WithoutCancel(ctx), "refresh-lock:"+key, lock.owner)
		}
	}()

	// Another request may have finished between our look and our lock.
	resp, found, err := s.refreshResult(ctx, key, old)
	if err != nil {
		s.tokenServerError(w, r, err)
		return
	}
	if found {
		s.writeRefreshResult(w, resp, scopes)
		return
	}

	// Once GitHub is asked, its old refresh token may be spent. So the refresh and keeping its result
	// go on when the client disconnects: its retry then finds the result instead of a dead token. The
	// GitHub call must end before the lock does, or a second refresh could start alongside it.
	// The deadline counts from when the lock was taken, not from now: a slow store before this point
	// mustn't let the GitHub call outlive the lock.
	gctx, cancel := context.WithDeadline(context.WithoutCancel(ctx), lock.at.Add(githubRefreshTimeout))
	defer cancel()
	tokens, err := s.github.Refresh(gctx, s.cfg.GitHub, rv.Refresh)
	if errors.Is(err, gh.ErrBadGrant) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "sign in again")
		return
	}
	if errors.Is(err, gh.ErrAppConfig) {
		s.logger.ErrorContext(ctx, "GitHub refresh failed", "err", err)
		oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "speki's GitHub app is misconfigured; tell the operator")
		return
	}
	if err != nil {
		s.logger.WarnContext(ctx, "GitHub refresh failed", "err", err)
		w.Header().Set("Retry-After", "5")
		oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "GitHub didn't answer; try again")
		return
	}
	user := gh.User{ID: rv.UserID, Login: rv.Login}
	resp, err = s.issue(user, tokens, scopes, rv.ClientID, rv.Client, rv.SignedIn)
	if err != nil {
		s.tokenServerError(w, r, err)
		return
	}
	err = s.keepRefreshResult(context.WithoutCancel(ctx), key, old, resp)
	if err != nil {
		err = s.keepRefreshResult(context.WithoutCancel(ctx), key, old, resp) // once more
	}
	if err != nil {
		// Another request with the same token would refresh again with a GitHub token that's spent.
		// Keeping the lock until it runs out makes it wait and fail as temporary instead.
		keepLock = true
		s.logger.WarnContext(ctx, "keeping refresh result failed", "err", err)
	}
	writeJSON(w, http.StatusOK, resp)
}

// writeRefreshResult answers with the result of a refresh another request made with the same token, if
// it was for the same scopes: a stored result never widens what this request asked for.
func (s *Server) writeRefreshResult(w http.ResponseWriter, resp tokenResponse, scopes []string) {
	if !sameScopes(strings.Fields(resp.Scope), scopes) {
		oauthError(w, http.StatusBadRequest, "invalid_scope", "this refresh token was just refreshed with other scopes")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// sameScopes reports whether a and b hold the same scopes, in any order.
func sameScopes(a, b []string) bool {
	for _, s := range a {
		if !slices.Contains(b, s) {
			return false
		}
	}
	for _, s := range b {
		if !slices.Contains(a, s) {
			return false
		}
	}
	return true
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
	access, err := s.keys.Seal(seal.Access, accessValue{
		UserID:   user.ID,
		Login:    user.Login,
		Token:    tokens.AccessToken,
		Scopes:   scopes,
		Client:   client,
		Audience: s.Resource(),
	}, accessExpires)
	if err != nil {
		return tokenResponse{}, err
	}
	refresh, err := s.keys.Seal(seal.Refresh, refreshValue{
		UserID:   user.ID,
		Login:    user.Login,
		Refresh:  tokens.RefreshToken,
		Scopes:   scopes,
		ClientID: clientID,
		Client:   client,
		Audience: s.Resource(),
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

// narrow returns the scopes asked for, which must be a subset of granted and include speki:read. An empty
// request keeps all.
func narrow(granted []string, requested string) ([]string, bool) {
	if strings.TrimSpace(requested) == "" {
		return granted, true
	}
	out := []string{ScopeRead}
	for _, s := range strings.Fields(requested) {
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

func (s *Server) refreshResult(ctx context.Context, key, old string) (tokenResponse, bool, error) {
	raw, found, err := s.store.Get(ctx, "refresh-result:"+key)
	if err != nil || !found {
		return tokenResponse{}, false, err
	}
	plain, err := resultCipher(old, raw, false)
	if err != nil {
		return tokenResponse{}, false, err
	}
	var resp tokenResponse
	err = json.Unmarshal(plain, &resp)
	return resp, err == nil, err
}

func (s *Server) keepRefreshResult(ctx context.Context, key, old string, resp tokenResponse) error {
	plain, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	sealed, err := resultCipher(old, plain, true)
	if err != nil {
		return err
	}
	return s.store.Set(ctx, "refresh-result:"+key, sealed, refreshResultTime)
}

// resultCipher encrypts or decrypts a refresh result with a key derived from the old refresh token, so the
// store never holds a usable token.
func resultCipher(oldToken string, data []byte, encrypt bool) ([]byte, error) {
	key, err := hkdf.Key(sha256.New, []byte(oldToken), nil, "speki refresh result", 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if encrypt {
		nonce := make([]byte, aead.NonceSize())
		_, _ = rand.Read(nonce)
		return aead.Seal(nonce, nonce, data, nil), nil
	}
	if len(data) < aead.NonceSize() {
		return nil, errors.New("short refresh result")
	}
	return aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], nil)
}

func (s *Server) tokenServerError(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.ErrorContext(r.Context(), "token request failed", "err", err)
	oauthError(w, http.StatusInternalServerError, "server_error", "try again")
}
