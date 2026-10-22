package gh_test

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/xaroth/speki/gh"
	"github.com/xaroth/speki/gh/ghtest"
)

const (
	writer  = "ghu_writer"
	reader  = "ghu_reader"
	revoked = "ghu_revoked"
)

// withUsers adds octocat, who may write to ours/kb, and reader, who may only read it.
func withUsers() ghtest.Option {
	return func(tb testing.TB, f *ghtest.Fake) {
		tb.Helper()
		ghtest.ExpectUser(writer, gh.User{ID: 7, Login: "octocat"})(tb, f)
		ghtest.ExpectRole(writer, kb.FullName, gh.RoleWrite)(tb, f)
		ghtest.ExpectUser(reader, gh.User{ID: 8, Login: "reader"})(tb, f)
		ghtest.ExpectRole(reader, kb.FullName, gh.RoleRead)(tb, f)
	}
}

// respond answers a route with a fixed response.
func respond(route string, status int, header http.Header, body string) ghtest.Option {
	return ghtest.ExpectRoute(route, func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			for k, v := range header {
				w.Header()[k] = v
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		})
	})
}

func recorded(tb testing.TB, name string) ghtest.Option {
	tb.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		tb.Fatal(err)
	}
	return respond("POST /graphql", http.StatusOK, nil, string(body))
}

