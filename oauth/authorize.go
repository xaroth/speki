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
	"time"

	"github.com/eve-online-tools/speki/gh"
	"github.com/eve-online-tools/speki/seal"
)

//go:embed pages/*.html
var pages embed.FS

// templates holds the consent and error pages. They're parsed once; html/template is safe for concurrent
// use.
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
}

type consentValue struct {
	Request    authRequest `json:"request"`
	CookieHash string      `json:"cookie"`
}

type transaction struct {
	Request  authRequest `json:"request"`
	Write    bool        `json:"write"`
	Verifier string      `json:"verifier"` // PKCE verifier for GitHub
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
	}
	switch {
	case q.Get("response_type") != "code":
		s.redirectError(w, r, req, "unsupported_response_type", "only the code flow is supported")
		return
	case req.CodeChallenge == "" || q.Get("code_challenge_method") != "S256":
		s.redirectError(w, r, req, "invalid_request", "PKCE with S256 is required")
		return
	case q.Get("resource") != "" && q.Get("resource") != s.Resource():
		s.redirectError(w, r, req, "invalid_target", "tokens are only issued for "+s.Resource())
		return
	}

	cookie := randomToken()
	consent, err := s.keys.Seal(seal.Consent, consentValue{Request: req, CookieHash: hash(cookie)}, s.now().Add(consentLifetime))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	setCookie(w, consentCookie, cookie, consentLifetime)
	s.page(w, r, http.StatusOK, "consent.html", map[string]any{
		"Client":   c.Name,
		"Host":     c.Host,
		"Redirect": redirectHost(redirectURI),
		"Local":    c.Host == "",
		"Consent":  consent,
	})
}

// consent handles the consent form: it checks the form belongs to this browser, then sends the user to
// GitHub with a fresh transaction.
func (s *Server) consent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	err := r.ParseForm()
	if err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "This sign-in link is broken.", "The form couldn't be read.")
		return
	}
	var cv consentValue
	err = s.keys.Open(seal.Consent, r.PostForm.Get("consent"), &cv)
	cookie, cerr := r.Cookie(consentCookie)
	if err != nil || cerr != nil || subtle.ConstantTimeCompare([]byte(hash(cookie.Value)), []byte(cv.CookieHash)) != 1 {
		s.errorPage(w, r, http.StatusBadRequest, "This sign-in expired.", "Start signing in again from your program.")
		return
	}
	clearCookie(w, consentCookie)
	if r.PostForm.Get("decision") != "allow" {
		s.signIns.WithLabelValues("declined").Inc()
		s.redirectError(w, r, cv.Request, "access_denied", "the user declined")
		return
	}

	nonce := randomToken()
	verifier := randomToken()
	tx := transaction{Request: cv.Request, Write: r.PostForm.Get("access") == "write", Verifier: verifier}
	sealed, err := s.keys.Seal(seal.Transaction, tx, s.now().Add(consentLifetime))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	setCookie(w, txCookie+nonce, sealed, consentLifetime)
	http.Redirect(w, r, s.cfg.GitHub.AuthorizeURL(s.callbackURL(), nonce, challenge(verifier)), http.StatusSeeOther)
}

// codeValue is what an authorization code holds.
type codeValue struct {
	ID            string        `json:"id"`
	User          gh.User       `json:"user"`
	Tokens        gh.UserTokens `json:"tokens"`
	ClientID      string        `json:"client_id"`
	ClientName    string        `json:"client_name"`
	RedirectURI   string        `json:"redirect_uri"`
	CodeChallenge string        `json:"code_challenge"`
	Scopes        []string      `json:"scopes"`
	SignedIn      time.Time     `json:"signed_in"`
}

// callback is where GitHub sends the user back. It exchanges GitHub's code, checks the user may get in,
// and sends the user back to the client with speki's own code.
func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	nonce := q.Get("state")
	cookie, err := r.Cookie(txCookie + nonce)
	var tx transaction
	if err == nil {
		err = s.keys.Open(seal.Transaction, cookie.Value, &tx)
	}
	if nonce == "" || err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "This sign-in expired.", "Start signing in again from your program.")
		return
	}
	clearCookie(w, txCookie+nonce)
	if q.Get("error") != "" {
		s.signIns.WithLabelValues("declined").Inc()
		s.redirectError(w, r, tx.Request, "access_denied", "GitHub authorization was declined")
		return
	}

	tokens, err := s.github.Exchange(r.Context(), s.cfg.GitHub, q.Get("code"), tx.Verifier, s.callbackURL())
	if errors.Is(err, gh.ErrAppConfig) {
		s.logger.ErrorContext(r.Context(), "GitHub code exchange failed", "err", err)
		s.signIns.WithLabelValues("failed").Inc()
		s.errorPage(w, r, http.StatusBadGateway, "Speki can't sign you in right now.", "Its GitHub app is misconfigured. Tell the person who runs speki.")
		return
	}
	if err != nil {
		s.logger.WarnContext(r.Context(), "GitHub code exchange failed", "err", err)
		s.signIns.WithLabelValues("failed").Inc()
		s.errorPage(w, r, http.StatusBadGateway, "GitHub didn't accept the sign-in.", "Try again from your program.")
		return
	}
	user, err := s.github.Me(r.Context(), tokens.AccessToken)
	if err != nil {
		s.logger.WarnContext(r.Context(), "GitHub user lookup failed", "err", err)
		s.signIns.WithLabelValues("failed").Inc()
		s.errorPage(w, r, http.StatusBadGateway, "GitHub didn't answer.", "Try again in a moment.")
		return
	}
	if s.cfg.CheckUser != nil {
		err = s.cfg.CheckUser(r.Context(), tokens.AccessToken)
		if err != nil {
			s.logger.InfoContext(r.Context(), "sign-in refused", "login", user.Login, "reason", err)
			s.signIns.WithLabelValues("refused").Inc()
			s.errorPage(w, r, http.StatusForbidden, "You can't use this speki.", err.Error())
			return
		}
	}

	scopes := []string{ScopeRead}
	if tx.Write {
		scopes = append(scopes, ScopeWrite)
	}
	code, err := s.keys.Seal(seal.Code, codeValue{
		ID:            randomToken(),
		User:          user,
		Tokens:        tokens,
		ClientID:      tx.Request.ClientID,
		ClientName:    tx.Request.ClientName,
		RedirectURI:   tx.Request.RedirectURI,
		CodeChallenge: tx.Request.CodeChallenge,
		Scopes:        scopes,
		SignedIn:      s.now(),
	}, s.now().Add(codeLifetime))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.signIns.WithLabelValues("ok").Inc()
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

// pageCSP keeps the pages from loading anything or being framed. It has no form-action: browsers apply
// it to the redirects after the consent form (to GitHub, or back to the client on Cancel), and the
// client's address isn't known in advance.
const pageCSP = "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'"

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
	s.page(w, r, status, "error.html", map[string]string{"Title": title, "Detail": detail})
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
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
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
