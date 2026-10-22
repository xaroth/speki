package library_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/xaroth/speki/library"
	"github.com/xaroth/speki/library/gitsync"
	"github.com/xaroth/speki/vault"
)

// A repository renamed and moved to another installation keeps its vault (the repository ID), and the
// copy follows: it fetches from the new name with the new installation's credentials, even after a new
// repository took the old name.
func TestRenamedRepositoryIsFollowed(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	r.push(t, map[string]string{"a.md": "# A\n\noriginal text\n"})
	var lastInstallation atomic.Int64
	l := newLibrary(t, r, library.Options{Tokens: func(id int64) gitsync.TokenSource {
		lastInstallation.Store(id)
		return nil // local remotes need no credentials
	}})
	v := serving(t, l)

	// The repository is renamed: its history now lives under the new name.
	moved := openRemote(t, r.root, filepath.Join(r.root, "ours/kb-archive.git"), r.dir)
	moved.push(t, map[string]string{"a.md": "# A\n\nrenamed text\n"})
	// Someone creates a new repository with the old name.
	r.push(t, map[string]string{"a.md": "# A\n\nimposter text\n"})
	r.setID("ours/kb-archive", kb.RepoID)
	r.setID(kb.Name, 999)

	// Before discovery learns the new name, the vault won't fetch what its old name points at now.
	err := l.Sync(t.Context(), v)
	if err == nil {
		t.Error("got no error syncing from a name that points at another repository")
	}
	if got := search(t, v, "imposter"); len(got) != 0 {
		t.Errorf("got %v, want the vault not to serve the repository that took its old name", got)
	}

	renamed := kb
	renamed.Name, renamed.InstallationID = "ours/kb-archive", 2
	l.Update(t.Context(), vault.Changes{Changed: []vault.Vault{renamed}})
	err = l.Sync(t.Context(), v)
	if err != nil {
		t.Fatal(err)
	}
	if got := search(t, v, "imposter"); len(got) != 0 {
		t.Errorf("got %v, want the vault not to serve the repository that took its old name", got)
	}
	if got := search(t, v, "renamed"); len(got) != 1 {
		t.Errorf("got %v, want one hit from the renamed repository", got)
	}
	if got := lastInstallation.Load(); got != 2 {
		t.Errorf("got credentials from installation %d, want 2", got)
	}
}

// A sync for a waiting request stops waiting for the vault's lock when the request's time is up.
func TestSyncChangesHonoursTheContext(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	r.push(t, map[string]string{"a.md": "# A\n"})
	l := newLibrary(t, r, library.Options{})
	v := serving(t, l)

	held, release := r.hold(t)
	var syncing sync.WaitGroup
	syncing.Go(func() { _ = l.Sync(t.Context(), v) }) // a long sync holds the vault's lock
	<-held
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := l.SyncChanges(ctx, v)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, want %v", err, context.Canceled)
	}
	release()
	syncing.Wait()
}

// A sync replaces a copy with too many pack files by a fresh clone.
func TestManyPacksGetAFreshClone(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	r.push(t, map[string]string{"a.md": "# A\n"})
	dataDir := t.TempDir()
	l := newLibrary(t, r, library.Options{DataDir: dataDir})
	l.SetMaxPacks(2)
	v := serving(t, l)

	for i := range 2 { // the second fetch makes the third pack
		r.push(t, map[string]string{"a.md": "# A\n\nversion " + strings.Repeat("x", i) + "\n"})
		err := l.Sync(t.Context(), v)
		if err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}
	if got := packs(t, dataDir); got != 1 {
		t.Errorf("got %d pack files, want 1 after a fresh clone", got)
	}
	if got, want := v.Index().Commit(), r.head(t); got != want {
		t.Errorf("got index at %s after the fresh clone, want %s", got, want)
	}
	doc, err := v.Index().Document(t.Context(), "a.md")
	if err != nil {
		t.Fatal(err)
	}
	content, err := v.Blob(doc.Version)
	if err != nil || !strings.Contains(string(content), "version") {
		t.Errorf("got %q, %v reading from the fresh copy, want the latest version", content, err)
	}
}

