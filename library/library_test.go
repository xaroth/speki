package library_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/xaroth/speki/library"
	"github.com/xaroth/speki/library/index"
	"github.com/xaroth/speki/vault"
)

func TestAddSyncAndRemove(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	r.push(t, map[string]string{"fleet.md": "# Fleet\n\nzeppelin notes\n", "speki.yaml": "title: kb\n"})
	dataDir := t.TempDir()
	l := newLibrary(t, r, library.Options{DataDir: dataDir})

	v := serving(t, l)
	if got := search(t, v, "zeppelin"); len(got) != 1 {
		t.Fatalf("got %v after the clone, want one hit", got)
	}
	want := library.Status{RepoID: kb.RepoID, Name: kb.Name, State: library.StateServing, Commit: r.head(t), Documents: 1, Bytes: 24}
	got := v.Status()
	if diff := cmp.Diff(want, got, cmpopts.IgnoreFields(library.Status{}, "LastSync", "Fresh")); diff != "" {
		t.Errorf("status (-want +got):\n%s", diff)
	}
	if got.Fresh.IsZero() || got.LastSync.IsZero() {
		t.Errorf("got fresh %v, last sync %v, want both set", got.Fresh, got.LastSync)
	}

	// A push shows up after a poll.
	r.push(t, map[string]string{"fleet.md": "# Fleet\n\nairship notes\n", "new.md": "# New\n\nairship too\n"})
	l.Poll(t.Context())
	if got := search(t, v, "airship"); len(got) != 2 {
		t.Errorf("got %v after the push, want two hits", got)
	}
	if got := search(t, v, "zeppelin"); len(got) != 0 {
		t.Errorf("got %v for the old text, want none", got)
	}

	// Nothing new: the lookup is answered from the ETag and nothing is synced.
	before := v.Status().LastSync
	l.Poll(t.Context())
	if got := v.Status().LastSync; !got.Equal(before) {
		t.Errorf("got last sync %v, want %v: synced although nothing changed", got, before)
	}

	l.Update(t.Context(), vault.Changes{Removed: []vault.Vault{kb}})
	l.WaitIdle()
	if _, ok := l.Vault(kb.RepoID); ok {
		t.Error("removed vault still served")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "100")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("got %v for the removed vault's files, want them gone", err)
	}
}

func TestUpdateAnnouncesNewVaults(t *testing.T) {
	t.Parallel()
	missing := kb
	missing.Name = "ours/missing"
	testCases := []struct {
		name                    string
		vault                   vault.Vault
		wantState               library.State
		wantServing, wantFailed []vault.Vault
		wantError               bool
	}{
		{name: "served", vault: kb, wantState: library.StateServing, wantServing: []vault.Vault{kb}},
		{name: "clone fails", vault: missing, wantState: library.StateFailed, wantFailed: []vault.Vault{missing}, wantError: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			r := newRemote(t)
			r.push(t, map[string]string{"a.md": "# A\n"})
			l := newLibrary(t, r, library.Options{})
			changed := l.Changed()
			l.Update(t.Context(), vault.Changes{Added: []vault.Vault{testCase.vault}})
			for {
				st := l.Status()
				if len(st) == 1 && st[0].State == testCase.wantState {
					break
				}
				select {
				case <-changed:
				case <-time.After(time.Minute):
					t.Fatalf("got status %+v and no change announced, want state %s", st, testCase.wantState)
				}
				changed = l.Changed()
			}
			l.WaitIdle()

			serving, building, failed := l.Vaults()
			if diff := cmp.Diff([][]vault.Vault{testCase.wantServing, nil, testCase.wantFailed}, [][]vault.Vault{serving, building, failed}); diff != "" {
				t.Errorf("Vaults (-want +got):\n%s", diff)
			}
			if got := l.Status()[0].Error; (got != "") != testCase.wantError {
				t.Errorf("got error %q, want one: %v", got, testCase.wantError)
			}
			if _, ok := l.Vault(testCase.vault.RepoID); ok != (testCase.wantState == library.StateServing) {
				t.Errorf("got served %v, want %v", ok, !ok)
			}

			changed = l.Changed()
			l.Update(t.Context(), vault.Changes{})
			select {
			case <-changed:
				t.Error("an update that changed nothing woke waiters")
			default:
			}
		})
	}
}

