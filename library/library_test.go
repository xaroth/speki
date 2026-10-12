package library

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/eve-online-tools/speki/gh"
	"github.com/eve-online-tools/speki/gitsync"
	"github.com/eve-online-tools/speki/index"
	"github.com/eve-online-tools/speki/vault"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// remote is a repository under a fake GitURL, plus a fake GitHub that reports its head.
type remote struct {
	t    *testing.T
	root string // the GitURL
	dir  string
	repo *git.Repository

	mu      sync.Mutex
	lookups int
	headErr error            // when set, BranchHead fails with it
	ids     map[string]int64 // repository IDs by name; ours/kb is 100 unless set
}

// RepositoryID answers like GitHub: which repository a name points to now.
func (r *remote) RepositoryID(_ context.Context, _ int64, repo string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.ids[repo]; ok {
		return id, nil
	}
	if repo == kb.Name {
		return kb.RepoID, nil
	}
	return 0, gh.ErrNotFound
}

func (r *remote) setID(repo string, id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ids == nil {
		r.ids = map[string]int64{}
	}
	r.ids[repo] = id
}

func newRemote(t *testing.T) *remote {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, kb.Name+".git")
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	return &remote{t: t, root: root, dir: dir, repo: repo}
}

func (r *remote) push(files map[string]string) string {
	r.t.Helper()
	wt, err := r.repo.Worktree()
	if err != nil {
		r.t.Fatal(err)
	}
	for name, content := range files {
		full := filepath.Join(r.dir, name)
		if content == "" {
			_, err = wt.Remove(name)
		} else {
			err = os.MkdirAll(filepath.Dir(full), 0o755)
			if err == nil {
				err = os.WriteFile(full, []byte(content), 0o644)
			}
			if err == nil {
				_, err = wt.Add(name)
			}
		}
		if err != nil {
			r.t.Fatal(err)
		}
	}
	h, err := wt.Commit("push", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@x", When: time.Now()}})
	if err != nil {
		r.t.Fatal(err)
	}
	return h.String()
}

func (r *remote) head() string {
	ref, err := r.repo.Head()
	if err != nil {
		r.t.Fatal(err)
	}
	return ref.Hash().String()
}

// BranchHead answers like GitHub: unchanged when the ETag (the commit) matches.
func (r *remote) BranchHead(_ context.Context, _ int64, _, _, etag string) (gh.Head, error) {
	r.mu.Lock()
	r.lookups++
	headErr := r.headErr
	r.mu.Unlock()
	if headErr != nil {
		return gh.Head{}, headErr
	}
	h := r.head()
	if etag == h {
		return gh.Head{ETag: etag}, nil
	}
	return gh.Head{SHA: h, ETag: h}, nil
}

var kb = vault.Vault{RepoID: 100, Name: "ours/kb", InstallationID: 1, DefaultBranch: "master"}

func newLibrary(t *testing.T, r *remote, dataDir string) *Library {
	t.Helper()
	l := New(r, Options{DataDir: dataDir, GitURL: r.root}, discard())
	t.Cleanup(l.Close)
	return l
}

// clock is a fake clock for the library's backoff and fresh-clone times.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

// fakeClock sets l's clock to a fake one. Call it before the library has vaults.
func fakeClock(l *Library) *clock {
	c := &clock{t: time.Now()}
	l.now = c.now
	return c
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func search(t *testing.T, v *Vault, q string) []string {
	t.Helper()
	res, err := index.Search(t.Context(), []index.Source{{Name: "kb", Index: v.Index()}}, q, index.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, h := range res.Hits {
		paths = append(paths, h.Path)
	}
	return paths
}

func serving(t *testing.T, l *Library) *Vault {
	t.Helper()
	l.background.Wait()
	v, ok := l.Vault(kb.RepoID)
	if !ok {
		t.Fatalf("vault not serving: %+v", l.Status())
	}
	return v
}

func TestAddSyncAndRemove(t *testing.T) {
	r := newRemote(t)
	r.push(map[string]string{"fleet.md": "# Fleet\n\nzeppelin notes\n", "speki.yaml": "title: kb\n"})
	dataDir := t.TempDir()
	l := newLibrary(t, r, dataDir)

	l.Update(t.Context(), vault.Changes{Added: []vault.Vault{kb}})
	v := serving(t, l)
	if got := search(t, v, "zeppelin"); len(got) != 1 {
		t.Fatalf("search after clone = %v", got)
	}
	st := v.Status()
	if st.Commit != r.head() || st.Documents != 1 || st.Fresh.IsZero() || st.Large {
		t.Errorf("status = %+v", st)
	}

	// A push shows up after a poll.
	r.push(map[string]string{"fleet.md": "# Fleet\n\nairship notes\n", "new.md": "# New\n\nairship too\n"})
	l.Poll(t.Context())
	if got := search(t, v, "airship"); len(got) != 2 {
		t.Errorf("search after push = %v", got)
	}
	if got := search(t, v, "zeppelin"); len(got) != 0 {
		t.Errorf("old text still found: %v", got)
	}

	// Nothing new: the lookup is answered from the ETag and nothing is synced.
	before := v.Status().LastSync
	l.Poll(t.Context())
	if !v.Status().LastSync.Equal(before) {
		t.Error("synced although nothing changed")
	}

	l.Update(t.Context(), vault.Changes{Removed: []vault.Vault{kb}})
	l.background.Wait()
	if _, ok := l.Vault(kb.RepoID); ok {
		t.Error("removed vault still served")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "100")); !os.IsNotExist(err) {
		t.Error("removed vault's files are still there")
	}
}

func TestRestoreServesWithoutGitHub(t *testing.T) {
	r := newRemote(t)
	head := r.push(map[string]string{"fleet.md": "# Fleet\n\nzeppelin notes\n"})
	dataDir := t.TempDir()

	first := New(r, Options{DataDir: dataDir, GitURL: r.root}, discard())
	first.Update(t.Context(), vault.Changes{Added: []vault.Vault{kb}})
	first.background.Wait()
	first.Close()

	err := os.RemoveAll(r.root) // GitHub is gone
	if err != nil {
		t.Fatal(err)
	}
	l := newLibrary(t, r, dataDir)
	err = l.Restore(t.Context())
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	v, ok := l.Vault(kb.RepoID)
	if !ok {
		t.Fatalf("restored vault not served: %+v", l.Status())
	}
	if v.Index().Commit() != head || len(search(t, v, "zeppelin")) != 1 {
		t.Errorf("restored index at %s", v.Index().Commit())
	}
	if v.Info().Name != "ours/kb" {
		t.Errorf("restored info = %+v", v.Info())
	}
}

func TestFailedCloneIsReported(t *testing.T) {
	r := newRemote(t)
	r.push(map[string]string{"a.md": "# A\n"})
	l := newLibrary(t, r, t.TempDir())
	missing := kb
	missing.Name = "ours/missing"

	l.Update(t.Context(), vault.Changes{Added: []vault.Vault{missing}})
	l.background.Wait()

	if _, ok := l.Vault(kb.RepoID); ok {
		t.Fatal("a vault that failed to clone is served")
	}
	st := l.Status()
	if len(st) != 1 || st[0].State != StateFailed || st[0].Error == "" {
		t.Errorf("status = %+v", st)
	}
}

func TestChangedFollowsState(t *testing.T) {
	missing := kb
	missing.Name = "ours/missing"
	tests := []struct {
		name  string
		vault vault.Vault
		done  func(*Library) []vault.Vault
	}{
		{name: "served", vault: kb, done: func(l *Library) []vault.Vault { s, _, _ := l.Vaults(); return s }},
		{name: "failed", vault: missing, done: func(l *Library) []vault.Vault { _, _, f := l.Vaults(); return f }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRemote(t)
			r.push(map[string]string{"a.md": "# A\n"})
			l := newLibrary(t, r, t.TempDir())
			changed := l.Changed()
			l.Update(t.Context(), vault.Changes{Added: []vault.Vault{tt.vault}})
			timeout := time.After(time.Minute)
			for len(tt.done(l)) == 0 {
				select {
				case <-changed:
				case <-timeout:
					t.Fatalf("no change announced; status = %+v", l.Status())
				}
				changed = l.Changed()
			}
			l.WaitIdle()

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

// A vault changing state while Vaults runs is in one of its lists, never in none: callers tell "not
// served yet" from "unknown" by it.
func TestVaultsListsEveryVaultOnce(t *testing.T) {
	l := newLibrary(t, newRemote(t), t.TempDir())
	v := newVault(kb, t.TempDir(), time.Now)
	l.mu.Lock()
	l.vaults[kb.RepoID] = v
	l.mu.Unlock()

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
	for range 10_000 {
		serving, building, failed := l.Vaults()
		if n := len(serving) + len(building) + len(failed); n != 1 {
			t.Fatalf("vault listed %d times", n)
		}
	}
}

func TestBranchChangeReclones(t *testing.T) {
	r := newRemote(t)
	r.push(map[string]string{"a.md": "# A\n\nmaster text\n"})
	l := newLibrary(t, r, t.TempDir())
	l.Update(t.Context(), vault.Changes{Added: []vault.Vault{kb}})
	serving(t, l)

	wt, _ := r.repo.Worktree()
	err := wt.Checkout(&git.CheckoutOptions{Branch: "refs/heads/trunk", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	r.push(map[string]string{"a.md": "# A\n\ntrunk text\n"})
	moved := kb
	moved.DefaultBranch = "trunk"
	l.Update(t.Context(), vault.Changes{Changed: []vault.Vault{moved}})
	v := serving(t, l)

	if got := search(t, v, "trunk"); len(got) != 1 {
		t.Errorf("after branch change: %v (status %+v)", got, v.Status())
	}
}

func TestManyPacksGetAFreshClone(t *testing.T) {
	r := newRemote(t)
	r.push(map[string]string{"a.md": "# A\n"})
	l := newLibrary(t, r, t.TempDir())
	l.Update(t.Context(), vault.Changes{Added: []vault.Vault{kb}})
	v := serving(t, l)

	for i := range maxPacks + 1 {
		r.push(map[string]string{"a.md": "# A\n\nversion " + strings.Repeat("x", i) + "\n"})
		err := l.Sync(t.Context(), v)
		if err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}
	if n := v.mirror.Packs(); n > maxPacks {
		t.Errorf("%d pack files after a fresh clone", n)
	}
	if v.Index().Commit() != r.head() {
		t.Error("index behind after fresh clone")
	}
	doc, err := v.Index().Document(t.Context(), "a.md")
	if err != nil {
		t.Fatal(err)
	}
	content, err := v.Blob(doc.Version)
	if err != nil || !strings.Contains(string(content), "version") {
		t.Errorf("reading from the fresh copy: %q, %v", content, err)
	}
}

func TestReconcileDropsVaultsGoneFromGitHub(t *testing.T) {
	r := newRemote(t)
	r.push(map[string]string{"a.md": "# A\n"})
	dataDir := t.TempDir()
	first := New(r, Options{DataDir: dataDir, GitURL: r.root}, discard())
	first.Update(t.Context(), vault.Changes{Added: []vault.Vault{kb}})
	first.WaitIdle()
	first.Close()

	l := newLibrary(t, r, dataDir)
	err := l.Restore(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	l.Reconcile(t.Context(), nil) // discovery found no vaults at all
	l.WaitIdle()
	if len(l.Status()) != 0 {
		t.Errorf("restored vault kept: %+v", l.Status())
	}
	if _, err := os.Stat(filepath.Join(dataDir, "100")); !os.IsNotExist(err) {
		t.Error("its files are still there")
	}
}

func TestRemoveWhilePollingAndOpening(t *testing.T) {
	r := newRemote(t)
	r.push(map[string]string{"a.md": "# A\n"})
	l := newLibrary(t, r, t.TempDir())
	l.Update(t.Context(), vault.Changes{Added: []vault.Vault{kb}})
	v := serving(t, l)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 20 {
			r.push(map[string]string{"a.md": "# A\n\n" + time.Now().String() + "\n"})
			l.Poll(t.Context())
		}
	}()
	l.Update(t.Context(), vault.Changes{Removed: []vault.Vault{kb}})
	<-done
	l.WaitIdle()

	_, err := index.Search(t.Context(), []index.Source{{Name: "kb", Index: v.Index()}}, "a", index.Options{})
	if !errors.Is(err, index.ErrClosed) {
		t.Errorf("search on removed vault: %v", err)
	}

	// Removing a vault that is still being opened leaves nothing behind.
	l.Update(t.Context(), vault.Changes{Added: []vault.Vault{kb}})
	l.Update(t.Context(), vault.Changes{Removed: []vault.Vault{kb}})
	l.WaitIdle()
	if len(l.Status()) != 0 {
		t.Errorf("status after add and remove: %+v", l.Status())
	}
}

func TestStallContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, touch, stop := stallContext(t.Context(), 4*time.Minute)
		defer stop()
		for range 5 { // fifteen minutes of steady progress
			touch()
			time.Sleep(3 * time.Minute)
			if ctx.Err() != nil {
				t.Fatal("cancelled while making progress")
			}
		}
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if ctx.Err() == nil {
			t.Fatal("not cancelled after stalling")
		}
	})
}

// A repository renamed and moved to another installation keeps its vault (the repository ID), and the
// copy follows: it fetches from the new name with the new installation's credentials, even after a new
// repository took the old name.
func TestRenamedRepositoryIsFollowed(t *testing.T) {
	r := newRemote(t)
	r.push(map[string]string{"a.md": "# A\n\noriginal text\n"})
	var mu sync.Mutex
	var installations []int64
	l := New(r, Options{DataDir: t.TempDir(), GitURL: r.root, Tokens: func(id int64) gitsync.TokenSource {
		mu.Lock()
		defer mu.Unlock()
		installations = append(installations, id)
		return nil // local remotes need no credentials
	}}, discard())
	t.Cleanup(l.Close)
	l.Update(t.Context(), vault.Changes{Added: []vault.Vault{kb}})
	v := serving(t, l)

	// The repository is renamed: its history now lives under the new name.
	archive, err := git.PlainClone(filepath.Join(r.root, "ours/kb-archive.git"), false, &git.CloneOptions{URL: r.dir})
	if err != nil {
		t.Fatal(err)
	}
	moved := &remote{t: t, root: r.root, dir: filepath.Join(r.root, "ours/kb-archive.git"), repo: archive}
	moved.push(map[string]string{"a.md": "# A\n\nrenamed text\n"})
	// Someone creates a new repository with the old name.
	r.push(map[string]string{"a.md": "# A\n\nimposter text\n"})

	r.setID("ours/kb-archive", kb.RepoID)
	r.setID(kb.Name, 999)

	// Before discovery learns the new name, the vault won't fetch what its old name points at now.
	err = l.Sync(t.Context(), v)
	if err == nil {
		t.Error("synced from a name that points at another repository")
	}
	if got := search(t, v, "imposter"); len(got) != 0 {
		t.Errorf("the vault serves the repository that took its old name")
	}

	renamed := kb
	renamed.Name, renamed.InstallationID = "ours/kb-archive", 2
	l.Update(t.Context(), vault.Changes{Changed: []vault.Vault{renamed}})
	err = l.Sync(t.Context(), v)
	if err != nil {
		t.Fatal(err)
	}
	if got := search(t, v, "imposter"); len(got) != 0 {
		t.Errorf("the vault serves the repository that took its old name")
	}
	if got := search(t, v, "renamed"); len(got) != 1 {
		t.Errorf("the vault didn't follow the rename: %v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if installations[len(installations)-1] != 2 {
		t.Errorf("credentials from installations %v", installations)
	}
}

// A sync for a waiting request stops waiting for the vault's lock when the request's time is up.
func TestSyncChangesHonoursTheContext(t *testing.T) {
	r := newRemote(t)
	r.push(map[string]string{"a.md": "# A\n"})
	l := newLibrary(t, r, t.TempDir())
	l.Update(t.Context(), vault.Changes{Added: []vault.Vault{kb}})
	v := serving(t, l)

	v.syncMu.Lock() // a long sync holds it
	defer v.syncMu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err := l.SyncChanges(ctx, v)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("SyncChanges = %v, want it to give up", err)
	}
}

// A notice doesn't override a vault's backoff after failures.
func TestSyncAfterNoticeBacksOff(t *testing.T) {
	r := newRemote(t)
	r.push(map[string]string{"a.md": "# A\n"})
	l := newLibrary(t, r, t.TempDir())
	l.Update(t.Context(), vault.Changes{Added: []vault.Vault{kb}})
	v := serving(t, l)
	r.push(map[string]string{"a.md": "# A\n\nchanged\n"})

	v.recordFailure(errors.New("fetch failed"))
	if err := l.syncAfterNotice(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	if got := search(t, v, "changed"); len(got) != 0 {
		t.Error("synced while backing off")
	}
}

// On the instance that writes, each write's quick sync adds a pack without ever compacting. The poller
// compacts once its head check finds nothing new.
func TestPollerCompactsPacksFromWrites(t *testing.T) {
	r := newRemote(t)
	r.push(map[string]string{"a.md": "# A\n"})
	l := newLibrary(t, r, t.TempDir())
	l.Update(t.Context(), vault.Changes{Added: []vault.Vault{kb}})
	v := serving(t, l)

	for i := range maxPacks + 1 {
		r.push(map[string]string{"a.md": "# A\n\nwrite " + strings.Repeat("x", i) + "\n"})
		err := l.SyncChanges(t.Context(), v)
		if err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}
	if n := v.mirror.Packs(); n <= maxPacks {
		t.Fatalf("only %d packs; the test needs more than %d", n, maxPacks)
	}
	l.Poll(t.Context()) // learns the head, which the index already has
	l.Poll(t.Context())
	l.WaitIdle() // compacting runs in the background
	if n := v.mirror.Packs(); n > maxPacks {
		t.Errorf("%d packs after polling", n)
	}
}

// A copy that lost its objects while in use still catches up at the next sync.
func TestCopyWithoutObjectsCatchesUp(t *testing.T) {
	r := newRemote(t)
	r.push(map[string]string{"a.md": "# A\n\nfirst\n"})
	l := newLibrary(t, r, t.TempDir())
	l.Update(t.Context(), vault.Changes{Added: []vault.Vault{kb}})
	v := serving(t, l)

	packs, err := filepath.Glob(filepath.Join(v.dir, gitDir, "objects", "pack", "*"))
	if err != nil || len(packs) == 0 {
		t.Fatalf("no pack files to remove: %v", err)
	}
	for _, p := range packs {
		_ = os.Remove(p)
	}
	r.push(map[string]string{"a.md": "# A\n\nsecond\n"})
	err = l.Sync(t.Context(), v)
	if err != nil {
		t.Fatalf("sync of a copy without its objects: %v", err)
	}
	if got := search(t, v, "second"); len(got) != 1 {
		t.Errorf("after the sync: %v", got)
	}
}

// A vault that can't be cloned backs off longer with every attempt, up to half an hour.
func TestFailedVaultBacksOffMore(t *testing.T) {
	r := newRemote(t)
	r.push(map[string]string{"a.md": "# A\n"})
	l := newLibrary(t, r, t.TempDir())
	clock := fakeClock(l)
	missing := kb
	missing.Name = "ours/missing"
	l.Update(t.Context(), vault.Changes{Added: []vault.Vault{missing}})
	l.background.Wait()
	v := l.vaults[missing.RepoID]
	for i, backoff := range []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute} {
		if i > 0 {
			l.initialise(t.Context(), v)
		}
		clock.advance(backoff - time.Second)
		if v.due() {
			t.Fatalf("due before %v after failure %d", backoff, i+1)
		}
		clock.advance(2 * time.Second)
		if !v.due() {
			t.Fatalf("not due %v after failure %d", backoff, i+1)
		}
	}
}

// A failing head check shows on the status page and backs off.
func TestFailedHeadCheckIsRecorded(t *testing.T) {
	r := newRemote(t)
	r.push(map[string]string{"a.md": "# A\n"})
	l := newLibrary(t, r, t.TempDir())
	l.Update(t.Context(), vault.Changes{Added: []vault.Vault{kb}})
	v := serving(t, l)
	r.mu.Lock()
	r.headErr = errors.New("401 Bad credentials")
	r.mu.Unlock()
	l.Poll(t.Context())
	if st := v.Status(); st.Error == "" || v.due() {
		t.Errorf("status %+v, due %v", st, v.due())
	}
}

// A vault busy with a long sync doesn't hold up a poll round.
func TestPollSkipsABusyVault(t *testing.T) {
	r := newRemote(t)
	r.push(map[string]string{"a.md": "# A\n"})
	l := newLibrary(t, r, t.TempDir())
	l.Update(t.Context(), vault.Changes{Added: []vault.Vault{kb}})
	v := serving(t, l)
	r.push(map[string]string{"a.md": "# A\n\nchanged\n"})
	v.syncMu.Lock() // a rebuild is running
	done := make(chan struct{})
	go func() {
		l.Poll(t.Context())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("the poll round waited for a busy vault")
	}
	v.syncMu.Unlock()
}

// A restart only restores vaults whose git copy is usable without the network.
func TestRestoreSkipsCopiesThatNeedTheNetwork(t *testing.T) {
	r := newRemote(t)
	r.push(map[string]string{"a.md": "# A\n"})
	dataDir := t.TempDir()
	l := newLibrary(t, r, dataDir)
	l.Update(t.Context(), vault.Changes{Added: []vault.Vault{kb}})
	serving(t, l)
	l.Close()

	err := os.RemoveAll(filepath.Join(dataDir, "100", gitDir))
	if err != nil {
		t.Fatal(err)
	}
	l2 := newLibrary(t, r, dataDir)
	err = l2.Restore(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := l2.Vault(kb.RepoID); ok {
		t.Error("restored a vault that would have needed a clone")
	}
}

// Work a poll round starts in the background finishes after the round: here a full rebuild.
func TestPollRebuildsInTheBackground(t *testing.T) {
	r := newRemote(t)
	r.push(map[string]string{"a.md": "# A\n"})
	l := newLibrary(t, r, t.TempDir())
	l.Update(t.Context(), vault.Changes{Added: []vault.Vault{kb}})
	v := serving(t, l)
	// An index at a commit the copy doesn't have can only be rebuilt.
	err := v.index.Apply(t.Context(), strings.Repeat("0", 40), nil)
	if err != nil {
		t.Fatal(err)
	}
	r.push(map[string]string{"a.md": "# A\n\nrebuilt\n"})
	l.Poll(t.Context())
	l.WaitIdle()
	if v.Index().Commit() != r.head() || len(search(t, v, "rebuilt")) != 1 {
		t.Errorf("index at %s after the background rebuild, want %s", v.Index().Commit(), r.head())
	}
}

// A vault that failed to open recovers through the poller once its repository can be cloned.
func TestFailedVaultRecovers(t *testing.T) {
	r := newRemote(t)
	r.push(map[string]string{"a.md": "# A\n"})
	l := newLibrary(t, r, t.TempDir())
	clock := fakeClock(l)
	late := kb
	late.Name = "ours/late"
	l.Update(t.Context(), vault.Changes{Added: []vault.Vault{late}})
	l.WaitIdle()
	if _, ok := l.Vault(late.RepoID); ok {
		t.Fatal("served a vault whose repository doesn't exist yet")
	}
	_, err := git.PlainClone(filepath.Join(r.root, "ours/late.git"), false, &git.CloneOptions{URL: r.dir})
	if err != nil {
		t.Fatal(err)
	}
	r.setID(late.Name, late.RepoID)
	clock.advance(time.Hour) // due
	l.Poll(t.Context())
	l.WaitIdle()
	if _, ok := l.Vault(late.RepoID); !ok {
		t.Errorf("not serving after recovery: %+v", l.vaults[late.RepoID].Status())
	}
}

// A branch change on a vault that never opened goes the normal way in, without holding an opening slot
// it would then wait for itself.
func TestRecloneOfANeverOpenedVault(t *testing.T) {
	r := newRemote(t)
	r.push(map[string]string{"a.md": "# A\n"})
	l := New(r, Options{DataDir: t.TempDir(), GitURL: r.root, Opening: 1}, discard())
	t.Cleanup(l.Close)
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
	case <-time.After(10 * time.Second):
		t.Fatal("reclone of a never-opened vault deadlocked")
	}
}

// A good head check after a failed one clears the failure.
func TestHeadCheckRecovers(t *testing.T) {
	r := newRemote(t)
	r.push(map[string]string{"a.md": "# A\n"})
	l := newLibrary(t, r, t.TempDir())
	clock := fakeClock(l)
	l.Update(t.Context(), vault.Changes{Added: []vault.Vault{kb}})
	v := serving(t, l)
	r.mu.Lock()
	r.headErr = errors.New("502")
	r.mu.Unlock()
	l.Poll(t.Context())
	r.mu.Lock()
	r.headErr = nil
	r.mu.Unlock()
	clock.advance(time.Hour) // due
	l.Poll(t.Context())
	if st := v.Status(); st.Error != "" {
		t.Errorf("error still shown after a good check: %q", st.Error)
	}
}

// A read that finds the git copy missing objects gets it cloned again at the next poll.
func TestBrokenCopyFoundByAReadIsCloned(t *testing.T) {
	r := newRemote(t)
	r.push(map[string]string{"a.md": "# A\n"})
	l := newLibrary(t, r, t.TempDir())
	l.Update(t.Context(), vault.Changes{Added: []vault.Vault{kb}})
	v := serving(t, l)
	doc, err := v.Index().Document(t.Context(), "a.md")
	if err != nil {
		t.Fatal(err)
	}
	packs, _ := filepath.Glob(filepath.Join(v.dir, gitDir, "objects", "pack", "*"))
	for _, p := range packs {
		_ = os.Remove(p)
	}
	_, err = v.Blob(doc.Version)
	if !errors.Is(err, gitsync.ErrBroken) {
		t.Fatalf("Blob on a copy without objects: %v", err)
	}
	l.Poll(t.Context())
	l.WaitIdle()
	content, err := v.Blob(doc.Version)
	if err != nil || string(content) != "# A\n" {
		t.Errorf("Blob after the poll: %q, %v", content, err)
	}
}

// Blobs missing just after a fresh clone, before the index catches up, don't mark the copy broken.
func TestFreshCloneIsntBroken(t *testing.T) {
	r := newRemote(t)
	r.push(map[string]string{"a.md": "# A\n"})
	l := newLibrary(t, r, t.TempDir())
	l.Update(t.Context(), vault.Changes{Added: []vault.Vault{kb}})
	v := serving(t, l)
	doc, err := v.Index().Document(t.Context(), "a.md")
	if err != nil {
		t.Fatal(err)
	}
	r.push(map[string]string{"a.md": "# A\n\nchanged\n"})
	if err := l.freshClone(t.Context(), v, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Blob(doc.Version); err == nil {
		t.Skip("the fresh clone has the old blob")
	}
	if v.broken.Load() {
		t.Error("a blob of an older commit marked the fresh copy broken")
	}
}

// A fresh clone that lost the index's commit to a push during the clone is thrown away, and tried again
// at the next chance, so the index needn't be rebuilt.
func TestCompactionKeepsTheIndexCommit(t *testing.T) {
	r := newRemote(t)
	r.push(map[string]string{"a.md": "# A\n"})
	l := newLibrary(t, r, t.TempDir())
	clock := fakeClock(l)
	l.Update(t.Context(), vault.Changes{Added: []vault.Vault{kb}})
	v := serving(t, l)
	for i := range maxPacks + 1 {
		r.push(map[string]string{"a.md": "# A\n\nversion " + strings.Repeat("x", i) + "\n"})
		v.gitMu.Lock()
		_, err := v.mirror.Fetch(t.Context(), nil)
		v.gitMu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	commit := v.Index().Commit() // the index stays behind, as if the pushes came after the poller looked
	l.compactLocked(t.Context(), v)
	if !v.mirror.Has(commit) {
		t.Fatal("the copy lost the index's commit")
	}
	if wait := v.freshCloneAt.Add(time.Hour).Sub(clock.now()); wait != movedOnRetry {
		t.Errorf("the next fresh clone comes in %v, want %v", wait, movedOnRetry)
	}
	clock.advance(movedOnRetry)
	if err := l.Sync(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	if n := v.mirror.Packs(); n > maxPacks {
		t.Errorf("%d pack files after the sync", n)
	}
	if v.Index().Commit() != r.head() {
		t.Error("index behind after the sync")
	}
}

// stop can be called more than once, and returns after the watcher has ended.
func TestStallContextStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, touch, stop := stallContext(t.Context(), time.Nanosecond)
		touch()
		stop()
		stop()
		if ctx.Err() == nil {
			t.Error("the context is still live after stop")
		}
	})
}

// The vault gauges follow the library's vaults: a served vault is up with its counts, and a removed one
// is gone.
func TestPublish(t *testing.T) {
	r := newRemote(t)
	r.push(map[string]string{"a.md": "# A\n", "b.md": "# B\n"})
	l := newLibrary(t, r, t.TempDir())
	l.Update(t.Context(), vault.Changes{Added: []vault.Vault{kb}})
	serving(t, l)
	l.publish()
	if up, docs := testutil.ToFloat64(vaultUp.WithLabelValues(kb.Name)), testutil.ToFloat64(vaultDocuments.WithLabelValues(kb.Name)); up != 1 || docs != 2 {
		t.Errorf("up %v, documents %v", up, docs)
	}
	l.Update(t.Context(), vault.Changes{Removed: []vault.Vault{kb}})
	l.WaitIdle()
	if n := testutil.CollectAndCount(vaultUp); n != 0 {
		t.Errorf("%d vaults still published after the removal", n)
	}
}