func TestCommit(t *testing.T) {
	t.Parallel()
	fake := ghtest.New(t, withKB(), withUsers(),
		ghtest.ExpectFile(kb.FullName, "a.md", []byte("old")),
		ghtest.ExpectFile(kb.FullName, "old.md", []byte("gone soon")))
	sha, err := fake.Commit(t.Context(), writer, gh.CommitRequest{
		Repo: kb.FullName, Branch: "main", ExpectedHead: "abc123", Message: "Update a.md\n\nSpeki-Client: test",
		Changes: []gh.FileChange{
			{Path: "a.md", Content: []byte("new")},
			{Path: "old.md"},
			{Path: "empty.md", Content: []byte{}}, // an empty file, not a deletion
		},
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := fake.BranchSHA(kb.FullName, "main"); sha != got {
		t.Errorf("got SHA %q, want the branch's new head %q", sha, got)
	}
	got := map[string]string{}
	for _, path := range []string{"a.md", "old.md", "empty.md"} {
		if content, ok := fake.FileContent(kb.FullName, path); ok {
			got[path] = string(content)
		}
	}
	want := map[string]string{"a.md": "new", "empty.md": ""}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("files (-want +got):\n%s", diff)
	}
	history, err := fake.Commits(t.Context(), writer, kb.FullName, "main", "", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Message != "Update a.md\n\nSpeki-Client: test" {
		t.Errorf("got history %+v, want the commit with its message", history)
	}
}

// errUnknown stands for an error that says nothing about whether the commit landed.
var errUnknown = errors.New("unknown outcome")

func TestCommitErrors(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name  string
		opts  []ghtest.Option
		token string
		head  string
		want  error
	}{
		{name: "branch moved", head: "old", want: gh.ErrHeadMoved},
		{name: "read only", token: reader, want: gh.ErrForbidden},
		{name: "token revoked", token: revoked, want: gh.ErrUnauthorized},
		{
			name: "ruleset",
			opts: []ghtest.Option{ghtest.ExpectCommit_Error("UNPROCESSABLE", "Repository rule violations found\n\nChanges must be made through a pull request.")},
			want: gh.ErrBranchRules,
		},
		{
			name: "status check named like a secret", // the check's name is the user's, not GitHub's verdict
			opts: []ghtest.Option{ghtest.ExpectCommit_Error("FORBIDDEN", "Repository rule violations found\n\nRequired status check \"secret scanning\" is expected.\n\n")},
			want: gh.ErrBranchRules,
		},
		{
			name: "push protection",
			opts: []ghtest.Option{ghtest.ExpectCommit_Error("UNPROCESSABLE", "Push cannot contain secrets:\nGitHub Personal Access Token")},
			want: &gh.SecretError{Detail: "Push cannot contain secrets: GitHub Personal Access Token"},
		},
		{
			name: "repository not seen",
			opts: []ghtest.Option{ghtest.ExpectCommit_Error("NOT_FOUND", "Could not resolve to a Repository with the name 'ours/kb'.")},
			want: gh.ErrForbidden,
		},
		{
			name: "forbidden, saying something went wrong", // not a timeout
			opts: []ghtest.Option{ghtest.ExpectCommit_Error("FORBIDDEN", "Something went wrong: you can't do that")},
			want: gh.ErrForbidden,
		},
		{
			name: "timeout",
			opts: []ghtest.Option{ghtest.ExpectCommit_Error("", "Something went wrong while executing your query. This may be the result of a timeout, or it could be a GitHub bug.")},
			want: errUnknown,
		},
		{
			name: "rate limited among other errors",
			opts: []ghtest.Option{ghtest.ExpectCommit_Error("UNPROCESSABLE", "something"), ghtest.ExpectCommit_Error("RATE_LIMITED", "API rate limit exceeded")},
			want: &gh.RateLimitError{RetryAfter: time.Minute},
		},
		{
			name: "other refusal",
			opts: []ghtest.Option{ghtest.ExpectCommit_Error("UNPROCESSABLE", "something else")},
			want: gh.ErrRefused,
		},
		{
			name: "secondary rate limit",
			opts: []ghtest.Option{respond("POST /graphql", http.StatusForbidden, http.Header{"Retry-After": {"30"}},
				`{"message":"You have exceeded a secondary rate limit.","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#about-secondary-rate-limits"}`)},
			want: &gh.RateLimitError{RetryAfter: 30 * time.Second},
		},
		{
			name: "HTTP not found",
			opts: []ghtest.Option{respond("POST /graphql", http.StatusNotFound, nil, `{"message":"Not Found"}`)},
			want: gh.ErrForbidden,
		},
		// Responses recorded from github.com: all FORBIDDEN, told apart only by their messages.
		{name: "recorded pull request rule", opts: []ghtest.Option{recorded(t, "commit-ruleset-pull-request")}, want: gh.ErrBranchRules},
		{name: "recorded status check rule", opts: []ghtest.Option{recorded(t, "commit-ruleset-status-check")}, want: gh.ErrBranchRules},
		{
			name: "recorded secret",
			opts: []ghtest.Option{recorded(t, "commit-secret")},
			want: &gh.SecretError{Detail: "Repository rule violations found Secret detected in content"},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := ghtest.New(t, append([]ghtest.Option{withKB(), withUsers()}, testCase.opts...)...)
			token, head := testCase.token, testCase.head
			if token == "" {
				token = writer
			}
			if head == "" {
				head = "abc123"
			}
			_, err := fake.Commit(t.Context(), token, gh.CommitRequest{
				Repo: kb.FullName, Branch: "main", ExpectedHead: head, Message: "Update a.md",
				Changes: []gh.FileChange{{Path: "a.md", Content: []byte("new")}},
			})
			checkCommitError(t, err, testCase.want)
		})
	}
}

func checkCommitError(tb testing.TB, err, want error) {
	tb.Helper()
	if wantSecret, ok := errors.AsType[*gh.SecretError](want); ok {
		got, ok := errors.AsType[*gh.SecretError](err)
		if !ok {
			tb.Fatalf("got %v, want a SecretError", err)
		}
		if diff := cmp.Diff(wantSecret, got); diff != "" {
			tb.Errorf("secret error (-want +got):\n%s", diff)
		}
		return
	}
	if wantLimit, ok := errors.AsType[*gh.RateLimitError](want); ok {
		got, ok := errors.AsType[*gh.RateLimitError](err)
		if !ok {
			tb.Fatalf("got %v, want a RateLimitError", err)
		}
		if diff := cmp.Diff(wantLimit, got); diff != "" {
			tb.Errorf("rate limit error (-want +got):\n%s", diff)
		}
		return
	}
	if !errors.Is(want, errUnknown) {
		if !errors.Is(err, want) {
			tb.Errorf("got %v, want %v", err, want)
		}
		return
	}
	if err == nil {
		tb.Fatal("got no error, want one with an unknown outcome")
	}
	for _, known := range []error{gh.ErrRefused, gh.ErrForbidden, gh.ErrBranchRules, gh.ErrHeadMoved} {
		if errors.Is(err, known) {
			tb.Errorf("got %v, want an error with an unknown outcome", err)
		}
	}
}