// A restart serves the vaults whose git copy and index are usable, without asking GitHub.
func TestRestore(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name       string
		damage     func(dataDir string) error
		wantServed bool
	}{
		{name: "usable copy", wantServed: true},
		{name: "copy needs the network", damage: func(dataDir string) error { return os.RemoveAll(gitDir(dataDir)) }},
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
			if testCase.damage != nil {
				err := testCase.damage(dataDir)
				if err != nil {
					t.Fatal(err)
				}
			}
			err := os.RemoveAll(r.root) // GitHub is gone
			if err != nil {
				t.Fatal(err)
			}

			l := newLibrary(t, r, library.Options{DataDir: dataDir})
			err = l.Restore(t.Context())
			if err != nil {
				t.Fatalf("Restore: %v", err)
			}
			v, ok := l.Vault(kb.RepoID)
			if ok != testCase.wantServed {
				t.Fatalf("got served %v, want %v; status %+v", ok, testCase.wantServed, l.Status())
			}
			if !ok {
				return
			}
			if got := v.Index().Commit(); got != head {
				t.Errorf("got index at %s, want %s", got, head)
			}
			if got := search(t, v, "zeppelin"); len(got) != 1 {
				t.Errorf("got %v, want one hit", got)
			}
			if diff := cmp.Diff(kb, v.Info()); diff != "" {
				t.Errorf("restored info (-want +got):\n%s", diff)
			}
		})
	}
}

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

func TestReconcileDropsVaultsGoneFromGitHub(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	r.push(t, map[string]string{"a.md": "# A\n"})
	dataDir := t.TempDir()
	first := library.New(r, library.Options{DataDir: dataDir, GitURL: r.root}, discard())
	serving(t, first)
	first.Close()

	l := newLibrary(t, r, library.Options{DataDir: dataDir})
	err := l.Restore(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	l.Reconcile(t.Context(), nil) // discovery found no vaults at all
	l.WaitIdle()
	if got := l.Status(); len(got) != 0 {
		t.Errorf("got status %+v, want the restored vault dropped", got)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "100")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("got %v for its files, want them gone", err)
	}
}

func TestBranchChangeReclones(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	r.push(t, map[string]string{"a.md": "# A\n\nmaster text\n"})
	l := newLibrary(t, r, library.Options{})
	v := serving(t, l)

	r.checkout(t, "trunk")
	r.push(t, map[string]string{"a.md": "# A\n\ntrunk text\n"})
	moved := kb
	moved.DefaultBranch = "trunk"
	l.Update(t.Context(), vault.Changes{Changed: []vault.Vault{moved}})
	l.WaitIdle()

	if got := search(t, v, "trunk"); len(got) != 1 {
		t.Errorf("got %v after the branch change, want one hit; status %+v", got, v.Status())
	}
}

// A branch change on a vault that never opened goes the normal way in, without holding an opening slot
// it would then wait for itself.
func TestRecloneOfANeverOpenedVault(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	r.push(t, map[string]string{"a.md": "# A\n"})
	l := newLibrary(t, r, library.Options{Opening: 1})
	missing := kb
	missing.Name = "ours/missing"
	l.Update(t.Context(), vault.Changes{Added: []vault.Vault{missing}})
	l.WaitIdle()
	moved := missing
	moved.DefaultBranch = "trunk"
	l.Update(t.Context(), vault.Changes{Changed: []vault.Vault{moved}})
	done := make(chan struct{})
	go func() {
		l.WaitIdle()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second): // a guard: done comes at once unless it deadlocked
		t.Fatal("reclone of a never-opened vault deadlocked")
	}
}

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

func TestRemoveWhilePollingAndOpening(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	r.push(t, map[string]string{"a.md": "# A\n"})
	l := newLibrary(t, r, library.Options{})
	v := serving(t, l)

	var polling sync.WaitGroup
	polling.Go(func() {
		for i := range 20 {
			_, err := r.commit(map[string]string{"a.md": "# A\n\n" + strings.Repeat("x", i) + "\n"})
			if err != nil {
				t.Error(err)
				return
			}
			l.Poll(t.Context())
		}
	})
	l.Update(t.Context(), vault.Changes{Removed: []vault.Vault{kb}})
	polling.Wait()
	l.WaitIdle()

	_, err := index.Search(t.Context(), []index.Source{{Name: "kb", Index: v.Index()}}, "a", index.Options{})
	if !errors.Is(err, index.ErrClosed) {
		t.Errorf("got %v searching the removed vault, want %v", err, index.ErrClosed)
	}

	// Removing a vault that is still being opened leaves nothing behind.
	l.Update(t.Context(), vault.Changes{Added: []vault.Vault{kb}})
	l.Update(t.Context(), vault.Changes{Removed: []vault.Vault{kb}})
	l.WaitIdle()
	if got := l.Status(); len(got) != 0 {
		t.Errorf("got status %+v after add and remove, want none", got)
	}
}
