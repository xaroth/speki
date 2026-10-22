package library_test

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xaroth/speki/library"
	"github.com/xaroth/speki/library/gitsync"
	"github.com/xaroth/speki/vault"
)

// A vault busy with a long sync doesn't hold up a poll round.
func TestPollSkipsABusyVault(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	r.push(t, map[string]string{"a.md": "# A\n"})
	l := newLibrary(t, r, library.Options{})
	v := serving(t, l)

	held, release := r.hold(t)
	var syncing sync.WaitGroup
	syncing.Go(func() { _ = l.Sync(t.Context(), v) })
	<-held
	r.push(t, map[string]string{"a.md": "# A\n\nchanged\n"})
	done := make(chan struct{})
	go func() {
		l.Poll(t.Context())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second): // a guard: done comes at once unless the round waits
		t.Error("the poll round waited for a busy vault")
	}
	release()
	<-done
	syncing.Wait()
}

// Work a poll round starts in the background finishes after the round: here a full rebuild.
func TestPollRebuildsInTheBackground(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	r.push(t, map[string]string{"a.md": "# A\n"})
	l := newLibrary(t, r, library.Options{})
	v := serving(t, l)
	// An index at a commit the copy doesn't have can only be rebuilt.
	err := v.Index().Apply(t.Context(), strings.Repeat("0", 40), nil)
	if err != nil {
		t.Fatal(err)
	}
	head := r.push(t, map[string]string{"a.md": "# A\n\nrebuilt\n"})
	l.Poll(t.Context())
	l.WaitIdle()
	if got := v.Index().Commit(); got != head {
		t.Errorf("got index at %s after the background rebuild, want %s", got, head)
	}
	if got := search(t, v, "rebuilt"); len(got) != 1 {
		t.Errorf("got %v, want one hit", got)
	}
}

// On the instance that writes, each write's quick sync adds a pack without compacting: the poller
// compacts once its head check finds nothing new. A fresh clone that lost the index's commit to a push
// during the clone is thrown away and tried again later, so the index needn't be rebuilt.
func TestPollerCompactsPacks(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	r.push(t, map[string]string{"a.md": "# A\n"})
	dataDir := t.TempDir()
	l := newLibrary(t, r, library.Options{DataDir: dataDir})
	l.SetMaxPacks(2)
	clock := newClock(l)
	v := serving(t, l)
	for i := range 3 {
		r.push(t, map[string]string{"a.md": "# A\n\nwrite " + strings.Repeat("x", i) + "\n"})
		err := l.SyncChanges(t.Context(), v)
		if err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}
	if got := packs(t, dataDir); got <= 2 {
		t.Fatalf("got %d packs, want more than 2", got)
	}
	poll := func() {
		l.Poll(t.Context())
		l.WaitIdle() // compacting runs in the background
	}

	r.pushAfterHead(t, map[string]string{"a.md": "# A\n\nduring the clone\n"})
	poll()
	if got := packs(t, dataDir); got <= 2 {
		t.Fatalf("got %d packs, want the spoiled fresh clone thrown away", got)
	}
	doc, err := v.Index().Document(t.Context(), "a.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Blob(doc.Version); err != nil {
		t.Fatalf("got %v reading the index's version, want the copy to keep it", err)
	}

	poll() // applies the push
	clock.advance(library.MovedOnRetry - time.Second)
	poll()
	if got := packs(t, dataDir); got <= 2 {
		t.Fatalf("got %d packs: cloned again before %v", got, library.MovedOnRetry)
	}
	clock.advance(2 * time.Second)
	poll()
	if got := packs(t, dataDir); got != 1 {
		t.Errorf("got %d packs after polling, want 1", got)
	}
	if got, want := v.Index().Commit(), r.head(t); got != want {
		t.Errorf("got index at %s, want %s", got, want)
	}
}

// A read that finds the git copy missing objects gets it cloned again at the next poll.
func TestBrokenCopyFoundByAReadIsCloned(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	r.push(t, map[string]string{"a.md": "# A\n"})
	dataDir := t.TempDir()
	l := newLibrary(t, r, library.Options{DataDir: dataDir})
	v := serving(t, l)
	doc, err := v.Index().Document(t.Context(), "a.md")
	if err != nil {
		t.Fatal(err)
	}
	removeObjects(t, dataDir)
	_, err = v.Blob(doc.Version)
	if !errors.Is(err, gitsync.ErrBroken) {
		t.Fatalf("got %v reading a copy without objects, want %v", err, gitsync.ErrBroken)
	}
	l.Poll(t.Context())
	l.WaitIdle()
	content, err := v.Blob(doc.Version)
	if err != nil || string(content) != "# A\n" {
		t.Errorf("got %q, %v after the poll, want %q", content, err, "# A\n")
	}
}

// A failing head check shows on the status page and backs off; a good one after it clears the failure.
func TestHeadCheckFailure(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	r.push(t, map[string]string{"a.md": "# A\n"})
	l := newLibrary(t, r, library.Options{})
	clock := newClock(l)
	v := serving(t, l)

	r.setHeadErr(errors.New("401 Bad credentials"))
	l.Poll(t.Context())
	if got := v.Status().Error; got == "" {
		t.Error("got no error on the status page, want the failed check")
	}
	lookups := r.lookupCount()
	l.Poll(t.Context())
	if got := r.lookupCount(); got != lookups {
		t.Errorf("got %d lookups, want %d: checked again while backing off", got, lookups)
	}

	r.setHeadErr(nil)
	clock.advance(time.Hour)
	l.Poll(t.Context())
	if got := v.Status().Error; got != "" {
		t.Errorf("got error %q after a good check, want none", got)
	}
}

// A vault that failed to open is tried again once its backoff is over, and recovers once its repository
// can be cloned.
func TestFailedVaultIsRetried(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	r.push(t, map[string]string{"a.md": "# A\n"})
	var clones atomic.Int32
	l := newLibrary(t, r, library.Options{Tokens: func(int64) gitsync.TokenSource {
		clones.Add(1)
		return nil
	}})
	clock := newClock(l)
	late := kb
	late.Name = "ours/late"
	l.Update(t.Context(), vault.Changes{Added: []vault.Vault{late}})
	l.WaitIdle()
	if _, ok := l.Vault(late.RepoID); ok {
		t.Fatal("served a vault whose repository doesn't exist yet")
	}
	poll := func(after time.Duration) {
		clock.advance(after)
		l.Poll(t.Context())
		l.WaitIdle()
	}

	poll(2*time.Minute - time.Second)
	if got := clones.Load(); got != 1 {
		t.Errorf("got %d clones before the backoff is over, want 1", got)
	}
	poll(2 * time.Second)
	if got := clones.Load(); got != 2 {
		t.Errorf("got %d clones after the backoff, want 2", got)
	}

	openRemote(t, r.root, filepath.Join(r.root, late.Name+".git"), r.dir)
	r.setID(late.Name, late.RepoID)
	poll(time.Hour)
	if _, ok := l.Vault(late.RepoID); !ok {
		t.Errorf("not serving after recovery: %+v", l.Status())
	}
}
