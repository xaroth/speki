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
	r.mu.Unlock()
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

func TestMetrics(t *testing.T) {
	r := newRemote(t)
	r.push(map[string]string{"a.md": "# A\n", "b.md": "# B\n"})
	l := newLibrary(t, r, t.TempDir())
	l.Update(t.Context(), vault.Changes{Added: []vault.Vault{kb}})
	serving(t, l)

	err := testutil.CollectAndCompare(l.Collector(), strings.NewReader(`
# HELP speki_vault_documents Documents in the vault's index.
# TYPE speki_vault_documents gauge
speki_vault_documents{vault="ours/kb"} 2
# HELP speki_vault_up 1 when the vault is served and its last sync succeeded.
# TYPE speki_vault_up gauge
speki_vault_up{vault="ours/kb"} 1
`), "speki_vault_documents", "speki_vault_up")
	if err != nil {
		t.Error(err)
	}
}

func TestStallContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, touch, stop := stallContext(t.Context(), 4*time.Minute)
		defer stop()
		for range 5 { // twenty minutes of steady progress
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
