// Package mcpserver is speki's MCP endpoint: the HTTP stack in front of it (origin check, size limit,
// token, access, rate limit) and the MCP server itself.
package mcpserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/time/rate"

	"github.com/eve-online-tools/speki/access"
	"github.com/eve-online-tools/speki/oauth"
)

// maxBody is the largest request speki reads.
const maxBody = 2 << 20

// Per-user request rate: enough for an agent working quickly, not enough to hog the instance.
const (
	ratePerSecond = 10
	rateBurst     = 30
)

// authenticator checks tokens and puts the signed-in identity on the context (see oauth.NewContext).
type authenticator interface {
	Middleware(next http.Handler) http.Handler
	Unauthorized(w http.ResponseWriter, reason string)
}

// resolver turns a signed-in user into a grant.
type resolver interface {
	Resolve(ctx context.Context, u access.User) (*access.Grant, error)
}

// Server is the MCP endpoint.
type Server struct {
	auth    authenticator
	access  resolver
	kb      knowledgeBase
	logger  *slog.Logger
	mcp     *mcp.Server
	metrics *metrics

	mu       sync.Mutex
	limiters map[int64]*rate.Limiter
}

// New returns the MCP endpoint, with its tools.
func New(auth authenticator, access resolver, kb knowledgeBase, version string, logger *slog.Logger) *Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "speki",
		Title:   "Speki knowledge base",
		Version: version,
	}, &mcp.ServerOptions{Instructions: instructions})
	s := &Server{
		auth:     auth,
		access:   access,
		kb:       kb,
		logger:   logger,
		mcp:      srv,
		metrics:  newMetrics(),
		limiters: make(map[int64]*rate.Limiter),
	}
	s.addTools()
	return s
}

// Collector returns the tool call metrics, for registering.
func (s *Server) Collector() prometheus.Collector { return s.metrics }

// Handler returns the whole /mcp stack, outermost first: cross-origin check, body limit, token check,
// rate limit, grant, MCP.
func (s *Server) Handler() http.Handler {
	sdk := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s.mcp },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
	var h http.Handler = sdk
	h = s.grant(h)
	h = s.rateLimit(h) // before grant: turned-away users would otherwise cost GitHub requests unthrottled
	h = s.auth.Middleware(h)
	h = http.MaxBytesHandler(h, maxBody)
	return http.NewCrossOriginProtection().Handler(h)
}

type grantKey struct{}

// GrantFrom returns the grant of the request's user.
func GrantFrom(ctx context.Context) (*access.Grant, bool) {
	g, ok := ctx.Value(grantKey{}).(*access.Grant)
	return g, ok
}

// grant resolves what the user may do, before the MCP handler: only here can speki still answer with a
// status code. A token GitHub no longer accepts gets a 401, so the client refreshes or signs in again;
// a user who isn't allowed in gets a 403 that says why.
func (s *Server) grant(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := oauth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "not signed in", http.StatusUnauthorized)
			return
		}
		g, err := s.access.Resolve(r.Context(), access.User{
			ID:     id.UserID,
			Login:  id.Login,
			Token:  id.GitHubToken,
			Client: id.Client,
			Write:  id.Write,
		})
		switch {
		case errors.Is(err, access.ErrUnauthorized):
			s.auth.Unauthorized(w, "GitHub no longer accepts this sign-in: sign in again")
			return
		case errors.Is(err, access.ErrNotMember), errors.Is(err, access.ErrNoVaults):
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		case err != nil:
			s.logger.ErrorContext(r.Context(), "resolving access failed", "login", id.Login, "err", err)
			http.Error(w, "speki couldn't check your access with GitHub; try again", http.StatusBadGateway)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), grantKey{}, g)))
	})
}

// rateLimit holds each user to ratePerSecond requests, with bursts of rateBurst, on this instance.
func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := oauth.IdentityFrom(r.Context())
		if !s.limiter(id.UserID).Allow() {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many requests: slow down", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) limiter(user int64) *rate.Limiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.limiters[user]
	if !ok {
		l = rate.NewLimiter(ratePerSecond, rateBurst)
		s.limiters[user] = l
	}
	return l
}
