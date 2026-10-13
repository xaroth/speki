package gh_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/xaroth/speki/gh"
)

var kb = gh.Repository{ID: 100, FullName: "ours/kb", DefaultBranch: "main", PushedAt: time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)}

// withKB is installation 1 with the repository ours/kb on branch main.
func withKB() gh.FakeOption {
	return func(tb testing.TB, f *gh.Fake) {
		tb.Helper()
		gh.ExpectInstallation(gh.Installation{ID: 1, AccountID: 42}, kb)(tb, f)
		gh.ExpectBranch(kb.FullName, "main", "abc123")(tb, f)
	}
}

func TestCheckPrivateKey(t *testing.T) {
	t.Parallel()
	fake := gh.NewFake(t)
	testCases := []struct {
		name    string
		key     []byte
		wantErr bool
	}{
		{name: "RSA key", key: fake.PrivateKey},
		{name: "not a key", key: []byte("not a key"), wantErr: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if err := gh.CheckPrivateKey(testCase.key); (err != nil) != testCase.wantErr {
				t.Errorf("CheckPrivateKey: got %v, want error %t", err, testCase.wantErr)
			}
			if _, err := gh.New(gh.Config{AppID: 1, PrivateKey: testCase.key}); (err != nil) != testCase.wantErr {
				t.Errorf("New: got %v, want error %t", err, testCase.wantErr)
			}
		})
	}
}

func TestAppOwner(t *testing.T) {
	t.Parallel()
	fake := gh.NewFake(t, gh.ExpectAppOwner(42))
	got, err := fake.AppOwner(t.Context())
	if err != nil {
		t.Fatalf("AppOwner: %v", err)
	}
	if got != 42 {
		t.Errorf("got owner %d, want 42", got)
	}
}

func TestInstallations(t *testing.T) {
	t.Parallel()
	want := []gh.Installation{
		{ID: 1, AccountID: 42},
		{ID: 2, AccountID: 7, Suspended: true},
	}
	fake := gh.NewFake(t, gh.ExpectPageSize(1), gh.ExpectInstallation(want[0]), gh.ExpectInstallation(want[1]))
	got, err := fake.Installations(t.Context())
	if err != nil {
		t.Fatalf("Installations: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("installations (-want +got):\n%s", diff)
	}
}

func TestRepositories(t *testing.T) {
	t.Parallel()
	other := gh.Repository{ID: 101, FullName: "ours/other", DefaultBranch: "master"}
	testCases := []struct {
		name         string
		installation int64
		want         []gh.Repository
		wantErr      bool
		wantNotFound bool
	}{
		{name: "paged", installation: 1, want: []gh.Repository{kb, other}},
		{name: "installation gone", installation: 9, wantErr: true, wantNotFound: true},
		{name: "token refused", installation: 8, wantErr: true}, // a 403 may be a rate limit, not gone
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := gh.NewFake(t, gh.ExpectPageSize(1),
				gh.ExpectInstallation(gh.Installation{ID: 1}, kb, other),
				gh.ExpectInstallation(gh.Installation{ID: 8}), gh.ExpectInstallation_Refused(8))
			got, err := fake.Repositories(t.Context(), testCase.installation)
			if (err != nil) != testCase.wantErr || errors.Is(err, gh.ErrNotFound) != testCase.wantNotFound {
				t.Fatalf("got %v, want error %t, ErrNotFound %t", err, testCase.wantErr, testCase.wantNotFound)
			}
			if diff := cmp.Diff(testCase.want, got); diff != "" {
				t.Errorf("repositories (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFile(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		repo    string
		path    string
		want    string
		wantErr error
	}{
		{name: "file", repo: kb.FullName, path: "speki.yaml", want: "title: KB\n"},
		{name: "missing", repo: kb.FullName, path: "missing.yaml", wantErr: gh.ErrNotFound},
		{name: "folder", repo: kb.FullName, path: "docs", wantErr: gh.ErrNotFound},
		{name: "over 64 KiB", repo: kb.FullName, path: "big.md", wantErr: gh.ErrTooLarge},
		{name: "over 1 MB, sent without content", repo: kb.FullName, path: "huge.md", wantErr: gh.ErrTooLarge},
		{name: "repository not seen", repo: "ours/gone", path: "speki.yaml", wantErr: gh.ErrNotFound},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := gh.NewFake(t, withKB(),
				gh.ExpectFile(kb.FullName, "speki.yaml", []byte("title: KB\n")),
				gh.ExpectFile(kb.FullName, "docs/a.md", []byte("a")),
				gh.ExpectFile(kb.FullName, "big.md", make([]byte, 64<<10+1)),
				gh.ExpectFile(kb.FullName, "huge.md", make([]byte, 1<<20+1)))
			got, err := fake.File(t.Context(), 1, testCase.repo, "main", testCase.path)
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("got %v, want %v", err, testCase.wantErr)
			}
			if string(got) != testCase.want {
				t.Errorf("got %q, want %q", got, testCase.want)
			}
		})
	}

	t.Run("rejects a repository name without an owner", func(t *testing.T) {
		t.Parallel()
		fake := gh.NewFake(t, withKB())
		if _, err := fake.File(t.Context(), 1, "kb", "main", "speki.yaml"); err == nil {
			t.Error("got no error, want one")
		}
		if got := fake.Requests(); len(got) != 0 {
			t.Errorf("got requests %q, want none", got)
		}
	})
}

// An installation's token is fetched once and used for its calls until it expires.
func TestInstallationTokenIsReused(t *testing.T) {
	t.Parallel()
	fake := gh.NewFake(t, withKB(), gh.ExpectFile(kb.FullName, "speki.yaml", []byte("x")))
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
			fake := gh.NewFake(t, withKB())
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

func TestBranchHead(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		repo    string
		branch  string
		etag    string
		want    gh.Head
		wantErr error
	}{
		{name: "head", repo: kb.FullName, branch: "main", want: gh.Head{SHA: "abc123", ETag: `"abc123"`}},
		{name: "unchanged", repo: kb.FullName, branch: "main", etag: `"abc123"`, want: gh.Head{ETag: `"abc123"`}},
		{name: "changed", repo: kb.FullName, branch: "main", etag: `"old"`, want: gh.Head{SHA: "abc123", ETag: `"abc123"`}},
		{name: "branch with a slash and a space", repo: kb.FullName, branch: "feature/x y", want: gh.Head{SHA: "def456", ETag: `"def456"`}},
		{name: "repository not seen", repo: "ours/gone", branch: "main", wantErr: gh.ErrNotFound},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := gh.NewFake(t, withKB(), gh.ExpectBranch(kb.FullName, "feature/x y", "def456"))
			got, err := fake.BranchHead(t.Context(), 1, testCase.repo, testCase.branch, testCase.etag)
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("got %v, want %v", err, testCase.wantErr)
			}
			if diff := cmp.Diff(testCase.want, got); diff != "" {
				t.Errorf("head (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRepositoryID(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		repo    string
		want    int64
		wantErr error
	}{
		{name: "found", repo: kb.FullName, want: 100},
		{name: "not seen", repo: "ours/gone", wantErr: gh.ErrNotFound},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := gh.NewFake(t, withKB())
			got, err := fake.RepositoryID(t.Context(), 1, testCase.repo)
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("got %v, want %v", err, testCase.wantErr)
			}
			if got != testCase.want {
				t.Errorf("got ID %d, want %d", got, testCase.want)
			}
		})
	}
}