func TestCommits(t *testing.T) {
	t.Parallel()
	history := []gh.Commit{
		{SHA: "c4", Author: "octocat", Date: time.Date(2026, 10, 9, 14, 2, 11, 0, time.UTC), Message: "Update a.md"},
		{SHA: "c3", Author: "octocat", Date: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC), Message: "Fix a.md"},
		{SHA: "c2", Author: "octocat", Date: time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC), Message: "Edit a.md"},
		{SHA: "c1", Author: "Someone", Date: time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC), Message: "Create a.md"}, // no account
	}
	commits := "GET /repos/" + kb.FullName + "/commits?"
	testCases := []struct {
		name         string
		token        string
		repo         string
		from         string
		skip         int
		pageSize     int // the fake's; 0 for its default
		want         []gh.Commit
		wantErr      error
		wantRequests []string
	}{
		{
			name:         "first page",
			token:        writer,
			repo:         kb.FullName,
			from:         "main",
			want:         history[:2],
			wantRequests: []string{commits + "page=1&path=a.md&per_page=2&sha=main"},
		},
		{
			name:         "skip a whole page",
			token:        writer,
			repo:         kb.FullName,
			from:         "c4",
			skip:         2,
			want:         history[2:],
			wantRequests: []string{commits + "page=2&path=a.md&per_page=2&sha=c4"},
		},
		{
			name:         "skip part of a page",
			token:        writer,
			repo:         kb.FullName,
			from:         "c4",
			skip:         1,
			pageSize:     2,
			want:         history[1:3],
			wantRequests: []string{commits + "page=1&path=a.md&per_page=100&sha=c4", commits + "page=2&path=a.md&per_page=100&sha=c4"},
		},
		{
			name:         "past the end",
			token:        writer,
			repo:         kb.FullName,
			from:         "c3",
			skip:         2,
			want:         history[3:],
			wantRequests: []string{commits + "page=2&path=a.md&per_page=2&sha=c3"},
		},
		{
			name:         "token revoked",
			token:        revoked,
			repo:         kb.FullName,
			from:         "main",
			wantErr:      gh.ErrUnauthorized,
			wantRequests: []string{commits + "page=1&path=a.md&per_page=2&sha=main"},
		},
		{
			name:         "repository not seen",
			token:        writer,
			repo:         "ours/gone",
			from:         "main",
			wantErr:      gh.ErrNotFound,
			wantRequests: []string{"GET /repos/ours/gone/commits?page=1&path=a.md&per_page=2&sha=main"},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			opts := []ghtest.Option{withKB(), withUsers(), ghtest.ExpectCommits(kb.FullName, history...)}
			if testCase.pageSize != 0 {
				opts = append(opts, ghtest.ExpectPageSize(testCase.pageSize))
			}
			fake := ghtest.New(t, opts...)
			got, err := fake.Commits(t.Context(), testCase.token, testCase.repo, testCase.from, "a.md", testCase.skip, 2)
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("got %v, want %v", err, testCase.wantErr)
			}
			if diff := cmp.Diff(testCase.want, got); diff != "" {
				t.Errorf("commits (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(testCase.wantRequests, fake.Requests()); diff != "" {
				t.Errorf("requests (-want +got):\n%s", diff)
			}
		})
	}
}

func TestBranchTip(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		token   string
		branch  string
		want    string
		wantErr error
	}{
		{name: "head", token: reader, branch: "main", want: "abc123"},
		{name: "token revoked", token: revoked, branch: "main", wantErr: gh.ErrUnauthorized},
		{name: "no such branch", token: reader, branch: "gone", wantErr: gh.ErrNotFound},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := ghtest.New(t, withKB(), withUsers())
			got, err := fake.BranchTip(t.Context(), testCase.token, kb.FullName, testCase.branch)
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("got %v, want %v", err, testCase.wantErr)
			}
			if got != testCase.want {
				t.Errorf("got %q, want %q", got, testCase.want)
			}
		})
	}
}
