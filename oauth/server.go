// Package oauth is speki's authorization server. MCP clients sign in here; speki sends the user through
// GitHub and hands the client its own sealed tokens, with the GitHub tokens inside. Nothing is stored:
// client registrations, state, codes and tokens are all sealed values.
package oauth

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/eve-online-tools/speki/gh"
	"github.com/eve-online-tools/speki/seal"
)

// Scopes speki grants.
const (
	ScopeRead  = "speki:read"
	ScopeWrite = "speki:write"
)

// Lifetimes of the values speki hands out.
const (
	consentLifetime = 10 * time.Minute
	codeLifetime    = time.Minute
	accessLifetime  = time.Hour
	signInLifetime  = 90 * 24 * time.Hour // refresh chains end this long after the user signed in
	usedCodeMemory  = 5 * time.Minute     // longer than a code lives, clock skew included
)

// Config configures the authorization server.
type Config struct {
	// Issuer is speki's public URL. Every URL speki hands out is built from it, never from requests.
	Issuer string
	// ClientMetadataHosts are the hosts whose client metadata documents are accepted.
	ClientMetadataHosts []string
	GitHub              gh.OAuthConfig
	// CheckUser runs at sign-in with the user's GitHub token, to turn away users who won't get in. Nil
	// lets everyone through.
	CheckUser func(ctx context.Context, githubToken string) error
}

// githubAPI is what sign-in needs from GitHub.
type githubAPI interface {
	Exchange(ctx context.Context, o gh.OAuthConfig, code, codeVerifier, redirectURI string) (gh.UserTokens, error)
	Refresh(ctx context.Context, o gh.OAuthConfig, refreshToken string) (gh.UserTokens, error)
	Me(ctx context.Context, token string) (gh.User, error)
}

// store holds the little short-lived state sealing can't replace: used codes and refresh results.
type stateStore interface {
	SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Release(ctx context.Context, key string, value []byte) error // deletes key while it holds value
}

// Server is the authorization server.
type Server struct {
	cfg     Config
	keys    *seal.Keyring
	github  githubAPI
	store   stateStore
	logger  *slog.Logger
	clients *clientResolver
	now     func() time.Time
	signIns *prometheus.CounterVec
}

// New returns an authorization server.
func New(cfg Config, keys *seal.Keyring, github githubAPI, st stateStore, logger *slog.Logger) *Server {
	cfg.Issuer = strings.TrimSuffix(cfg.Issuer, "/")
	return &Server{
		cfg:     cfg,
		keys:    keys,
		github:  github,
		store:   st,
		logger:  logger,
		clients: newClientResolver(keys, cfg.ClientMetadataHosts),
		now:     time.Now,
		signIns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "speki_signins_total",
			Help: "Sign-ins through GitHub, by result: ok, declined, refused (not allowed in) or failed.",
		}, []string{"result"}),
	}
}

// Collector returns the sign-in metrics, for registering.
func (s *Server) Collector() prometheus.Collector { return s.signIns }

// resource is the URL of the MCP endpoint, which tokens are issued for.
func (s *Server) resource() string { return s.cfg.Issuer + "/mcp" }

func (s *Server) resourceMetadataURL() string {
	return s.cfg.Issuer + "/.well-known/oauth-protected-resource/mcp"
}

func (s *Server) callbackURL() string { return s.cfg.Issuer + "/oauth/github/callback" }

// Handler serves the metadata and OAuth endpoints. Mount it at the root: its paths are fixed.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	prm := auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               s.resource(),
		AuthorizationServers:   []string{s.cfg.Issuer},
		ScopesSupported:        []string{ScopeRead, ScopeWrite},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "speki",
	})
	mux.Handle("GET /.well-known/oauth-protected-resource", prm)
	mux.Handle("GET /.well-known/oauth-protected-resource/mcp", prm)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.serverMetadata)
	mux.HandleFunc("POST /oauth/register", s.register)
	mux.HandleFunc("GET /oauth/authorize", s.authorize)
	mux.HandleFunc("POST /oauth/authorize", s.consent)
	mux.HandleFunc("GET /oauth/github/callback", s.callback)
	mux.HandleFunc("POST /oauth/token", s.token)
	return mux
}

func (s *Server) serverMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         s.cfg.Issuer,
		"authorization_endpoint":                         s.cfg.Issuer + "/oauth/authorize",
		"token_endpoint":                                 s.cfg.Issuer + "/oauth/token",
		"registration_endpoint":                          s.cfg.Issuer + "/oauth/register",
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"scopes_supported":                               []string{ScopeRead, ScopeWrite},
		"client_id_metadata_document_supported":          true,
		"authorization_response_iss_parameter_supported": true,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// oauthError answers a token or registration request with an OAuth error.
func oauthError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": description})
}
