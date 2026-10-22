package library_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/xaroth/speki/gh"
	"github.com/xaroth/speki/library"
	"github.com/xaroth/speki/library/gitsync"
	"github.com/xaroth/speki/library/index"
	"github.com/xaroth/speki/vault"
)

var kb = vault.Vault{RepoID: 100, Name: "ours/kb", InstallationID: 1, DefaultBranch: "master"}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// remote is a repository under a fake GitURL, plus a fake GitHub that reports its head.
type remote struct {
	root string // the GitURL
	dir  string

	gitMu sync.Mutex // go-git repositories aren't safe for concurrent use
	repo  *git.Repository

	mu        sync.Mutex
	lookups   int
	headErr   error            // when set, BranchHead fails with it
	afterHead func()           // runs once, after the next BranchHead answered
	ids       map[string]int64 // repository IDs by name; ours/kb is 100 unless set
	gate      chan struct{}    // when set, RepositoryID waits for it to close
	held      chan struct{}    // gets a value when a RepositoryID call waits
}

func newRemote(tb testing.TB) *remote {
	tb.Helper()
	root := tb.TempDir()
	return openRemote(tb, root, filepath.Join(root, kb.Name+".git"), "")
}

// openRemote returns a remote for the repository in dir, cloned from source when it's set.
func openRemote(tb testing.TB, root, dir, source string) *remote {
	tb.Helper()
	var repo *git.Repository
	var err error
	if source == "" {
		repo, err = git.PlainInit(dir, false)
	} else {
		repo, err = git.PlainClone(dir, false, &git.CloneOptions{URL: source})
	}
	if err != nil {
		tb.Fatal(err)
	}
	return &remote{root: root, dir: dir, repo: repo}
}

// RepositoryID answers like GitHub: which repository a name points to now.
func (r *remote) RepositoryID(_ context.Context, _ int64, repo string) (int64, error) {
	r.mu.Lock()
	gate, held := r.gate, r.held
	id, ok := r.ids[repo]
	r.mu.Unlock()
	if gate != nil {
		select {
		case held <- struct{}{}:
		default:
		}
		<-gate
	}
	if ok {
		return id, nil
	}
	if repo == kb.Name {
		return kb.RepoID, nil
	}
	return 0, gh.ErrNotFound
}

// BranchHead answers like GitHub: unchanged when the ETag (the commit) matches.
func (r *remote) BranchHead(_ context.Context, _ int64, _, _, etag string) (gh.Head, error) {
	r.mu.Lock()
	r.lookups++
	headErr, after := r.headErr, r.afterHead
	r.afterHead = nil
	r.mu.Unlock()
	if headErr != nil {
		return gh.Head{}, headErr
	}
	h, err := r.currentHead()
	if err != nil {
		return gh.Head{}, err
	}
	if after != nil {
		after()
	}
	if etag == h {
		return gh.Head{ETag: etag}, nil
	}
	return gh.Head{SHA: h, ETag: h}, nil
}

func (r *remote) setID(repo string, id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ids == nil {
		r.ids = map[string]int64{}
	}
	r.ids[repo] = id
}

func (r *remote) setHeadErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.headErr = err
}

func (r *remote) lookupCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lookups
}

// pushAfterHead pushes files right after the next BranchHead answered, as a push racing the poller would.
func (r *remote) pushAfterHead(tb testing.TB, files map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.afterHead = func() {
		_, err := r.commit(files)
		if err != nil {
			tb.Errorf("push after the head check: %v", err)
		}
	}
}

// hold makes RepositoryID calls wait, as a slow sync would, until release is called. held gets a value
// once a call waits.
func (r *remote) hold(tb testing.TB) (held <-chan struct{}, release func()) {
	r.mu.Lock()
	gate, h := make(chan struct{}), make(chan struct{}, 1)
	r.gate, r.held = gate, h
	r.mu.Unlock()
	var once sync.Once
	release = func() {
		once.Do(func() {
			r.mu.Lock()
			r.gate = nil
			r.mu.Unlock()
			close(gate)
		})
	}
	tb.Cleanup(release)
	return h, release
}

