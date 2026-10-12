package oauth_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

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

// A metadata document that couldn't be fetched isn't fetched again for a while: a minute when it's
// missing or wrong, a few seconds when its host had a blip.
func TestMetadataFailuresAreRemembered(t *testing.T) {
	t.Parallel()
	const doc = "https://claude.ai/oauth/other.json"
	testCases := []struct {
		name    string
		serve   http.HandlerFunc
		wantFor time.Duration
	}{
		{name: "wrong document", serve: document(claudeClient, claudeCallback), wantFor: time.Minute},
		{name: "missing document", serve: status(http.StatusNotFound), wantFor: time.Minute},
		{name: "host failing", serve: status(http.StatusServiceUnavailable), wantFor: 10 * time.Second},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				e.docs.serve(doc, testCase.serve)
				authorize := authorizeURL(doc, claudeCallback, nil)
				steps := []struct {
					name        string
					sleep       time.Duration
					wantStatus  int
					wantFetches int
				}{
					{name: "first", wantStatus: http.StatusBadRequest, wantFetches: 1},
					{name: "just before the failure runs out", sleep: testCase.wantFor - time.Second, wantStatus: http.StatusBadRequest, wantFetches: 1},
					{name: "once it ran out", sleep: time.Second, wantStatus: http.StatusOK, wantFetches: 2},
				}
				for _, step := range steps {
					if step.name == "just before the failure runs out" {
						e.docs.serve(doc, document(doc, claudeCallback))
					}
					time.Sleep(step.sleep)
					if got := e.do(t, authorize, nil).Code; got != step.wantStatus {
						t.Errorf("%s: got status %d, want %d", step.name, got, step.wantStatus)
					}
					if got := e.docs.count(doc); got != step.wantFetches {
						t.Errorf("%s: got %d fetches, want %d", step.name, got, step.wantFetches)
					}
				}
			})
		})
	}
}

// A document fetched before is refetched in the background, on fetch slots of its own, and is still used
// for an hour past its time while its host can't be reached. A document that's gone isn't used any more.
func TestKnownMetadataDocument(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		good := e.docs.docs[claudeClient]
		hang := make(chan struct{})
		var wg sync.WaitGroup
		defer func() {
			close(hang)
			wg.Wait()
		}()
		takeSlots := func() {
			for i := range 4 {
				u := fmt.Sprintf("https://claude.ai/hang-%d.json", i)
				e.docs.serve(u, func(http.ResponseWriter, *http.Request) { <-hang })
				wg.Go(func() { e.do(t, authorizeURL(u, claudeCallback, nil), nil) })
			}
		}
		steps := []struct {
			name        string
			before      func()
			serve       http.HandlerFunc
			clientID    string
			sleep       time.Duration
			wantStatus  int
			wantWait    time.Duration
			wantFetches int // of the known document
		}{
			{name: "first sign-in", wantStatus: http.StatusOK, wantFetches: 1},
			{name: "unknown document while every slot is taken", before: takeSlots, clientID: "https://claude.ai/new.json",
				wantStatus: http.StatusBadRequest, wantWait: 2 * time.Second, wantFetches: 1},
			{name: "expired, refetched in the background on a slot of its own", sleep: 10 * time.Minute, wantStatus: http.StatusOK, wantFetches: 2},
			{name: "refetched", wantStatus: http.StatusOK, wantFetches: 2},
			{name: "host failing, old document holds", serve: status(http.StatusServiceUnavailable), sleep: 10 * time.Minute, wantStatus: http.StatusOK, wantFetches: 3},
			{name: "failure remembered, old document holds", wantStatus: http.StatusOK, wantFetches: 3},
			{name: "an hour past its time", sleep: time.Hour, wantStatus: http.StatusBadRequest, wantFetches: 4},
			{name: "host back", serve: good, sleep: 10 * time.Second, wantStatus: http.StatusOK, wantFetches: 5},
			{name: "gone, old document used while refetching", serve: status(http.StatusNotFound), sleep: 10 * time.Minute, wantStatus: http.StatusOK, wantFetches: 6},
			{name: "gone", wantStatus: http.StatusBadRequest, wantFetches: 6},
		}
		for _, step := range steps {
			if step.before != nil {
				step.before()
				synctest.Wait()
			}
			if step.serve != nil {
				e.docs.serve(claudeClient, step.serve)
			}
			time.Sleep(step.sleep)
			clientID := step.clientID
			if clientID == "" {
				clientID = claudeClient
			}
			start := time.Now()
			got := e.do(t, authorizeURL(clientID, claudeCallback, nil), nil).Code
			if waited := time.Since(start); waited != step.wantWait {
				t.Errorf("%s: got an answer after %v, want %v", step.name, waited, step.wantWait)
			}
			synctest.Wait() // for the background refetch
			if got != step.wantStatus {
				t.Errorf("%s: got status %d, want %d", step.name, got, step.wantStatus)
			}
			if n := e.docs.count(claudeClient); n != step.wantFetches {
				t.Errorf("%s: got %d fetches, want %d", step.name, n, step.wantFetches)
			}
		}
	})
}

// Requests for one document share a fetch. One that leaves stops waiting, without failing the fetch for
// the others.
func TestMetadataFetchesAreShared(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		const doc = "https://claude.ai/shared.json"
		release := make(chan struct{})
		serve := document(doc, claudeCallback)
		e.docs.serve(doc, func(w http.ResponseWriter, r *http.Request) {
			<-release
			serve(w, r)
		})
		authorize := authorizeURL(doc, claudeCallback, nil)
		var wg sync.WaitGroup
		statuses := make(chan int, 8)
		for range 8 {
			wg.Go(func() { statuses <- e.do(t, authorize, nil).Code })
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if got := e.doCtx(t, ctx, authorize, nil).Code; got != http.StatusBadRequest {
			t.Errorf("request that left: got %d, want 400", got)
		}
		close(release)
		wg.Wait()
		close(statuses)
		for got := range statuses {
			if got != http.StatusOK {
				t.Errorf("got status %d, want 200", got)
			}
		}
		if n := e.docs.count(doc); n != 1 {
			t.Errorf("got %d fetches, want 1", n)
		}
	})
}

// The real transport refuses addresses that aren't public, whatever the host name says.
func TestMetadataFetchRefusesPrivateAddresses(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withRealMetadataTransport(), withMetadataHosts("127.0.0.1"))
	rec := e.do(t, authorizeURL("https://127.0.0.1/client.json", "https://127.0.0.1/cb", nil), nil)
	checkErrorPage(t, rec, http.StatusBadRequest, "This program isn't known to speki.", stepChoose)
	if !strings.Contains(e.logs.String(), "refusing to connect to 127.0.0.1") {
		t.Errorf("got logs %s, want the connection refused", e.logs)
	}
}
