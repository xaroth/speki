package oauth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/eve-online-tools/speki/gh"
)

const (
	refreshLockTime      = 25 * time.Second
	githubRefreshTimeout = 20 * time.Second // shorter than the lock, leaving time to keep the result
	refreshResultTime    = time.Minute
	unknownOutcomeTime   = 10 * time.Minute // how long a GitHub refresh without an answer is remembered
)

// refreshError is a refresh that failed with an answer for the client.
type refreshError struct {
	status      int
	code        string
	description string
	retryAfter  string // seconds; empty for none
}

func (e *refreshError) Error() string { return e.code + ": " + e.description }

// errStoppedWaiting means the client went away while another request held the lock. It gets no answer.
var errStoppedWaiting = errors.New("stopped waiting for another refresh")

// refresh trades the GitHub refresh token in rv for a new pair, or picks up the result of a concurrent
// refresh of the same speki token, old.
func (s *Server) refresh(ctx context.Context, rv refreshValue, scopes []string, old string) (tokenResponse, error) {
	key := hash(old)
	deadline := time.Now().Add(refreshLockTime)
	for {
		resp, found, err := s.refreshResult(ctx, key, old)
		if err != nil {
			return tokenResponse{}, lostError(err)
		}
		if found {
			return storedResult(resp, scopes)
		}
		owner := []byte(randomToken())
		lockedAt := time.Now() // before asking: the lock's time starts when the store sets it
		locked, err := s.store.SetNX(ctx, "refresh-lock:"+key, owner, refreshLockTime)
		if err != nil {
			return tokenResponse{}, err
		}
		if locked {
			return s.refreshLocked(ctx, refreshLock{key: key, owner: owner, at: lockedAt}, rv, scopes, old)
		}
		if time.Now().After(deadline) {
			return tokenResponse{}, &refreshError{
				status:      http.StatusServiceUnavailable,
				code:        "temporarily_unavailable",
				description: "another refresh is in progress; try again",
				retryAfter:  "1",
			}
		}
		select { // someone else is refreshing: wait for their result, or for the lock
		case <-ctx.Done():
			return tokenResponse{}, errStoppedWaiting
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
func (s *Server) refreshLocked(ctx context.Context, lock refreshLock, rv refreshValue, scopes []string, old string) (tokenResponse, error) {
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
		return tokenResponse{}, lostError(err)
	}
	if found {
		return storedResult(resp, scopes)
	}

	// Once GitHub is asked, the old refresh token may be spent, so the refresh and keeping its result
	// outlive a client disconnect: the retry then finds the result instead of a dead token. The GitHub
	// call must end before the lock does, or a second refresh could start alongside it; the deadline
	// counts from lock.at so a slow store before this point can't stretch it.
	gctx, cancel := context.WithDeadline(context.WithoutCancel(ctx), lock.at.Add(githubRefreshTimeout))
	defer cancel()
	tokens, err := s.github.Refresh(gctx, s.cfg.GitHub, rv.Refresh)
	if errors.Is(err, gh.ErrBadGrant) {
		s.signedOut(ctx, key, rv.Login)
		return tokenResponse{}, &refreshError{
			status:      http.StatusBadRequest,
			code:        "invalid_grant",
			description: "sign in again",
		}
	}
	if errors.Is(err, gh.ErrAppConfig) {
		s.logger.ErrorContext(ctx, "GitHub refresh failed", "err", err)
		refreshes.WithLabelValues("app_config").Inc()
		return tokenResponse{}, &refreshError{
			status:      http.StatusServiceUnavailable,
			code:        "temporarily_unavailable",
			description: "speki's GitHub app is misconfigured; tell the operator",
		}
	}
	if err != nil {
		s.logger.WarnContext(ctx, "GitHub refresh failed", "err", err)
		if errors.Is(err, gh.ErrUnreached) {
			refreshes.WithLabelValues("unreached").Inc()
		} else {
			// GitHub may have spent the refresh token without speki hearing back. The retry will then
			// be refused; the marker lets it tell that apart from an ordinary sign-out.
			refreshes.WithLabelValues("no_answer").Inc()
			mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			defer cancel()
			_ = s.store.Set(mctx, unknownOutcomeKey(key), []byte(unknownOutcome), unknownOutcomeTime)
		}
		return tokenResponse{}, &refreshError{
			status:      http.StatusServiceUnavailable,
			code:        "temporarily_unavailable",
			description: "GitHub didn't answer; try again",
			retryAfter:  "5",
		}
	}
	user := gh.User{ID: rv.UserID, Login: rv.Login}
	resp, err = s.issue(user, tokens, scopes, rv.ClientID, rv.Client, rv.SignedIn)
	if err != nil {
		return tokenResponse{}, err
	}
	refreshes.WithLabelValues("ok").Inc()
	// Keep the result before the lock runs out, or a waiter refreshes again with the spent GitHub token.
	// A late GitHub answer still gets a moment to be kept.
	kctx, cancel := context.WithDeadline(context.WithoutCancel(ctx), maxTime(lock.at.Add(refreshLockTime), time.Now().Add(2*time.Second)))
	defer cancel()
	err = s.keepRefreshResult(kctx, key, old, resp)
	if err != nil {
		err = s.keepRefreshResult(kctx, key, old, resp) // once more
	}
	if err != nil {
		// A marker tells requests with the same token not to try GitHub with a spent token. If even that
		// can't be written the store is down, and the lock is left to run out.
		s.logger.WarnContext(ctx, "keeping refresh result failed", "err", err)
		mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		if s.store.Set(mctx, "refresh-result:"+key, []byte(resultLost), refreshResultTime) != nil {
			keepLock = true
		}
	}
	// GitHub hadn't spent the token when an earlier refresh got no answer: a later refusal of it is an
	// ordinary sign-out.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	_, _, _ = s.store.GetDel(dctx, unknownOutcomeKey(key))
	return resp, nil
}

// lostError turns a lost result into its answer. A lost result means another request refreshed this
// token, and may have handed the new pair to the same client. So the client is told to try again, by
// which time it may hold that pair, rather than to sign in again; once the marker expires, GitHub's
// refusal of the spent token signs it out.
func lostError(err error) error {
	if errors.Is(err, errResultLost) {
		return &refreshError{
			status:      http.StatusServiceUnavailable,
			code:        "temporarily_unavailable",
			description: "this refresh token was just used, but its result was lost",
			retryAfter:  "5",
		}
	}
	return err
}

// storedResult returns the result of a refresh another request made with the same token, if it was for
// the same scopes: a stored result never widens what this request asked for.
func storedResult(resp tokenResponse, scopes []string) (tokenResponse, error) {
	if !sameScopes(strings.Fields(resp.Scope), scopes) {
		return tokenResponse{}, &refreshError{
			status:      http.StatusBadRequest,
			code:        "invalid_scope",
			description: "this refresh token was just refreshed with other scopes",
		}
	}
	refreshes.WithLabelValues("shared").Inc()
	return resp, nil
}

// unknownOutcome marks a token whose GitHub refresh ended without an answer.
const unknownOutcome = "unknown"

// signedOut counts a refresh GitHub refused, which signs the user out. When an earlier refresh with the
// same token got no answer from GitHub, GitHub most likely spent the token then: the user is signed out
// by that timeout, not by a revoked or expired authorization.
func (s *Server) signedOut(ctx context.Context, key, login string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	_, found, err := s.store.GetDel(ctx, unknownOutcomeKey(key)) // taken, so each lost token counts once
	if err != nil || !found {
		refreshes.WithLabelValues("signed_out").Inc()
		return
	}
	s.logger.WarnContext(ctx, "refresh lost to GitHub timeout", "login", login)
	refreshes.WithLabelValues("lost_to_timeout").Inc()
}

func unknownOutcomeKey(key string) string { return "refresh-unknown:" + key }

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

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// resultLost is kept in place of a refresh result that couldn't be stored. It can't be mistaken for a
// sealed result, which is longer than a nonce.
const resultLost = "lost"

// errResultLost means the token was refreshed, but the result couldn't be kept for other requests.
var errResultLost = errors.New("refresh result lost")

func (s *Server) refreshResult(ctx context.Context, key, old string) (tokenResponse, bool, error) {
	raw, found, err := s.store.Get(ctx, "refresh-result:"+key)
	if err != nil || !found {
		return tokenResponse{}, false, err
	}
	if string(raw) == resultLost {
		return tokenResponse{}, false, errResultLost
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
