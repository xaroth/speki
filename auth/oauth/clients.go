package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/sync/singleflight"

	"github.com/xaroth/speki/auth/seal"
)

// client is a registered MCP client.
type client struct {
	ID           string
	Name         string
	Host         string // for clients with a metadata document: the host it comes from. Empty for local ones
	RedirectURIs []string
}

type clientResolver struct {
	keys  *seal.Keyring
	hosts []string
	fetch func(ctx context.Context, url string) ([]byte, error)
	now   func() time.Time

	fetching chan struct{}      // a slot per metadata fetch in progress
	known    chan struct{}      // slots kept for refetching documents that were fetched before
	flight   singleflight.Group // one fetch per URL at a time, shared by its callers

	mu     sync.Mutex
	cache  map[string]cachedClient // metadata documents, by URL
	failed map[string]failedClient // failed fetches, by URL
}

type cachedClient struct {
	client  client
	expires time.Time // after this it's refetched, but still used while its host can't be reached
	used    time.Time // for evicting the least recently used
}

type failedClient struct {
	err     error
	expires time.Time
}

const (
	metadataCacheSize  = 100
	metadataCacheTime  = 10 * time.Minute
	metadataStaleTime  = time.Hour // how long past metadataCacheTime a document is used while it can't be refetched
	metadataFailedTime = time.Minute
	metadataRetryTime  = 10 * time.Second
	metadataSlotWait   = 2 * time.Second
	metadataFetches    = 4 // at once
	metadataKnown      = 2 // at once, for documents fetched before, besides metadataFetches
	metadataMaxBytes   = 64 << 10
)

func newClientResolver(keys *seal.Keyring, hosts []string) *clientResolver {
	return &clientResolver{
		keys:     keys,
		hosts:    hosts,
		fetch:    newMetadataFetcher(hosts, publicTransport()),
		now:      time.Now,
		fetching: make(chan struct{}, metadataFetches),
		known:    make(chan struct{}, metadataKnown),
		cache:    make(map[string]cachedClient),
		failed:   make(map[string]failedClient),
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
	err := r.keys.Open(seal.PurposeClient, clientID, &reg)
	if err != nil {
		return client{}, errUnknownClient
	}
	return client{ID: clientID, Name: reg.Name, RedirectURIs: reg.RedirectURIs}, nil
}

func (r *clientResolver) metadataClient(ctx context.Context, clientID string) (client, error) {
	u, err := url.Parse(clientID)
	if err != nil || u.User != nil || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" || !slices.Contains(r.hosts, u.Hostname()) {
		return client{}, fmt.Errorf("%w: metadata documents are only accepted from %s, without a query", errUnknownClient, strings.Join(r.hosts, ", "))
	}
	now := r.now()
	r.mu.Lock()
	cached, known := r.cache[clientID]
	if known {
		cached.used = now
		r.cache[clientID] = cached
	}
	f, failed := r.failed[clientID]
	r.mu.Unlock()
	usable := known && now.Before(cached.expires.Add(metadataStaleTime))
	switch {
	case known && now.Before(cached.expires):
		return cached.client, nil
	case usable && failed && now.Before(f.expires): // its host had a blip: the old document still holds
		return cached.client, nil
	case usable: // refetched in the background; until then the old document holds
		r.flight.DoChan(clientID, r.refetch(ctx, u, clientID, known))
		return cached.client, nil
	case failed && now.Before(f.expires):
		return client{}, f.err
	}

	// Requests for the same document share one fetch. A request that leaves stops waiting for it.
	var res singleflight.Result
	select {
	case res = <-r.flight.DoChan(clientID, r.refetch(ctx, u, clientID, known)):
	case <-ctx.Done():
		return client{}, ctx.Err()
	}
	if res.Err != nil {
		return client{}, res.Err
	}
	return res.Val.(client), nil
}

// refetch returns a shared fetch of a metadata document. The fetch is the server's, not the request's:
// a client that disconnects mustn't fail it for the others.
func (r *clientResolver) refetch(ctx context.Context, u *url.URL, clientID string, known bool) func() (any, error) {
	return func() (any, error) {
		cl, failure, err := r.fetchClient(context.WithoutCancel(ctx), u, clientID, known)
		r.remember(clientID, cl, failure, err)
		return cl, err
	}
}

