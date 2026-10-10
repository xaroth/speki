// Package gh is speki's GitHub API client. It authenticates as the GitHub App (JWT) and as its
// installations, and turns GitHub's answers into the few types and errors the rest of speki needs.
package gh

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v88/github"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
)

// ErrNotFound means GitHub definitely says the thing doesn't exist (or the app can't see it).
var ErrNotFound = errors.New("not found on GitHub")

// DefaultAPIURL is GitHub's REST API.
const DefaultAPIURL = "https://api.github.com/"

const requestTimeout = 30 * time.Second

// Config identifies the GitHub App.
type Config struct {
	AppID      int64
	PrivateKey []byte // PEM
	// APIURL is the REST API base URL. Empty means DefaultAPIURL. Tests point it at a fake.
	APIURL string
}

// Client talks to GitHub as the app and as its installations.
type Client struct {
	apiURL  string
	base    http.RoundTripper
	web     *http.Client // GitHub's OAuth endpoints, which aren't part of the REST API
	appAuth *ghinstallation.AppsTransport
	app     *github.Client
	metrics *metrics

	mu       sync.Mutex
	installs map[int64]*installation
}

type installation struct {
	auth   *ghinstallation.Transport
	client *github.Client
}

// CheckPrivateKey reports whether pem is a GitHub App private key New accepts.
func CheckPrivateKey(pem []byte) error {
	_, err := ghinstallation.NewAppsTransport(http.DefaultTransport, 1, pem)
	return err
}

// New returns a client for the app described by cfg. Request metrics are registered with reg.
func New(cfg Config, reg prometheus.Registerer) (*Client, error) {
	apiURL := cfg.APIURL
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	if !strings.HasSuffix(apiURL, "/") {
		apiURL += "/"
	}
	m, err := newMetrics(reg)
	if err != nil {
		return nil, err
	}

	// A span per GitHub request when tracing is on, without sending speki's trace context to GitHub.
	base := otelhttp.NewTransport(http.DefaultTransport, otelhttp.WithPropagators(propagation.NewCompositeTextMapPropagator()))
	appAuth, err := ghinstallation.NewAppsTransport(m.observe("app", base), cfg.AppID, cfg.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("github app key: %w", err)
	}
	appAuth.BaseURL = strings.TrimSuffix(apiURL, "/")

	app, err := newGitHub(apiURL, appAuth)
	if err != nil {
		return nil, err
	}
	return &Client{
		apiURL:   apiURL,
		base:     base,
		web:      &http.Client{Transport: m.observe("oauth", base), Timeout: requestTimeout},
		appAuth:  appAuth,
		app:      app,
		metrics:  m,
		installs: make(map[int64]*installation),
	}, nil
}

func newGitHub(apiURL string, rt http.RoundTripper) (*github.Client, error) {
	c, err := github.NewClient(
		github.WithTransport(rt),
		github.WithTimeout(requestTimeout),
		github.WithURLs(&apiURL, nil),
	)
	if err != nil {
		return nil, fmt.Errorf("github client: %w", err)
	}
	return c, nil
}

// installation returns the auth transport and API client for one installation, creating them once.
// The transport renews the installation token as needed.
func (c *Client) installation(id int64) (*installation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	in, ok := c.installs[id]
	if ok {
		return in, nil
	}
	auth := ghinstallation.NewFromAppsTransport(c.appAuth, id)
	auth.BaseURL = c.appAuth.BaseURL
	client, err := newGitHub(c.apiURL, c.metrics.observe("installation", auth))
	if err != nil {
		return nil, err
	}
	in = &installation{auth: auth, client: client}
	c.installs[id] = in
	return in, nil
}

func (c *Client) installationClient(id int64) (*github.Client, error) {
	in, err := c.installation(id)
	if err != nil {
		return nil, err
	}
	return in.client, nil
}

// notFound turns a definite 404 from GitHub into ErrNotFound: for the API, and for an installation token
// GitHub refuses to issue because the installation is gone. Other failures, a 403 included (it may be a
// rate limit), stay ordinary errors.
func notFound(err error) error {
	var gerr *github.ErrorResponse
	if errors.As(err, &gerr) && gerr.Response != nil && gerr.Response.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	var terr *ghinstallation.HTTPError
	if errors.As(err, &terr) && terr.Response != nil && terr.Response.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return err
}

// TokenSource hands out installation tokens, for git over HTTPS.
type TokenSource struct {
	c  *Client
	id int64
}

// Token returns a current installation token.
func (s TokenSource) Token(ctx context.Context) (string, error) {
	in, err := s.c.installation(s.id)
	if err != nil {
		return "", err
	}
	t, err := in.auth.Token(ctx)
	if err != nil {
		return "", notFound(err)
	}
	return t, nil
}

// InstallationTokens returns a TokenSource for one installation.
func (c *Client) InstallationTokens(installationID int64) TokenSource {
	return TokenSource{c: c, id: installationID}
}
