package library_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/xaroth/speki/library"
	"github.com/xaroth/speki/vault"
)

// A branch change whose clone fails keeps serving the old copy, shows the failure, and leaves no
// half-cloned copy behind.
func TestBranchChangeCloneFails(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name string
		// breakRemote runs after the new branch is checked out on the remote.
		breakRemote func(tb testing.TB, r *remote)
	}{
		{
			name: "unreachable remote",
			breakRemote: func(tb testing.TB, r *remote) {
				tb.Helper()
				err := os.Rename(r.dir, r.dir+".moved")
				if err != nil {
					tb.Fatal(err)
				}
			},
		},
		{
			name: "fails part way",
			breakRemote: func(tb testing.TB, r *remote) {
				tb.Helper()
				const content = "# A\n\nlost text\n"
				r.push(tb, map[string]string{"a.md": content})
				h := plumbing.ComputeHash(plumbing.BlobObject, []byte(content)).String()
				err := os.Remove(filepath.Join(r.dir, ".git", "objects", h[:2], h[2:]))
				if err != nil {
					tb.Fatal(err)
				}
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			r := newRemote(t)
			r.push(t, map[string]string{"a.md": "# A\n\nmaster text\n"})
			dataDir := t.TempDir()
			l := newLibrary(t, r, library.Options{DataDir: dataDir})
			v := serving(t, l)

			r.checkout(t, "trunk")
			testCase.breakRemote(t, r)
			moved := kb
			moved.DefaultBranch = "trunk"
			l.Update(t.Context(), vault.Changes{Changed: []vault.Vault{moved}})
			l.WaitIdle()

			if got := v.Status().Error; got == "" {
				t.Error("got no error on the status page, want the failed clone")
			}
			if got := search(t, v, "master"); len(got) != 1 {
				t.Errorf("got %v, want one hit from the old copy", got)
			}
			_, err := os.Stat(gitDir(dataDir) + ".next")
			if !errors.Is(err, os.ErrNotExist) {
				t.Errorf("got %v for git.next, want it gone", err)
			}
		})
	}
}