// remember caches a fetched client, or a failure for as long as its kind deserves. Failures have their
// own small cache, so a stream of made-up URLs can't push real clients out.
func (r *clientResolver) remember(clientID string, cl client, failure failureKind, err error) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		var ttl time.Duration
		switch failure {
		case badDocument:
			ttl = metadataFailedTime
		case unreachable:
			ttl = metadataRetryTime
		default:
			return // busy: the next request tries again
		}
		if len(r.failed) >= metadataCacheSize {
			for k, f := range r.failed {
				if !now.Before(f.expires) || len(r.failed) >= metadataCacheSize {
					delete(r.failed, k)
				}
			}
		}
		r.failed[clientID] = failedClient{err: err, expires: now.Add(ttl)}
		if failure == badDocument {
			delete(r.cache, clientID) // the document is gone or now wrong: stop using the old one
		}
		return
	}
	delete(r.failed, clientID)
	if _, ok := r.cache[clientID]; !ok && len(r.cache) >= metadataCacheSize {
		oldest := ""
		for k, e := range r.cache {
			if oldest == "" || e.used.Before(r.cache[oldest].used) {
				oldest = k
			}
		}
		delete(r.cache, oldest)
	}
	r.cache[clientID] = cachedClient{client: cl, expires: now.Add(metadataCacheTime), used: now}
}

// failureKind says why a metadata fetch failed, which decides how long the failure is remembered.
type failureKind int

const (
	noFailure   failureKind = iota
	badDocument             // the document is missing or wrong: remembered for a minute
	unreachable             // it couldn't be fetched: remembered for a few seconds
	busy                    // too many fetches already: not remembered
)

// fetchClient fetches and checks a client metadata document. It waits a moment for a free fetch slot.
// Documents fetched before have slots of their own, so a crowd of unknown URLs can't keep known clients
// out.
func (r *clientResolver) fetchClient(ctx context.Context, u *url.URL, clientID string, known bool) (client, failureKind, error) {
	slots := r.fetching
	if known {
		slots = r.known
	}
	wait := time.NewTimer(metadataSlotWait)
	defer wait.Stop()
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-wait.C:
		return client{}, busy, fmt.Errorf("%w: too many metadata documents being fetched", errUnknownClient)
	}
	raw, err := r.fetch(ctx, clientID)
	if errors.Is(err, errNoDocument) {
		return client{}, badDocument, fmt.Errorf("%w: fetching its metadata document: %w", errUnknownClient, err)
	}
	if err != nil {
		return client{}, unreachable, fmt.Errorf("%w: fetching its metadata document: %w", errUnknownClient, err)
	}
	var doc struct {
		ClientID     string   `json:"client_id"`
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	err = json.Unmarshal(raw, &doc)
	if err != nil || doc.ClientID != clientID || len(doc.RedirectURIs) == 0 {
		return client{}, badDocument, fmt.Errorf("%w: invalid metadata document", errUnknownClient)
	}
	for _, ru := range doc.RedirectURIs {
		if !isLoopback(ru) && !sameHTTPSHost(ru, u.Hostname()) {
			return client{}, badDocument, fmt.Errorf("%w: redirect URI %q isn't on %s or loopback", errUnknownClient, ru, u.Hostname())
		}
	}
	return client{
		ID:           clientID,
		Name:         oneLine(doc.ClientName),
		Host:         u.Hostname(),
		RedirectURIs: doc.RedirectURIs,
	}, noFailure, nil
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

// maxClientName is the longest client name speki shows, in runes.
const maxClientName = 64

// oneLine keeps the printable characters of s on one line, at most maxClientName runes.
func oneLine(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == maxClientName {
			break
		}
		if unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp) || hidesText(r) {
			continue
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// hidesText reports whether r can reorder or hide text: the bidi controls and the tag characters. Other
// format characters, such as the zero-width joiners emoji and some scripts need, are kept.
func hidesText(r rune) bool {
	return r == 0x061c || r == 0x200e || r == 0x200f || (r >= 0x202a && r <= 0x202e) ||
		(r >= 0x2066 && r <= 0x2069) || (r >= 0xe0000 && r <= 0xe007f)
}
