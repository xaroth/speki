package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/xaroth/speki/auth/seal"
)

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
