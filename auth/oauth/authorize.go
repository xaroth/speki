package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/xaroth/speki/auth/seal"
	"github.com/xaroth/speki/gh"
	"github.com/xaroth/speki/telemetry"
)

//go:embed pages/*.html pages/fonts/*.woff2
var pages embed.FS

var templates = template.Must(template.ParseFS(pages, "pages/*.html"))

const (
	consentCookie = "__Host-speki_consent"
	txCookie      = "__Host-speki_tx_"

	// Limits that keep the transaction cookie under the 4 KB browsers allow.
	maxState    = 512
	maxClientID = 1024

	maxForm = 64 << 10
)

// authRequest is a validated authorization request, carried through consent and GitHub.
type authRequest struct {
	ClientID      string `json:"client_id"`
	ClientName    string `json:"client_name"`
	ClientHost    string `json:"client_host,omitempty"`
	RedirectURI   string `json:"redirect_uri"`
	State         string `json:"state,omitempty"`
	CodeChallenge string `json:"code_challenge"`
	AllowWrite    bool   `json:"allow_write"`    // the client asked for speki:write, or for no speki scope in particular
	RedirectGiven bool   `json:"redirect_given"` // the request named its redirect_uri, so the token request must too
}

type consentValue struct {
	Request    authRequest `json:"request"`
	CookieHash string      `json:"cookie"`
}

type transaction struct {
	Request  authRequest `json:"request"`
	Write    bool        `json:"write"`
	Verifier string      `json:"verifier"` // PKCE verifier for GitHub
	Nonce    string      `json:"nonce"`    // the state sent to GitHub, which names the cookie too
}

// authorize validates an authorization request and shows the consent page. A request whose client or
// redirect URI can't be trusted gets an error page, never a redirect: speki mustn't be an open
// redirector.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if len(q.Get("client_id")) > maxClientID || len(q.Get("state")) > maxState {
		s.errorPage(w, r, http.StatusBadRequest, "This sign-in link is broken.", "The request is too long.")
		return
	}
	c, err := s.clients.resolve(r.Context(), q.Get("client_id"))
	if err != nil {
		s.logger.InfoContext(r.Context(), "unknown client", "err", err)
		s.errorPage(w, r, http.StatusBadRequest, "This program isn't known to speki.",
			"It isn't registered, or its description couldn't be checked.")
		return
	}
	redirectURI := q.Get("redirect_uri")
	redirectGiven := redirectURI != ""
	if redirectURI == "" && len(c.RedirectURIs) == 1 {
		redirectURI = c.RedirectURIs[0]
	}
	if !c.redirectAllowed(redirectURI) {
		s.errorPage(w, r, http.StatusBadRequest, "This sign-in link is broken.", "The redirect address doesn't match the program's registration.")
		return
	}

	req := authRequest{
		ClientID:      c.ID,
		ClientName:    c.Name,
		ClientHost:    c.Host,
		RedirectURI:   redirectURI,
		State:         q.Get("state"),
		CodeChallenge: q.Get("code_challenge"),
		AllowWrite:    allowsWrite(q.Get("scope")),
		RedirectGiven: redirectGiven,
	}
	switch {
	case q.Get("response_type") != "code":
		s.redirectError(w, r, req, "unsupported_response_type", "only the code flow is supported")
		return
	case !validChallenge(req.CodeChallenge) || q.Get("code_challenge_method") != "S256":
		s.redirectError(w, r, req, "invalid_request", "PKCE with S256 is required")
		return
	case q.Get("resource") != "" && q.Get("resource") != s.resource():
		s.redirectError(w, r, req, "invalid_target", "tokens are only issued for "+s.resource())
		return
	}

	cookie := randomToken()
	consent, err := s.keys.Seal(seal.PurposeConsent, consentValue{Request: req, CookieHash: hash(cookie)}, s.now().Add(consentLifetime))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	setCookie(w, consentCookie, cookie, consentLifetime)
	s.page(w, r, http.StatusOK, "consent.html", map[string]any{
		"Name":       s.title(),
		"Step":       1,
		"Client":     c.Name,
		"Host":       c.Host,
		"Redirect":   redirectHost(redirectURI),
		"Local":      c.Host == "",
		"Consent":    consent,
		"AllowWrite": req.AllowWrite,
	})
}

// allowsWrite reads the scope parameter: whether write access may be offered. Scopes speki doesn't know
// (offline_access, openid and the like) are ignored, as RFC 6749 allows; the token response says what
// was granted. A request naming no speki scope lets the user choose.
func allowsWrite(scope string) bool {
	fields := strings.Fields(scope)
	return slices.Contains(fields, ScopeWrite) || !slices.Contains(fields, ScopeRead)
}

