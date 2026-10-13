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
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/time/rate"

	"github.com/xaroth/speki/auth/access"
)

const maxBody = 2 << 20

// Retry-After, in seconds, for a user whose vaults are still being built, and for one whose vaults failed
// to open and are retried with backoff.
const (
	notReadyRetry = "15"
	failedRetry   = "120"
)

// bodyTimeout is how long the MCP handler has to read the body once the grant is resolved. The server's
// ReadTimeout runs from the start of the request, and resolving can wait for vaults being built.
const bodyTimeout = 30 * time.Second

// requestTimeout bounds a /mcp request from its start, the wait for vaults being built included, so a
// lock wait or GitHub call ends with an error the agent sees rather than a cut connection. The server's
// WriteTimeout leaves room past it for a change looking for a commit whose answer was lost.
const requestTimeout = 50 * time.Second

// Per-user request rate: enough for an agent working quickly, not enough to hog the instance.
const (
	ratePerSecond = 10
	rateBurst     = 30
)

// authenticator signs requests in.
type authenticator interface {
	Middleware(next http.Handler) http.Handler
	User(ctx context.Context) (access.User, bool) // who Middleware signed in
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

// Handler returns the whole /mcp stack, outermost first: deadline, cross-origin check, body limit, token
// check, rate limit, grant, MCP.
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
	h = http.NewCrossOriginProtection().Handler(h)
	return withDeadline(h)
}

type deadlineKey struct{}

// withDeadline sets the request's deadline, from its start, as a value: the go-sdk runs tools on a context
// that keeps values but not deadlines, and the HTTP request must outlive the tool to carry its answer. The
// grant applies it to resolving, handle to the tool.
func withDeadline(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), deadlineKey{}, time.Now().Add(requestTimeout))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// withRequestDeadline returns ctx ending at the request's deadline, if withDeadline set one.
func withRequestDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	d, ok := ctx.Value(deadlineKey{}).(time.Time)
	if !ok {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, d)
}

type grantKey struct{}

// GrantFrom returns the grant of the request's user.
func GrantFrom(ctx context.Context) (*access.Grant, bool) {
	g, ok := ctx.Value(grantKey{}).(*access.Grant)
	return g, ok
}

// grant runs before the MCP handler because only here can speki still answer with a status code: 401
// makes the client refresh or sign in again, 403 says why the user isn't allowed in, 503 that their vaults
// aren't served yet.
func (s *Server) grant(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.auth.User(r.Context())
		if !ok {
			http.Error(w, "not signed in", http.StatusUnauthorized)
			return
		}
		rctx, cancel := withRequestDeadline(r.Context())
		g, err := s.grants.Resolve(rctx, u)
		cancel()
		switch {
		case errors.Is(err, access.ErrUnauthorized):
			s.auth.Unauthorized(w, "GitHub no longer accepts this sign-in: sign in again")
			return
		case errors.Is(err, access.ErrNotMember), errors.Is(err, access.ErrNoVaults):
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		case errors.Is(err, access.ErrNotReady):
			w.Header().Set("Retry-After", notReadyRetry)
			http.Error(w, access.ErrNotReady.Error(), http.StatusServiceUnavailable)
			return
		case errors.Is(err, access.ErrVaultsFailed):
			w.Header().Set("Retry-After", failedRetry)
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		case errors.Is(err, context.DeadlineExceeded) && r.Context().Err() == nil:
			s.logger.WarnContext(r.Context(), "resolving access ran out of time", "login", u.Login, "err", err)
			w.Header().Set("Retry-After", notReadyRetry)
			http.Error(w, "speki couldn't check your access in time; try again", http.StatusServiceUnavailable)
			return
		case err != nil:
			s.logger.ErrorContext(r.Context(), "resolving access failed", "login", u.Login, "err", err)
			http.Error(w, "speki couldn't check your access with GitHub; try again", http.StatusBadGateway)
			return
		}
		// Fails only where deadlines aren't supported, such as in tests: nothing to extend there.
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyTimeout))
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), grantKey{}, g)))
	})
}

// rateLimit is per instance, not cluster-wide.
func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := s.auth.User(r.Context())
		if !s.limiter(u.ID).Allow() {
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
