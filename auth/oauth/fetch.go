package oauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"syscall"
	"time"
)

// errNoDocument means the host answered that there's no such document (404 or 410), as opposed to not
// answering or refusing.
var errNoDocument = errors.New("no metadata document")

// publicTransport connects to public addresses only, refusing any address that isn't public after DNS
// resolution, and ignores proxy settings.
func publicTransport() http.RoundTripper {
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: refusePrivate}
	return &http.Transport{
		Proxy:               nil,
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: 5 * time.Second,
	}
}

// newMetadataFetcher returns a function that fetches client metadata documents from the allowed hosts
// only, through rt, following no redirects, and reading at most metadataMaxBytes within five seconds.
func newMetadataFetcher(hosts []string, rt http.RoundTripper) func(ctx context.Context, url string) ([]byte, error) {
	hc := &http.Client{
		Timeout:       5 * time.Second,
		Transport:     rt,
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
		if err := statusError(resp); err != nil {
			return nil, err
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

// statusError returns the error for a metadata response's status, nil for 200. Only a 404 or 410 says the
// document is gone: a CDN's challenge or a bad deploy is a blip, not a removal.
func statusError(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound, http.StatusGone:
		return fmt.Errorf("%w: status %s", errNoDocument, resp.Status)
	}
	return fmt.Errorf("status %s", resp.Status)
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

// specialPurpose lists the address blocks of the IANA special-purpose registries that aren't public
// unicast and aren't covered by netip's own checks.
var specialPurpose = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/96"), // IPv4-compatible, such as ::127.0.0.1
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"), // 6to4, which can embed any IPv4 address
}

func publicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range specialPurpose {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}