// A fresh clone that can't be swapped in or reopened puts the old copy back, which keeps serving. When
// the old copy can't go back either, the next sync clones it again.
func TestFreshCloneSwapFails(t *testing.T) {
	t.Parallel()
	errRename := errors.New("rename refused")
	isMove := func(from, to, fromName, toName string) bool {
		return filepath.Base(from) == fromName && filepath.Base(to) == toName
	}
	testCases := []struct {
		name string
		// rename stands in for os.Rename during the first fresh clone.
		rename    func(from, to string) error
		wantPacks int // after the failed fresh clone; 0 means more than one
	}{
		{
			name: "swap fails",
			rename: func(from, to string) error {
				if isMove(from, to, "git.next", "git") {
					return errRename
				}
				return os.Rename(from, to)
			},
		},
		{
			name: "reopen fails",
			rename: func(from, to string) error {
				err := os.Rename(from, to)
				if err != nil || !isMove(from, to, "git.next", "git") {
					return err
				}
				return os.RemoveAll(filepath.Join(to, "objects"))
			},
		},
		{
			name: "old copy can't be moved aside",
			rename: func(from, to string) error {
				if isMove(from, to, "git", "git.old") {
					return errRename
				}
				return os.Rename(from, to)
			},
		},
		{
			name: "old copy can't go back",
			rename: func(from, to string) error {
				if isMove(from, to, "git.next", "git") || isMove(from, to, "git.old", "git") {
					return errRename
				}
				return os.Rename(from, to)
			},
			wantPacks: 1, // cloned again by the next sync
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			r := newRemote(t)
			r.push(t, map[string]string{"a.md": "# A\n"})
			dataDir := t.TempDir()
			l := newLibrary(t, r, library.Options{DataDir: dataDir})
			l.SetMaxPacks(2)
			var failing atomic.Bool
			failing.Store(true)
			l.SetRename(func(from, to string) error {
				if failing.Load() {
					return testCase.rename(from, to)
				}
				return os.Rename(from, to)
			})
			v := serving(t, l)

			for i := range 2 { // the second sync tries a fresh clone
				r.push(t, map[string]string{"a.md": "# A\n\nversion " + strings.Repeat("x", i) + "\n"})
				err := l.Sync(t.Context(), v)
				if err != nil {
					t.Fatalf("sync %d: %v", i, err)
				}
			}
			failing.Store(false)
			if testCase.wantPacks != 0 {
				err := l.Sync(t.Context(), v)
				if err != nil {
					t.Fatalf("sync after the failed fresh clone: %v", err)
				}
			}

			got := packs(t, dataDir)
			if testCase.wantPacks == 0 && got <= 1 {
				t.Errorf("got %d pack files, want the old copy's", got)
			}
			if testCase.wantPacks != 0 && got != testCase.wantPacks {
				t.Errorf("got %d pack files, want %d", got, testCase.wantPacks)
			}
			for _, leftover := range []string{".next", ".old"} {
				_, err := os.Stat(gitDir(dataDir) + leftover)
				if !errors.Is(err, os.ErrNotExist) {
					t.Errorf("got %v for git%s, want it gone", err, leftover)
				}
			}
			if got, want := v.Index().Commit(), r.head(t); got != want {
				t.Errorf("got index at %s, want %s", got, want)
			}
			doc, err := v.Index().Document(t.Context(), "a.md")
			if err != nil {
				t.Fatal(err)
			}
			content, err := v.Blob(doc.Version)
			if err != nil || !strings.Contains(string(content), "version x") {
				t.Errorf("got %q, %v reading the latest version, want it", content, err)
			}
		})
	}
}

// A copy that lost its objects while in use still catches up at the next sync.
func TestCopyWithoutObjectsCatchesUp(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	r.push(t, map[string]string{"a.md": "# A\n\nfirst\n"})
	dataDir := t.TempDir()
	l := newLibrary(t, r, library.Options{DataDir: dataDir})
	v := serving(t, l)

	removeObjects(t, dataDir)
	r.push(t, map[string]string{"a.md": "# A\n\nsecond\n"})
	err := l.Sync(t.Context(), v)
	if err != nil {
		t.Fatalf("got %v syncing a copy without its objects", err)
	}
	if got := search(t, v, "second"); len(got) != 1 {
		t.Errorf("got %v after the sync, want one hit", got)
	}
}
