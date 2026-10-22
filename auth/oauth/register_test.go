package oauth_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestRegister(t *testing.T) {
	t.Parallel()
	long := make([]string, 4)
	for i := range long {
		long[i] = fmt.Sprintf("http://127.0.0.1/%d/%s", i, strings.Repeat("x", 180))
	}
	type registered struct {
		ClientName              string   `json:"client_name"`
		RedirectURIs            []string `json:"redirect_uris"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		GrantTypes              []string `json:"grant_types"`
		ResponseTypes           []string `json:"response_types"`
	}
	registration := func(name string, uris ...string) registered {
		return registered{
			ClientName: name, RedirectURIs: uris, TokenEndpointAuthMethod: "none",
			GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"},
		}
	}
	badRedirect := oauthError{Error: "invalid_redirect_uri", Description: "registered clients may only redirect to this computer (http://127.0.0.1, http://[::1] or http://localhost)"}
	testCases := []struct {
		name       string
		body       any // marshalled unless a string
		wantStatus int
		want       registered
		wantErr    oauthError
	}{
		{
			name:       "loopback hosts",
			body:       map[string]any{"client_name": "Claude Code", "redirect_uris": []string{"http://127.0.0.1/cb", "http://[::1]:8080/cb", "http://localhost/cb"}},
			wantStatus: http.StatusCreated,
			want:       registration("Claude Code", "http://127.0.0.1/cb", "http://[::1]:8080/cb", "http://localhost/cb"),
		},
		{name: "unnamed", body: map[string]any{"redirect_uris": []string{loopback}}, wantStatus: http.StatusCreated, want: registration("unnamed program", loopback)},
		{
			name:       "name with characters that break or hide text",
			body:       map[string]any{"client_name": "Claude\nCode\u202e \U0001f468\u200d\U0001f469\U000e0041\u0085", "redirect_uris": []string{loopback}},
			wantStatus: http.StatusCreated,
			want:       registration("ClaudeCode \U0001f468\u200d\U0001f469", loopback),
		},
		{
			name:       "long name",
			body:       map[string]any{"client_name": strings.Repeat("n", 70), "redirect_uris": []string{loopback}},
			wantStatus: http.StatusCreated,
			want:       registration(strings.Repeat("n", 64), loopback),
		},

		{name: "not JSON", body: "{", wantStatus: http.StatusBadRequest, wantErr: oauthError{Error: "invalid_client_metadata", Description: "the request isn't valid JSON"}},
		{name: "no redirect URIs", body: map[string]any{"redirect_uris": []string{}}, wantStatus: http.StatusBadRequest,
			wantErr: oauthError{Error: "invalid_redirect_uri", Description: "give between 1 and 4 redirect URIs"}},
		{name: "five redirect URIs", body: map[string]any{"redirect_uris": []string{loopback, loopback, loopback, loopback, loopback}}, wantStatus: http.StatusBadRequest,
			wantErr: oauthError{Error: "invalid_redirect_uri", Description: "give between 1 and 4 redirect URIs"}},
		{name: "https redirect", body: map[string]any{"redirect_uris": []string{"https://evil.example/cb"}}, wantStatus: http.StatusBadRequest, wantErr: badRedirect},
		{name: "redirect URI too long", body: map[string]any{"redirect_uris": []string{"http://127.0.0.1/" + strings.Repeat("x", 200)}}, wantStatus: http.StatusBadRequest, wantErr: badRedirect},
		{
			name:       "registration too long for authorize",
			body:       map[string]any{"client_name": strings.Repeat("n", 64), "redirect_uris": long},
			wantStatus: http.StatusBadRequest,
			wantErr: oauthError{Error: "invalid_client_metadata",
				Description: "the registration is too long: use fewer or shorter redirect URIs, or a shorter name"},
		},
	}
	e := newEnv(t)
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			body, ok := testCase.body.(string)
			if !ok {
				raw, _ := json.Marshal(testCase.body)
				body = string(raw)
			}
			rec := e.doJSON(t, "/oauth/register", body)
			if rec.Code != testCase.wantStatus {
				t.Fatalf("got status %d %s, want %d", rec.Code, rec.Body, testCase.wantStatus)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("got Cache-Control %q, want no-store", got)
			}
			if testCase.wantErr.Error != "" {
				var got oauthError
				_ = json.Unmarshal(rec.Body.Bytes(), &got)
				if diff := cmp.Diff(testCase.wantErr, got); diff != "" {
					t.Errorf("error mismatch (-want +got):\n%s", diff)
				}
				return
			}
			var got registered
			_ = json.Unmarshal(rec.Body.Bytes(), &got)
			if diff := cmp.Diff(testCase.want, got); diff != "" {
				t.Errorf("registration mismatch (-want +got):\n%s", diff)
			}
			var id struct {
				ClientID string `json:"client_id"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &id)
			if page := e.do(t, authorizeURL(id.ClientID, testCase.want.RedirectURIs[0], nil), nil); page.Code != http.StatusOK {
				t.Errorf("authorize with the new client: got %d, want 200", page.Code)
			}
		})
	}
}
