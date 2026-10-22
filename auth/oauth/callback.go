package oauth

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/xaroth/speki/auth/seal"
	"github.com/xaroth/speki/gh"
	"github.com/xaroth/speki/telemetry"
)

type codeValue struct {
	ID            string        `json:"id"`
	User          gh.User       `json:"user"`
	Tokens        gh.UserTokens `json:"tokens"`
	ClientID      string        `json:"client_id"`
	ClientName    string        `json:"client_name"`
	RedirectURI   string        `json:"redirect_uri"`
	CodeChallenge string        `json:"code_challenge"`
	RedirectGiven bool          `json:"redirect_given"`
	Scopes        []string      `json:"scopes"`
	Resource      string        `json:"resource"`
	SignedIn      time.Time     `json:"signed_in"`
}

// Refusal is what Config.CheckUser returns to turn a user away. Its reason is shown to them; any other
// error means the check itself failed.
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return r.Reason }

// callback is where GitHub sends the user back. It exchanges GitHub's code, checks the user may get in,
// and sends the user back to the client with speki's own code.
func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	nonce := q.Get("state")
	cookie, err := r.Cookie(txCookie + nonce)
	var tx transaction
	if err == nil {
		err = s.keys.Open(seal.PurposeTransaction, cookie.Value, &tx)
	}
	if nonce == "" || err != nil || subtle.ConstantTimeCompare([]byte(tx.Nonce), []byte(nonce)) != 1 {
		s.errorPage(w, r, http.StatusBadRequest, "This sign-in expired.", "Start signing in again from your program.")
		return
	}
	clearCookie(w, txCookie+nonce)
	if q.Get("error") != "" {
		signIns.WithLabelValues("declined").Inc()
		s.redirectError(w, r, tx.Request, "access_denied", "GitHub authorization was declined")
		return
	}

	tokens, err := s.github.Exchange(r.Context(), s.cfg.GitHub, q.Get("code"), tx.Verifier, s.callbackURL())
	if errors.Is(err, gh.ErrAppConfig) {
		s.logger.ErrorContext(r.Context(), "GitHub code exchange failed", "err", err)
		signIns.WithLabelValues("failed").Inc()
		s.errorPage(w, r, http.StatusBadGateway, "Speki can't sign you in right now.", "Its GitHub app is misconfigured. Tell the person who runs speki.")
		return
	}
	if err != nil {
		s.logger.WarnContext(r.Context(), "GitHub code exchange failed", "err", err)
		signIns.WithLabelValues("failed").Inc()
		s.errorPage(w, r, http.StatusBadGateway, "GitHub didn't accept the sign-in.", "Try again from your program.")
		return
	}
	user, err := s.github.Me(r.Context(), tokens.AccessToken)
	if err != nil {
		s.logger.WarnContext(r.Context(), "GitHub user lookup failed", "err", err)
		signIns.WithLabelValues("failed").Inc()
		s.errorPage(w, r, http.StatusBadGateway, "GitHub didn't answer.", "Try again in a moment.")
		return
	}
	if s.cfg.CheckUser != nil {
		err = s.cfg.CheckUser(r.Context(), tokens.AccessToken)
		refusal, refused := errors.AsType[*Refusal](err)
		switch {
		case refused:
			s.logger.InfoContext(r.Context(), "sign-in refused", "login", user.Login, "reason", refusal.Reason)
			signIns.WithLabelValues("refused").Inc()
			s.errorPageAs(w, r, http.StatusForbidden, "You can't use this speki.", refusal.Reason, user.Login)
			return
		case err != nil: // GitHub's own words never reach the browser
			s.logger.WarnContext(r.Context(), "checking a sign-in with GitHub failed", "login", user.Login, "err", err)
			signIns.WithLabelValues("failed").Inc()
			s.errorPageAs(w, r, http.StatusBadGateway, "Speki couldn't check your access with GitHub.",
				"Try again in a moment. If it keeps failing, give this to whoever runs speki: request "+telemetry.RequestIDFrom(r.Context())+".",
				user.Login)
			return
		}
	}

	scopes := []string{ScopeRead}
	if tx.Write {
		scopes = append(scopes, ScopeWrite)
	}
	code, err := s.keys.Seal(seal.PurposeCode, codeValue{
		ID:            randomToken(),
		User:          user,
		Tokens:        tokens,
		ClientID:      tx.Request.ClientID,
		ClientName:    tx.Request.ClientName,
		RedirectURI:   tx.Request.RedirectURI,
		CodeChallenge: tx.Request.CodeChallenge,
		RedirectGiven: tx.Request.RedirectGiven,
		Scopes:        scopes,
		Resource:      s.resource(),
		SignedIn:      s.now(),
	}, s.now().Add(codeLifetime))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	signIns.WithLabelValues("ok").Inc()
	s.logger.InfoContext(r.Context(), "signed in", "login", user.Login, "client", tx.Request.ClientName, "write", tx.Write)
	redirectTo(w, r, tx.Request.RedirectURI, url.Values{"code": {code}, "state": {tx.Request.State}, "iss": {s.cfg.Issuer}})
}