// push commits files, an empty content deleting the file, and returns the new head.
func (r *remote) push(tb testing.TB, files map[string]string) string {
	tb.Helper()
	h, err := r.commit(files)
	if err != nil {
		tb.Fatal(err)
	}
	return h
}

func (r *remote) commit(files map[string]string) (string, error) {
	r.gitMu.Lock()
	defer r.gitMu.Unlock()
	wt, err := r.repo.Worktree()
	if err != nil {
		return "", err
	}
	for name, content := range files {
		if content == "" {
			_, err = wt.Remove(name)
			if err != nil {
				return "", err
			}
			continue
		}
		full := filepath.Join(r.dir, name)
		err = os.MkdirAll(filepath.Dir(full), 0o755)
		if err == nil {
			err = os.WriteFile(full, []byte(content), 0o644)
		}
		if err == nil {
			_, err = wt.Add(name)
		}
		if err != nil {
			return "", err
		}
	}
	h, err := wt.Commit("push", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@x", When: time.Now()}})
	if err != nil {
		return "", err
	}
	return h.String(), nil
}

func (r *remote) checkout(tb testing.TB, branch string) {
	tb.Helper()
	r.gitMu.Lock()
	defer r.gitMu.Unlock()
	wt, err := r.repo.Worktree()
	if err == nil {
		err = wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName(branch), Create: true})
	}
	if err != nil {
		tb.Fatal(err)
	}
}

func (r *remote) currentHead() (string, error) {
	r.gitMu.Lock()
	defer r.gitMu.Unlock()
	ref, err := r.repo.Head()
	if err != nil {
		return "", err
	}
	return ref.Hash().String(), nil
}

func (r *remote) head(tb testing.TB) string {
	tb.Helper()
	h, err := r.currentHead()
	if err != nil {
		tb.Fatal(err)
	}
	return h
}

// newLibrary returns a library that clones from r, closed at the end of the test. DataDir defaults to a
// temporary folder.
func newLibrary(tb testing.TB, r *remote, opts library.Options) *library.Library {
	tb.Helper()
	if opts.DataDir == "" {
		opts.DataDir = tb.TempDir()
	}
	opts.GitURL = r.root
	l := library.New(r, opts, discard())
	tb.Cleanup(l.Close)
	return l
}

// clock is a fake clock for the library's backoff and fresh-clone times.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

// newClock sets l's clock to a fake one. Call it before the library has vaults.
func newClock(l *library.Library) *clock {
	c := &clock{t: time.Now()}
	l.SetNow(c.now)
	return c
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func search(tb testing.TB, v *library.Vault, q string) []string {
	tb.Helper()
	res, err := index.Search(tb.Context(), []index.Source{{Name: "kb", Index: v.Index()}}, q, index.Options{})
	if err != nil {
		tb.Fatal(err)
	}
	var paths []string
	for _, h := range res.Hits {
		paths = append(paths, h.Path)
	}
	return paths
}

// serving adds kb to l and returns it once it's served.
func serving(tb testing.TB, l *library.Library) *library.Vault {
	tb.Helper()
	l.Update(tb.Context(), vault.Changes{Added: []vault.Vault{kb}})
	l.WaitIdle()
	v, ok := l.Vault(kb.RepoID)
	if !ok {
		tb.Fatalf("vault not serving: %+v", l.Status())
	}
	return v
}

// gitDir is where kb's git copy lives in dataDir.
func gitDir(dataDir string) string { return filepath.Join(dataDir, "100", "git") }

func packs(tb testing.TB, dataDir string) int {
	tb.Helper()
	p, err := filepath.Glob(filepath.Join(gitDir(dataDir), "objects", "pack", "*.pack"))
	if err != nil {
		tb.Fatal(err)
	}
	return len(p)
}

// removeObjects deletes every pack file of kb's git copy, as a damaged disk would.
func removeObjects(tb testing.TB, dataDir string) {
	tb.Helper()
	p, err := filepath.Glob(filepath.Join(gitDir(dataDir), "objects", "pack", "*"))
	if err != nil || len(p) == 0 {
		tb.Fatalf("got pack files %v, %v, want some", p, err)
	}
	for _, f := range p {
		err = os.Remove(f)
		if err != nil {
			tb.Fatal(err)
		}
	}
}

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
