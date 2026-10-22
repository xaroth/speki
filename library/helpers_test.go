package library_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/xaroth/speki/gh"
	"github.com/xaroth/speki/library"
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
