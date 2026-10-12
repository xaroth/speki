package gh_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/eve-online-tools/speki/gh"
)

func TestFakeApply(t *testing.T) {
	t.Parallel()
	fake := gh.NewFake(t, withKB())
	fake.Apply(gh.ExpectBranch(kb.FullName, "main", "def456"))
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
	var got []gh.FakeCommit
	fake := gh.NewFake(t, withKB(), withUsers(), gh.ExpectCommitHook(kb.FullName, func(c gh.FakeCommit) string {
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
	want := []gh.FakeCommit{{
		Repo: kb.FullName, Branch: "main", Author: "octocat", Message: "Update a.md\n\nbody",
		Changes: []gh.FileChange{{Path: "old.md"}, {Path: "a.md", Content: []byte("new")}},
	}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("hook calls (-want +got):\n%s", diff)
	}
}
