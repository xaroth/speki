package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/eve-online-tools/speki/seal"
)

// Identity is who made a request, from their speki access token.
type Identity struct {
	UserID      int64
	Login       string
	GitHubToken string
	Client      string
	Write       bool // the token has speki:write
}

const identityExtra = "speki.identity"

type identityKey struct{}

// NewContext returns ctx with id as the signed-in identity. Middleware does this for every request with a
// valid token; tests use it to sign someone in without a token.
func NewContext(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// IdentityFrom returns the signed-in identity on the context.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// Middleware accepts requests with a valid speki access token issued for this server, and answers others
// with a 401 that points clients at the sign-in flow.
func (s *Server) Middleware(next http.Handler) http.Handler {
	withIdentity := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := auth.TokenInfoFromContext(r.Context())
		id, _ := info.Extra[identityExtra].(Identity)
		next.ServeHTTP(w, r.WithContext(NewContext(r.Context(), id)))
	})
	return auth.RequireBearerToken(s.verify, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: s.resourceMetadataURL(),
		ClockSkew:           time.Minute,
	})(withIdentity)
}

// Unauthorized answers with the 401 that sends a client to sign in again, for a token GitHub no longer
// accepts.
func (s *Server) Unauthorized(w http.ResponseWriter, reason string) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf("Bearer resource_metadata=%q, error=\"invalid_token\"", s.resourceMetadataURL()))
	http.Error(w, reason, http.StatusUnauthorized)
}

func (s *Server) verify(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	var a accessValue
	err := s.keys.Open(seal.Access, token, &a)
	if errors.Is(err, seal.ErrExpired) {
		return nil, fmt.Errorf("%w: expired", auth.ErrInvalidToken)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: not a speki token", auth.ErrInvalidToken)
	}
	if a.Audience != s.resource() {
		return nil, fmt.Errorf("%w: issued for another server", auth.ErrInvalidToken)
	}
	return &auth.TokenInfo{
		Scopes:     a.Scopes,
		Expiration: s.now().Add(time.Minute), // seal checked the real expiry
		UserID:     fmt.Sprint(a.UserID),
		Extra: map[string]any{identityExtra: Identity{
			UserID:      a.UserID,
			Login:       a.Login,
			GitHubToken: a.Token,
			Client:      a.Client,
			Write:       slices.Contains(a.Scopes, ScopeWrite),
		}},
	}, nil
}