// validChallenge reports whether c can be an S256 PKCE challenge: 43 base64url characters (RFC 7636).
func validChallenge(c string) bool {
	if len(c) != 43 {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(c)
	return err == nil
}

// consent checks the consent form came from the browser that got the page, via the cookie hash, then
// sends the user to GitHub.
func (s *Server) consent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	err := r.ParseForm()
	if err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "This sign-in link is broken.", "The form couldn't be read.")
		return
	}
	var cv consentValue
	err = s.keys.Open(seal.PurposeConsent, r.PostForm.Get("consent"), &cv)
	cookie, cerr := r.Cookie(consentCookie)
	if err != nil || cerr != nil || subtle.ConstantTimeCompare([]byte(hash(cookie.Value)), []byte(cv.CookieHash)) != 1 {
		s.errorPage(w, r, http.StatusBadRequest, "This sign-in expired.", "Start signing in again from your program.")
		return
	}
	clearCookie(w, consentCookie)
	if r.PostForm.Get("decision") != "allow" {
		signIns.WithLabelValues("declined").Inc()
		s.redirectError(w, r, cv.Request, "access_denied", "the user declined")
		return
	}

	nonce := randomToken()
	verifier := randomToken()
	write := r.PostForm.Get("access") == "write" && cv.Request.AllowWrite
	tx := transaction{
		Request:  cv.Request,
		Write:    write,
		Verifier: verifier,
		Nonce:    nonce,
	}
	sealed, err := s.keys.Seal(seal.PurposeTransaction, tx, s.now().Add(consentLifetime))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	setCookie(w, txCookie+nonce, sealed, consentLifetime)
	http.Redirect(w, r, s.cfg.GitHub.AuthorizeURL(s.callbackURL(), nonce, challenge(verifier)), http.StatusSeeOther)
}

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

// redirectError sends an OAuth error back to the client, after its redirect URI has been checked.
func (s *Server) redirectError(w http.ResponseWriter, r *http.Request, req authRequest, code, description string) {
	redirectTo(w, r, req.RedirectURI, url.Values{
		"error":             {code},
		"error_description": {description},
		"state":             {req.State},
		"iss":               {s.cfg.Issuer},
	})
}

func redirectTo(w http.ResponseWriter, r *http.Request, base string, params url.Values) {
	u, err := url.Parse(base)
	if err != nil {
		http.Error(w, "bad redirect", http.StatusBadRequest)
		return
	}
	q := u.Query()
	for k, vs := range params {
		if len(vs) > 0 && vs[0] != "" {
			q.Set(k, vs[0])
		}
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// pageCSP keeps the pages from loading anything but speki's fonts, or being framed. It has no
// form-action: browsers apply it to the redirects after the consent form (to GitHub, or back to the
// client on Cancel), and the client's address isn't known in advance.
const pageCSP = "default-src 'none'; style-src 'unsafe-inline'; font-src 'self'; frame-ancestors 'none'"

func (s *Server) page(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", pageCSP)
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	err := templates.ExecuteTemplate(w, name, data)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "rendering page", "page", name, "err", err)
	}
}

func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, status int, title, detail string) {
	s.errorPageAs(w, r, status, title, detail, "")
}

// errorPageAs is errorPage after GitHub sign-in: it says which GitHub account was used, so a wrong one
// is obvious.
func (s *Server) errorPageAs(w http.ResponseWriter, r *http.Request, status int, title, detail, login string) {
	step := 1 // choosing access
	if r.URL.Path == callbackPath {
		step = 2 // signing in with GitHub
	}
	s.page(w, r, status, "error.html", map[string]any{
		"Name":   s.title(),
		"Step":   step,
		"Failed": true,
		"Title":  title,
		"Detail": detail,
		"Login":  login,
	})
}

// font serves a typeface the pages use. The fonts are in the binary, so they only change with it.
func font(w http.ResponseWriter, r *http.Request) {
	data, err := pages.ReadFile("pages/fonts/" + r.PathValue("name"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "font/woff2")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}

func (s *Server) title() string {
	if s.cfg.Title == "" {
		return "speki"
	}
	return s.cfg.Title
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.ErrorContext(r.Context(), "sign-in failed", "err", err)
	s.errorPage(w, r, http.StatusInternalServerError, "Something went wrong.", "Try again in a moment.")
}

func setCookie(w http.ResponseWriter, name, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   int(maxAge.Seconds()),
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func redirectHost(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return u
	}
	return p.Host
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return base64.RawURLEncoding.EncodeToString(b)
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// challenge returns the S256 PKCE challenge for verifier.
func challenge(verifier string) string { return hash(verifier) }
