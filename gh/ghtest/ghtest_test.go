package ghtest_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/xaroth/speki/gh"
	"github.com/xaroth/speki/gh/ghtest"
)

const writer = "ghu_writer"

var kb = gh.Repository{ID: 100, FullName: "ours/kb", DefaultBranch: "main"}

// withKB is installation 1 with the repository ours/kb on branch main, and octocat who may write to it.
func withKB() ghtest.Option {
	return func(tb testing.TB, f *ghtest.Fake) {
		tb.Helper()
		ghtest.ExpectInstallation(gh.Installation{ID: 1, AccountID: 42}, kb)(tb, f)
		ghtest.ExpectBranch(kb.FullName, "main", "abc123")(tb, f)
		ghtest.ExpectUser(writer, gh.User{ID: 7, Login: "octocat"})(tb, f)
		ghtest.ExpectRole(writer, kb.FullName, gh.RoleWrite)(tb, f)
	}
}

func TestFakeApply(t *testing.T) {
	t.Parallel()
	fake := ghtest.New(t, withKB())
	fake.Apply(ghtest.ExpectBranch(kb.FullName, "main", "def456"))
	got, err := fake.BranchHead(t.Context(), 1, kb.FullName, "main", `"abc123"`)
	if err != nil {
		t.Fatal(err)
	}
	if want := (gh.Head{SHA: "def456", ETag: `"def456"`}); got != want {
		t.Errorf("got head %+v, want %+v", got, want)
	}
}

func TestFakeCommitHook(t *testing.T) {
	t.Parallel()
	var got []ghtest.Commit
	fake := ghtest.New(t, withKB(), ghtest.ExpectCommitHook(kb.FullName, func(c ghtest.Commit) string {
		got = append(got, c)
		return "from-the-hook"
	}))
	sha, err := fake.Commit(t.Context(), writer, gh.CommitRequest{
		Repo: kb.FullName, Branch: "main", ExpectedHead: "abc123", Message: "Update a.md\n\nbody",
		Changes: []gh.FileChange{{Path: "a.md", Content: []byte("new")}, {Path: "old.md"}},
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if sha != "from-the-hook" || fake.BranchSHA(kb.FullName, "main") != "from-the-hook" {
		t.Errorf("got SHA %q and head %q, want the hook's", sha, fake.BranchSHA(kb.FullName, "main"))
	}
	want := []ghtest.Commit{{
		Repo: kb.FullName, Branch: "main", Author: "octocat", Message: "Update a.md\n\nbody",
		Changes: []gh.FileChange{{Path: "old.md"}, {Path: "a.md", Content: []byte("new")}},
	}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("hook calls (-want +got):\n%s", diff)
	}
}
