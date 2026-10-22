package library_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/xaroth/speki/library"
	"github.com/xaroth/speki/vault"
)

// Restore opens many vaults a few at a time and serves each one.
func TestRestoreParallel(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	head := r.push(t, map[string]string{"fleet.md": "# Fleet\n\nzeppelin notes\n"})
	var vaults []vault.Vault
	for i := range 9 {
		v := kb
		v.RepoID = int64(200 + i)
		v.Name = fmt.Sprintf("ours/kb%d", i)
		openRemote(t, r.root, filepath.Join(r.root, v.Name+".git"), r.dir)
		r.setID(v.Name, v.RepoID)
		vaults = append(vaults, v)
	}
	dataDir := t.TempDir()
	first := library.New(r, library.Options{DataDir: dataDir, GitURL: r.root}, discard())
	first.Update(t.Context(), vault.Changes{Added: vaults})
	first.WaitIdle()
	first.Close()
	err := os.RemoveAll(r.root) // GitHub is gone
	if err != nil {
		t.Fatal(err)
	}

	l := newLibrary(t, r, library.Options{DataDir: dataDir, Opening: 2})
	err = l.Restore(t.Context())
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for _, want := range vaults {
		v, ok := l.Vault(want.RepoID)
		if !ok {
			t.Errorf("%s not served; status %+v", want.Name, l.Status())
			continue
		}
		if got := v.Index().Commit(); got != head {
			t.Errorf("%s: got index at %s, want %s", want.Name, got, head)
		}
		if got := search(t, v, "zeppelin"); len(got) != 1 {
			t.Errorf("%s: got %v, want one hit", want.Name, got)
		}
	}
}

// A git copy that's there but unusable isn't served or deleted by Restore, and discovery clones it again.
func TestRestoreUnusableCopy(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name   string
		damage func(tb testing.TB, dataDir string)
	}{
		{name: "objects missing", damage: removeObjects},
		{name: "branch missing", damage: func(tb testing.TB, dataDir string) {
			tb.Helper()
			for _, f := range []string{"refs/heads/master", "packed-refs"} {
				err := os.Remove(filepath.Join(gitDir(dataDir), f))
				if err != nil && !os.IsNotExist(err) {
					tb.Fatal(err)
				}
			}
		}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			r := newRemote(t)
			head := r.push(t, map[string]string{"fleet.md": "# Fleet\n\nzeppelin notes\n"})
			dataDir := t.TempDir()
			first := library.New(r, library.Options{DataDir: dataDir, GitURL: r.root}, discard())
			serving(t, first)
			first.Close()
			testCase.damage(t, dataDir)

			l := newLibrary(t, r, library.Options{DataDir: dataDir})
			err := l.Restore(t.Context())
			if err != nil {
				t.Fatalf("Restore: %v", err)
			}
			if got := l.Status(); len(got) != 0 {
				t.Fatalf("got status %+v, want nothing restored", got)
			}
			_, err = os.Stat(gitDir(dataDir))
			if err != nil {
				t.Errorf("got %v for the git copy, want it left in place", err)
			}

			v := serving(t, l)
			if got := v.Index().Commit(); got != head {
				t.Errorf("got index at %s, want %s", got, head)
			}
		})
	}
}
