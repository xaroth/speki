package library

// Internal: these set a vault's state directly. Reaching it through the library takes failures SQLite and
// git can't be made to produce on demand.

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/mattn/go-sqlite3"

	"github.com/eve-online-tools/speki/library/gitsync"
	"github.com/eve-online-tools/speki/library/index"
	"github.com/eve-online-tools/speki/vault"
)

var testVault = vault.Vault{RepoID: 100, Name: "ours/kb", InstallationID: 1, DefaultBranch: "master"}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// A failure backs off longer with every one, up to half an hour. Running into SQLite's memory limit,
// which other work can cause, doesn't: the vault is tried again a minute later.
func TestRecordFailureBacksOff(t *testing.T) {
	t.Parallel()
	errFetch := errors.New("fetch failed")
	errNomem := fmt.Errorf("apply: %w", sqlite3.Error{Code: sqlite3.ErrNomem})
	testCases := []struct {
		name     string
		err      error
		failures int
		want     time.Duration
	}{
		{name: "first failure", err: errFetch, failures: 1, want: 2 * time.Minute},
		{name: "third failure", err: errFetch, failures: 3, want: 8 * time.Minute},
		{name: "capped", err: errFetch, failures: 6, want: 30 * time.Minute},
		{name: "out of memory", err: errNomem, failures: 5, want: time.Minute},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			now := time.Now()
			v := newVault(testVault, t.TempDir(), func() time.Time { return now })
			for range testCase.failures {
				v.recordFailure(testCase.err)
			}
			if got := v.Status().Error; got != testCase.err.Error() {
				t.Errorf("got error %q, want %q", got, testCase.err.Error())
			}
			now = now.Add(testCase.want - time.Second)
			if v.due() {
				t.Errorf("got due before %v, want not due", testCase.want)
			}
			now = now.Add(2 * time.Second)
			if !v.due() {
				t.Errorf("got not due after %v, want due", testCase.want)
			}
			v.recordFailure(nil)
			if got := v.Status().Error; got != "" {
				t.Errorf("got error %q after a success, want none", got)
			}
		})
	}
}

// Only a copy at the index's commit must have every blob the index names. Just after a fresh clone,
// before the rebuild, older blobs are missing for good reason, and don't mark the copy broken.
func TestBlobMarksBrokenCopies(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name        string
		indexAtHead bool
		wantBroken  bool
	}{
		{name: "index at the copy's head", indexAtHead: true, wantBroken: true},
		{name: "index behind the copy", indexAtHead: false, wantBroken: false},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			v, head := openTestVault(t)
			commit := strings.Repeat("0", 40)
			if testCase.indexAtHead {
				commit = head
			}
			err := v.index.Apply(t.Context(), commit, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = v.Blob(strings.Repeat("1", 40))
			if !errors.Is(err, gitsync.ErrBroken) {
				t.Fatalf("got %v for a missing blob, want %v", err, gitsync.ErrBroken)
			}
			if got := v.broken.Load(); got != testCase.wantBroken {
				t.Errorf("got broken %v, want %v", got, testCase.wantBroken)
			}
		})
	}
}

// openTestVault returns a vault with a git copy of a one-commit repository and an empty index, and the
// commit.
func openTestVault(tb testing.TB) (*Vault, string) {
	tb.Helper()
	src := tb.TempDir()
	repo, err := git.PlainInit(src, false)
	if err != nil {
		tb.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		tb.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(src, "a.md"), []byte("# A\n"), 0o644)
	if err != nil {
		tb.Fatal(err)
	}
	_, err = wt.Add("a.md")
	if err != nil {
		tb.Fatal(err)
	}
	h, err := wt.Commit("push", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@x", When: time.Now()}})
	if err != nil {
		tb.Fatal(err)
	}

	dir := tb.TempDir()
	m, err := gitsync.Open(tb.Context(), filepath.Join(dir, gitDir), gitsync.Source{URL: src, Branch: testVault.DefaultBranch})
	if err != nil {
		tb.Fatal(err)
	}
	idx, err := index.Open(tb.Context(), filepath.Join(dir, indexDir), discard())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(idx.Close)
	v := newVault(testVault, dir, time.Now)
	v.mirror, v.index = m, idx
	return v, h.String()
}

// A vault changing state while Vaults runs is in one of its lists, never in none: callers tell "not
// served yet" from "unknown" by it.
func TestVaultsListsEveryVaultOnce(t *testing.T) {
	t.Parallel()
	l := New(nil, Options{DataDir: t.TempDir()}, discard())
	v := newVault(testVault, t.TempDir(), time.Now)
	l.vaults[testVault.RepoID] = v

	stop := make(chan struct{})
	var flipping sync.WaitGroup
	flipping.Go(func() {
		states := []State{StateBuilding, StateServing, StateFailed}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			v.mu.Lock()
			v.status.State = states[i%len(states)]
			v.mu.Unlock()
		}
	})
	defer flipping.Wait()
	defer close(stop)
	for range 1000 {
		serving, building, failed := l.Vaults()
		if n := len(serving) + len(building) + len(failed); n != 1 {
			t.Fatalf("got the vault listed %d times, want once", n)
		}
	}
}
