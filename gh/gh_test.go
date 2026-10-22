package gh_test

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/xaroth/speki/gh"
	"github.com/xaroth/speki/gh/ghtest"
)

// An installation's token is fetched once and used for its calls until it expires.
func TestInstallationTokenIsReused(t *testing.T) {
	t.Parallel()
	fake := ghtest.New(t, withKB(), ghtest.ExpectFile(kb.FullName, "speki.yaml", []byte("x")))
	if _, err := fake.Repositories(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := fake.File(t.Context(), 1, kb.FullName, "main", "speki.yaml"); err != nil {
		t.Fatal(err)
	}
	if _, err := fake.InstallationTokens(1).Token(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"POST /app/installations/1/access_tokens",
		"GET /installation/repositories?per_page=100",
		"GET /repos/ours/kb/contents/speki.yaml?ref=main",
	}
	if diff := cmp.Diff(want, fake.Requests()); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
}

func TestInstallationTokens(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name         string
		installation int64
		want         string
		wantNotFound bool
	}{
		{name: "token", installation: 1, want: "ghs_fake_1"},
		{name: "installation gone", installation: 9, wantNotFound: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := ghtest.New(t, withKB())
			got, err := fake.InstallationTokens(testCase.installation).Token(t.Context())
			if errors.Is(err, gh.ErrNotFound) != testCase.wantNotFound || err != nil && !testCase.wantNotFound {
				t.Fatalf("got %v, want ErrNotFound %t", err, testCase.wantNotFound)
			}
			if got != testCase.want {
				t.Errorf("got token %q, want %q", got, testCase.want)
			}
		})
	}
}
