package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/eve-online-tools/speki/seal"
)

// client is a registered MCP client.
type client struct {
	ID           string
	Name         string
	Host         string // for clients with a metadata document: the host it comes from. Empty for local ones
	RedirectURIs []string
}

// registration is what a dynamically registered client's ID holds.
type registration struct {
	Name         string   `json:"name"`
	RedirectURIs []string `json:"redirect_uris"`
}

// clientResolver finds out who a client_id is.
type clientResolver struct {
	keys  *seal.Keyring
	hosts []string
	fetch func(ctx context.Context, url string) ([]byte, error)

	mu    sync.Mutex
	cache map[string]cachedClient // metadata documents, by URL
}

type cachedClient struct {
	client  client
	expires time.Time
}

const (
	metadataCacheSize = 100
	metadataCacheTime = 10 * time.Minute
	metadataMaxBytes  = 64 << 10
)

func newClientResolver(keys *seal.Keyring, hosts []string) *clientResolver {
	return &clientResolver{
		keys:  keys,
		hosts: hosts,
		fetch: newMetadataFetcher(hosts),
		cache: make(map[string]cachedClient),
	}
}

var errUnknownClient = errors.New("unknown client")

// resolve returns the client behind clientID: a metadata document URL on an allowed host, or a sealed
// registration.
func (r *clientResolver) resolve(ctx context.Context, clientID string) (client, error) {
	if strings.HasPrefix(clientID, "https://") {
		return r.metadataClient(ctx, clientID)
	}
	var reg registration
	err := r.keys.Open(seal.Client, clientID, &reg)
	if err != nil {
		return client{}, errUnknownClient
	}
	return client{ID: clientID, Name: reg.Name, RedirectURIs: reg.RedirectURIs}, nil
}

func (r *clientResolver) metadataClient(ctx context.Context, clientID string) (client, error) {
	u, err := url.Parse(clientID)
	if err != nil || u.User != nil || u.Port() != "" || !slices.Contains(r.hosts, u.Hostname()) {
		return client{}, fmt.Errorf("%w: metadata documents are only accepted from %s", errUnknownClient, strings.Join(r.hosts, ", "))
	}
	now := time.Now()
	r.mu.Lock()
	c, ok := r.cache[clientID]
	r.mu.Unlock()
	if ok && now.Before(c.expires) {
		return c.client, nil
	}

	raw, err := r.fetch(ctx, clientID)
	if err != nil {
		return client{}, fmt.Errorf("%w: fetching its metadata document: %w", errUnknownClient, err)
	}
	var doc struct {
		ClientID     string   `json:"client_id"`
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	err = json.Unmarshal(raw, &doc)
	if err != nil || doc.ClientID != clientID || len(doc.RedirectURIs) == 0 {
		return client{}, fmt.Errorf("%w: invalid metadata document", errUnknownClient)
	}
	for _, ru := range doc.RedirectURIs {
		if !isLoopback(ru) && !sameHTTPSHost(ru, u.Hostname()) {
			return client{}, fmt.Errorf("%w: redirect URI %q isn't on %s or loopback", errUnknownClient, ru, u.Hostname())
		}
	}
	cl := client{ID: clientID, Name: oneLine(doc.ClientName, 64), Host: u.Hostname(), RedirectURIs: doc.RedirectURIs}

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cache) >= metadataCacheSize {
		for k := range r.cache { // drop one; the cache only saves fetches
			delete(r.cache, k)
			break
		}
	}
	r.cache[clientID] = cachedClient{client: cl, expires: now.Add(metadataCacheTime)}
	return cl, nil
}

// register handles dynamic client registration. Only local programs register, so only loopback
// redirects are accepted: anything that says what it is can't send codes elsewhere.
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req)
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "the request isn't valid JSON")
		return
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 4 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "give between 1 and 4 redirect URIs")
		return
	}
	for _, ru := range req.RedirectURIs {
		if len(ru) > 200 || !isLoopback(ru) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri",
				"registered clients may only redirect to this computer (http://127.0.0.1, http://[::1] or http://localhost)")
			return
		}
	}
	name := oneLine(req.ClientName, 64)
	if name == "" {
		name = "unnamed program"
	}
	id, err := s.keys.Seal(seal.Client, registration{Name: name, RedirectURIs: req.RedirectURIs}, time.Time{})
	if err != nil {
		s.logger.ErrorContext(r.Context(), "sealing client registration", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "registration failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  id,
		"client_name":                name,
		"redirect_uris":              req.RedirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	})
}

// redirectAllowed reports whether redirectURI matches one of the client's URIs exactly. Loopback URIs
// may differ in port, as native apps pick one at run time.
func (c client) redirectAllowed(redirectURI string) bool {
	for _, registered := range c.RedirectURIs {
		if registered == redirectURI {
			return true
		}
		if isLoopback(registered) && isLoopback(redirectURI) {
			a, _ := url.Parse(registered)
			b, _ := url.Parse(redirectURI)
			if a.Scheme == b.Scheme && a.Hostname() == b.Hostname() && a.Path == b.Path && a.RawQuery == b.RawQuery {
				return true
			}
		}
	}
	return false
}

// isLoopback reports whether u is an http URL on this computer.
func isLoopback(u string) bool {
	p, err := url.Parse(u)
	if err != nil || p.Scheme != "http" || p.User != nil || p.Fragment != "" {
		return false
	}
	switch p.Hostname() {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

func sameHTTPSHost(u, host string) bool {
	p, err := url.Parse(u)
	return err == nil && p.Scheme == "https" && p.User == nil && p.Hostname() == host && p.Fragment == ""
}

// oneLine keeps the printable characters of s on one line, at most limit runes.
func oneLine(s string, limit int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == limit {
			break
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			continue
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// newMetadataFetcher returns a function that fetches client metadata documents from the allowed hosts
// only, refusing any address that isn't public after DNS resolution, ignoring proxy settings, following
// no redirects, and reading at most metadataMaxBytes within five seconds.
func newMetadataFetcher(hosts []string) func(ctx context.Context, url string) ([]byte, error) {
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: refusePrivate}
	hc := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: 5 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return func(ctx context.Context, rawURL string) ([]byte, error) {
		u, err := url.Parse(rawURL)
		if err != nil || u.Scheme != "https" || !slices.Contains(hosts, u.Hostname()) {
			return nil, errors.New("not an allowed metadata URL")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("status %s", resp.Status)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, metadataMaxBytes+1))
		if err != nil {
			return nil, err
		}
		if len(raw) > metadataMaxBytes {
			return nil, errors.New("metadata document too large")
		}
		return raw, nil
	}
}

// refusePrivate is a dialer Control hook: it refuses addresses that aren't public, after DNS resolution,
// so a hostile DNS answer can't point a fetch at something internal.
func refusePrivate(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	if !publicAddr(ip) {
		return fmt.Errorf("refusing to connect to %s", ip)
	}
	return nil
}

var (
	cgnat = netip.MustParsePrefix("100.64.0.0/10")
	nat64 = netip.MustParsePrefix("64:ff9b::/96")
)

func publicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsValid() &&
		!ip.IsUnspecified() &&
		!ip.IsLoopback() &&
		!ip.IsPrivate() &&
		!ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() &&
		!ip.IsMulticast() &&
		!ip.IsInterfaceLocalMulticast() &&
		!cgnat.Contains(ip) &&
		!nat64.Contains(ip)
}
