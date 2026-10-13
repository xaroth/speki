package gh_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/xaroth/speki/gh"
)

func TestExchange(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name         string
		opts         []gh.FakeOption
		code         string
		wrongSecret  bool
		want         gh.UserTokens
		wantErr      bool
		wantBadGrant bool
		wantConfig   bool
	}{
		{
			name: "code",
			code: "good",
			want: gh.UserTokens{
				AccessToken:    writer,
				AccessExpires:  time.Now().Add(8 * time.Hour),
				RefreshToken:   "ghr_fake_1",
				RefreshExpires: time.Now().Add(184 * 24 * time.Hour),
			},
		},
		{
			name: "code for any verifier",
			opts: []gh.FakeOption{gh.ExpectOAuthCode("any", "", writer)},
			code: "any",
			want: gh.UserTokens{
				AccessToken:    writer,
				AccessExpires:  time.Now().Add(8 * time.Hour),
				RefreshToken:   "ghr_fake_1",
				RefreshExpires: time.Now().Add(184 * 24 * time.Hour),
			},
		},
		{name: "bad code", code: "bad", wantErr: true, wantBadGrant: true},
		{name: "wrong client secret", code: "good", wrongSecret: true, wantErr: true, wantConfig: true},
		{
			name:    "token that doesn't expire",
			opts:    []gh.FakeOption{respond("POST /login/oauth/access_token", http.StatusOK, nil, `{"access_token":"ghu_forever"}`)},
			code:    "good",
			wantErr: true,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := gh.NewFake(t, append([]gh.FakeOption{gh.ExpectOAuthCode("good", "verifier", writer)}, testCase.opts...)...)
			o := fake.OAuth
			if testCase.wrongSecret {
				o.ClientSecret = "wrong"
			}
			got, err := fake.Exchange(t.Context(), o, testCase.code, "verifier", "https://speki/cb")
			if (err != nil) != testCase.wantErr || errors.Is(err, gh.ErrBadGrant) != testCase.wantBadGrant ||
				errors.Is(err, gh.ErrAppConfig) != testCase.wantConfig {
				t.Fatalf("got %v, want error %t, ErrBadGrant %t, ErrAppConfig %t", err, testCase.wantErr, testCase.wantBadGrant, testCase.wantConfig)
			}
			if diff := cmp.Diff(testCase.want, got, cmpopts.EquateApproxTime(time.Minute)); diff != "" {
				t.Errorf("tokens (-want +got):\n%s", diff)
			}
		})
	}
}

// GitHub refresh tokens work once.
func TestRefresh(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name         string
		usedBefore   bool
		want         string
		wantBadGrant bool
	}{
		{name: "unused", want: "ghr_fake_2"},
		{name: "used", usedBefore: true, wantBadGrant: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := gh.NewFake(t, gh.ExpectOAuthCode("good", "verifier", writer))
			tok, err := fake.Exchange(t.Context(), fake.OAuth, "good", "verifier", "https://speki/cb")
			if err != nil {
				t.Fatal(err)
			}
			if testCase.usedBefore {
				if _, err := fake.Refresh(t.Context(), fake.OAuth, tok.RefreshToken); err != nil {
					t.Fatal(err)
				}
			}
			got, err := fake.Refresh(t.Context(), fake.OAuth, tok.RefreshToken)
			if errors.Is(err, gh.ErrBadGrant) != testCase.wantBadGrant || err != nil && !testCase.wantBadGrant {
				t.Fatalf("got %v, want ErrBadGrant %t", err, testCase.wantBadGrant)
			}
			if got.RefreshToken != testCase.want {
				t.Errorf("got refresh token %q, want %q", got.RefreshToken, testCase.want)
			}
		})
	}
}

func TestAuthorizeURL(t *testing.T) {
	t.Parallel()
	o := gh.OAuthConfig{ClientID: "id", URL: "https://github.example/"}
	got := o.AuthorizeURL("https://speki/cb", "st", "ch")
	want := "https://github.example/login/oauth/authorize?client_id=id&code_challenge=ch&code_challenge_method=S256&prompt=select_account&redirect_uri=https%3A%2F%2Fspeki%2Fcb&state=st"
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}
