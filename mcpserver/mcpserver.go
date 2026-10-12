// Package mcpserver is speki's MCP endpoint: the HTTP stack in front of it (origin check, size limit,
// token, access, rate limit) and the MCP server itself.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/time/rate"

	"github.com/eve-online-tools/speki/access"
	"github.com/eve-online-tools/speki/oauth"
)

const maxBody = 2 << 20

// Per-user request rate: enough for an agent working quickly, not enough to hog the instance.
const (
	ratePerSecond = 10
	rateBurst     = 30
)

// authenticator puts the signed-in identity on the context (see oauth.NewContext).
type authenticator interface {
	Middleware(next http.Handler) http.Handler
	Unauthorized(w http.ResponseWriter, reason string)
}

type resolver interface {
	Resolve(ctx context.Context, u access.User) (*access.Grant, error)
}

// Server is the MCP endpoint.
type Server struct {
	id     Identity
	auth   authenticator
	grants resolver
	kb     knowledgeBase
	logger *slog.Logger
	mcp    *mcp.Server

	mu       sync.Mutex
	limiters map[int64]*rate.Limiter
}

// Identity says which speki this is, for clients connected to more than one. The zero value is a plain
// "speki".
type Identity struct {
	Name        string // speki-<Name> is the advertised name
	Title       string
	Description string // what belongs in this speki
}

func (id Identity) name() string {
	if id.Name == "" {
		return "speki"
	}
	return "speki-" + id.Name
}

func (id Identity) title() string {
	if t := strings.TrimSpace(id.Title); t != "" {
		return t
	}
	return "Speki knowledge base"
}

func (id Identity) description() string {
	return oneLine(strings.Join(strings.Fields(id.Description), " "))
}

// instructions open with the instance's description when it has one. Only that opening is formatted,
// so the shared text may hold any character.
func (id Identity) instructions() string {
	desc := id.description()
	if desc == "" {
		return instructions
	}
	head := "Speki: " + desc
	if strings.TrimSpace(id.Title) != "" {
		head = fmt.Sprintf("Speki %q: %s", oneLine(id.title()), desc)
	}
	return head + "\n\n" + instanceSearch + instructionsRest
}

// New returns the MCP endpoint, with its tools.
func New(auth authenticator, grants resolver, kb knowledgeBase, id Identity, version string, logger *slog.Logger) *Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    id.name(),
		Title:   id.title(),
		Version: version,
	}, &mcp.ServerOptions{Instructions: id.instructions()})
	s := &Server{
		id:       id,
		auth:     auth,
		grants:   grants,
		kb:       kb,
		logger:   logger,
		mcp:      srv,
		limiters: make(map[int64]*rate.Limiter),
	}
	s.addTools()
	return s
}

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

// grant runs before the MCP handler because only here can speki still answer with a status code: 401
// makes the client refresh or sign in again, 403 says why the user isn't allowed in.
func (s *Server) grant(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := oauth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "not signed in", http.StatusUnauthorized)
			return
		}
		g, err := s.grants.Resolve(r.Context(), access.User{
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

// rateLimit is per instance, not cluster-wide.
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
