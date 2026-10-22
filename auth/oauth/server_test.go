package oauth_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestServerMetadata(t *testing.T) {
	t.Parallel()
	resource := map[string]any{
		"resource":                 issuer + "/mcp",
		"authorization_servers":    []any{issuer},
		"scopes_supported":         []any{"speki:read", "speki:write"},
		"bearer_methods_supported": []any{"header"},
		"resource_name":            "speki",
	}
	testCases := []struct {
		name   string
		target string
		want   map[string]any
	}{
		{
			name:   "authorization server",
			target: "/.well-known/oauth-authorization-server",
			want: map[string]any{
				"issuer":                                         issuer,
				"authorization_endpoint":                         issuer + "/oauth/authorize",
				"token_endpoint":                                 issuer + "/oauth/token",
				"registration_endpoint":                          issuer + "/oauth/register",
				"response_types_supported":                       []any{"code"},
				"grant_types_supported":                          []any{"authorization_code", "refresh_token"},
				"code_challenge_methods_supported":               []any{"S256"},
				"token_endpoint_auth_methods_supported":          []any{"none"},
				"scopes_supported":                               []any{"speki:read", "speki:write"},
				"client_id_metadata_document_supported":          true,
				"authorization_response_iss_parameter_supported": true,
			},
		},
		{name: "protected resource", target: "/.well-known/oauth-protected-resource", want: resource},
		{name: "protected resource for /mcp", target: "/.well-known/oauth-protected-resource/mcp", want: resource},
	}
	e := newEnv(t, withIssuer(issuer+"/")) // a trailing slash in the config doesn't reach the URLs
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			rec := e.do(t, testCase.target, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("got status %d, want 200", rec.Code)
			}
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(testCase.want, got); diff != "" {
				t.Errorf("metadata mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func BenchmarkHandler(b *testing.B) {
	e := newEnv(b)
	clientID := e.register(b, loopback)
	authorize := authorizeURL(clientID, loopback, nil)
	page := e.do(b, authorize, nil)
	form := consentForm(b, page, "allow", "write")
	consentC := cookie(page, consentCookie)
	toGitHub, state := e.toGitHub(b, authorize, "write")
	tx := cookie(toGitHub, txCookie+state)
	signedInClient, tokens := e.signedIn(b)
	registration, _ := json.Marshal(map[string]any{"client_name": "Claude Code", "redirect_uris": []string{loopback}})

	benchmarks := []struct {
		name string
		do   func(b *testing.B) int
		want int
	}{
		{name: "server metadata", want: http.StatusOK, do: func(b *testing.B) int {
			return e.do(b, "/.well-known/oauth-authorization-server", nil).Code
		}},
		{name: "register", want: http.StatusCreated, do: func(b *testing.B) int {
			return e.doJSON(b, "/oauth/register", string(registration)).Code
		}},
		{name: "authorize", want: http.StatusOK, do: func(b *testing.B) int { return e.do(b, authorize, nil).Code }},
		{name: "consent", want: http.StatusSeeOther, do: func(b *testing.B) int {
			return e.do(b, "/oauth/authorize", form, consentC).Code
		}},
		{name: "callback", want: http.StatusFound, do: func(b *testing.B) int {
			return e.do(b, "/oauth/github/callback?code=good&state="+state, nil, tx).Code
		}},
		{name: "font", want: http.StatusOK, do: func(b *testing.B) int {
			return e.do(b, "/oauth/fonts/alegreya.woff2", nil).Code
		}},
		{name: "refresh", want: http.StatusOK, do: func(b *testing.B) int {
			resp, rec := e.token(b, refreshForm(signedInClient, tokens.RefreshToken))
			tokens = resp // each refresh token works once
			return rec.Code
		}},
		{name: "middleware", want: http.StatusOK, do: func(b *testing.B) int {
			_, rec := e.identity(b, tokens.AccessToken)
			return rec.Code
		}},
	}
	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			for b.Loop() {
				if got := bm.do(b); got != bm.want {
					b.Fatalf("got status %d, want %d", got, bm.want)
				}
			}
		})
	}
}
