package knowledge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"

	"github.com/eve-online-tools/speki/access"
	"github.com/eve-online-tools/speki/gh"
	"github.com/eve-online-tools/speki/index"
	"github.com/eve-online-tools/speki/vault"
)

// fakeCopy is a vault copy with a real index, and the git tree and blobs in memory.
type fakeCopy struct {
	mu    sync.Mutex
	info  vault.Vault
	idx   *index.Index
	blobs map[string][]byte
	trees map[string]map[string][]byte // commit -> path -> content

	blobFailures int
}

func (c *fakeCopy) Index() *index.Index { return c.idx }

func (c *fakeCopy) Info() vault.Vault {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info
}

// failBlobs makes the next n blob reads fail, as after a fresh clone dropped old objects.
func (c *fakeCopy) failBlobs(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blobFailures = n
}

func (c *fakeCopy) Blob(sha string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.blobFailures > 0 {
		c.blobFailures--
		return nil, errors.New("object not found")
	}
	b, ok := c.blobs[sha]
	if !ok {
		return nil, errors.New("object not found")
	}
	return b, nil
}

func (c *fakeCopy) Paths(commit string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tree, ok := c.trees[commit]
	if !ok {
		return nil, fmt.Errorf("commit %s not in the copy", commit)
	}
	return slices.Sorted(maps.Keys(tree)), nil
}

func (c *fakeCopy) Version(commit, path string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tree, ok := c.trees[commit]
	if !ok {
		return "", fmt.Errorf("commit %s not in the copy", commit)
	}
	content, ok := tree[path]
	if !ok {
		return "", nil
	}
	return blobID(content), nil
}

// tree returns the files at the index's commit.
func (c *fakeCopy) tree() map[string][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.trees[c.idx.Commit()]
}

// setFile changes one file in the tree at commit without touching the index; nil content deletes it.
// The commit starts from the tree at the index's commit when the copy doesn't have it yet.
func (c *fakeCopy) setFile(commit, path string, content []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tree, ok := c.trees[commit]
	if !ok {
		tree = c.trees[c.idx.Commit()]
	}
	tree = maps.Clone(tree)
	if content == nil {
		delete(tree, path)
	} else {
		tree[path] = content
		c.blobs[blobID(content)] = content
	}
	c.trees[commit] = tree
}

// load rebuilds the copy at commit with files. Files that aren't markdown are in the tree but not indexed.
func (c *fakeCopy) load(t *testing.T, commit string, files map[string][]byte) {
	t.Helper()
	c.mu.Lock()
	c.trees[commit] = maps.Clone(files)
	for _, content := range files {
		c.blobs[blobID(content)] = content
	}
	c.mu.Unlock()
	changes := func(yield func(index.Change, error) bool) {
		for _, p := range slices.Sorted(maps.Keys(files)) {
			if !strings.HasSuffix(p, ".md") {
				continue
			}
			if !yield(index.Change{Path: p, Version: blobID(files[p]), Content: files[p]}, nil) {
				return
			}
		}
	}
	err := c.idx.Build(t.Context(), commit, iter.Seq2[index.Change, error](changes))
	if err != nil {
		t.Fatal(err)
	}
}

func newCopy(t *testing.T, files map[string]string) *fakeCopy {
	t.Helper()
	idx, err := index.Open(t.Context(), t.TempDir(), discard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(idx.Close)
	c := &fakeCopy{idx: idx, blobs: map[string][]byte{}, trees: map[string]map[string][]byte{}}
	raw := map[string][]byte{}
	for p, content := range files {
		raw[p] = []byte(content)
	}
	c.load(t, "c1", raw)
	return c
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeRepo is a repository on the fake GitHub.
type fakeRepo struct {
	head  string
	files map[string][]byte
}

// fakeGitHub keeps repositories that start out as their copies, and takes commits to them.
type fakeGitHub struct {
	mu      sync.Mutex
	t       *testing.T
	f       *fixture
	repos   map[string]*fakeRepo
	commits int
	// Commit
	requests []gh.CommitRequest
	before   func(repo *fakeRepo) // runs before a commit is checked, to change the repository under it
	err      error                // returned by Commit when set
	lostErr  error                // when set, Commit makes the commit and then fails with it, as when the answer is lost
	log      []gh.Commit          // the commits made, newest first
	// Commits (history)
	token, repo, branch, path string
	limit                     int
	history                   []gh.Commit
	historyErr                error
}

func (f *fakeGitHub) Commits(_ context.Context, token, repo, branch, path string, limit int) ([]gh.Commit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.history == nil && f.historyErr == nil { // nothing set up: what was committed
		return f.log, nil
	}
	f.token, f.repo, f.branch, f.path, f.limit = token, repo, branch, path, limit
	return f.history, f.historyErr
}

// repoOf returns the repository behind a vault, starting it from the vault's copy.
func (f *fakeGitHub) repoOf(name string) *fakeRepo {
	r, ok := f.repos[name]
	if ok {
		return r
	}
	for _, v := range []vault.Vault{ours, shared, secret} {
		if v.Name == name {
			c := f.f.copies[v.RepoID]
			r = &fakeRepo{head: c.idx.Commit(), files: maps.Clone(c.tree())}
		}
	}
	f.repos[name] = r
	return r
}

func (f *fakeGitHub) Commit(_ context.Context, _ string, req gh.CommitRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if f.err != nil {
		return "", f.err
	}
	r := f.repoOf(req.Repo)
	if f.before != nil {
		f.before(r)
	}
	if req.ExpectedHead != r.head {
		return "", gh.ErrHeadMoved
	}
	r.files = maps.Clone(r.files)
	for _, ch := range req.Changes {
		if ch.Content == nil {
			delete(r.files, ch.Path)
		} else {
			r.files[ch.Path] = ch.Content
		}
	}
	f.commits++
	r.head = fmt.Sprintf("commit-%d", f.commits)
	f.log = append([]gh.Commit{{SHA: r.head, Message: req.Message}}, f.log...)
	if f.lostErr != nil {
		return "", f.lostErr
	}
	return r.head, nil
}

// push changes a repository behind speki's back, as a push from elsewhere would.
func (f *fakeGitHub) push(name, path string, content []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repoOf(name)
	r.files = maps.Clone(r.files)
	r.files[path] = content
	f.commits++
	r.head = fmt.Sprintf("push-%d", f.commits)
}

// fakeVaults serves the fixture's copies. Sync brings a copy up to date with its fake repository.
type fakeVaults struct {
	f       *fixture
	syncs   atomic.Int32
	syncErr error
}

func (v *fakeVaults) Copy(id int64) (Copy, bool) {
	c, ok := v.f.copies[id]
	if !ok {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.info.RepoID == 0 {
		for _, vv := range []vault.Vault{ours, shared, secret} {
			if vv.RepoID == id {
				c.info = vv
			}
		}
	}
	return c, true
}

func (v *fakeVaults) Sync(_ context.Context, id int64) error {
	v.syncs.Add(1)
	if v.syncErr != nil {
		return v.syncErr
	}
	gh := v.f.github
	gh.mu.Lock()
	var r *fakeRepo
	for _, vv := range []vault.Vault{ours, shared, secret} {
		if vv.RepoID == id {
			r = gh.repos[vv.Name]
		}
	}
	var head string
	var files map[string][]byte
	if r != nil {
		head, files = r.head, maps.Clone(r.files)
	}
	gh.mu.Unlock()
	c := v.f.copies[id]
	if r == nil || c.idx.Commit() == head {
		return nil
	}
	c.load(v.f.t, head, files)
	return nil
}

var ours = vault.Vault{RepoID: 1, Name: "ours/kb", DefaultBranch: "main", Config: vault.Config{Title: "KB", Description: "Our knowledge.", Guidelines: "One page per service."}}
var shared = vault.Vault{RepoID: 2, Name: "ours/shared", DefaultBranch: "main"}
var secret = vault.Vault{RepoID: 3, Name: "ours/secret", DefaultBranch: "main"}

// fixture serves three vaults. The test user may read ours and shared, and write ours; secret holds a
// document only others may read.
type fixture struct {
	t       *testing.T
	base    *Base
	grant   *access.Grant
	copies  map[int64]*fakeCopy
	github  *fakeGitHub
	vaults  *fakeVaults
	notices *fakeNotices
}

// fakeNotices records the change notices sent.
type fakeNotices struct {
	mu   sync.Mutex
	sent []string // repo id:commit
}

func (n *fakeNotices) Publish(_ context.Context, repoID int64, commit string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, fmt.Sprintf("%d:%s", repoID, commit))
	return nil
}

var fixedNow = time.Date(2026, 10, 9, 14, 2, 11, 0, time.UTC)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		t: t,
		copies: map[int64]*fakeCopy{
			1: newCopy(t, map[string]string{
				"README.md":             "---\ntitle: About\n---\n# About\n\nThis vault holds zeppelin notes.\n",
				"services/esi-proxy.md": "---\ntitle: ESI proxy\ntags: [esi]\n---\n# ESI proxy\n\nThe proxy caches zeppelin responses.\n\n## Deploying\n\nRun the job.\n",
				"services/db/pg.md":     "# Postgres\n\nZeppelin database.\n",
			}),
			2: newCopy(t, map[string]string{"zeppelin.md": "# Zeppelin\n\nShared zeppelin facts.\n"}),
			3: newCopy(t, map[string]string{"services/esi-proxy.md": "# Hidden\n\nThe secret zeppelin plan.\n"}),
		},
	}
	f.github = &fakeGitHub{t: t, f: f, repos: map[string]*fakeRepo{}}
	f.vaults = &fakeVaults{f: f}
	sandbox, err := index.NewSandbox()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.Close() })
	f.notices = &fakeNotices{}
	f.base = New(f.vaults, f.github, sandbox, f.notices, discard())
	f.base.now = func() time.Time { return fixedNow }
	f.grant = access.NewGrant(access.User{ID: 7, Login: "octocat", Token: "ghu_x", Client: "Claude Code", Write: true},
		[]vault.Vault{ours, shared}, []int64{1})
	return f
}

func wantCode(t *testing.T, err error, code Code) {
	t.Helper()
	var kerr *Error
	if !errors.As(err, &kerr) || kerr.Code != code {
		t.Fatalf("error = %v, want %s", err, code)
	}
}

func TestVaults(t *testing.T) {
	f := newFixture(t)
	got, err := f.base.Vaults(t.Context(), f.grant)
	if err != nil {
		t.Fatal(err)
	}
	want := []VaultInfo{
		{Name: "ours/kb", Title: "KB", Description: "Our knowledge.", Guidelines: "One page per service.", CanWrite: true, Documents: 3},
		{Name: "ours/shared", Documents: 1},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("vaults (-want +got):\n%s", diff)
	}

	delete(f.copies, 2) // served when the grant was made, but not any more
	got, _ = f.base.Vaults(t.Context(), f.grant)
	if len(got) != 2 || got[1].Name != "ours/shared" || got[1].State != access.StateGone || got[1].Documents != 0 {
		t.Errorf("vault gone since the grant: %+v", got)
	}
}

var building = vault.Vault{RepoID: 4, Name: "ours/new", DefaultBranch: "main", Config: vault.Config{Title: "New"}}
var failed = vault.Vault{RepoID: 5, Name: "ours/old", DefaultBranch: "main"}

// withUnavailable gives f's user a grant that also knows building and failed, which they may read but
// speki doesn't serve. building has a copy with matching documents, which must stay out of every answer.
func withUnavailable(t *testing.T, f *fixture) {
	t.Helper()
	f.copies[building.RepoID] = newCopy(t, map[string]string{"zeppelin.md": "# Zeppelin\n\nNew zeppelin facts.\n"})
	f.grant = access.NewGrant(f.grant.User(), []vault.Vault{ours, shared}, []int64{1},
		access.Unavailable{Vault: failed, State: access.StateFailed},
		access.Unavailable{Vault: building, State: access.StateBuilding},
	)
}

func TestUnavailableVaultsSkipped(t *testing.T) {
	f := newFixture(t)
	withUnavailable(t, f)
	ctx := t.Context()
	want := []Skipped{
		{Name: "ours/new", State: access.StateBuilding},
		{Name: "ours/old", State: access.StateFailed},
	}

	res, err := f.base.Search(ctx, f.grant, SearchRequest{Query: "zeppelin"})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range res.Hits {
		if h.Source != "ours/kb" && h.Source != "ours/shared" {
			t.Errorf("hit from a vault that isn't served: %+v", h)
		}
	}
	if len(res.Hits) != 4 {
		t.Errorf("hits = %+v", res.Hits)
	}
	if diff := cmp.Diff(want, res.Skipped); diff != "" {
		t.Errorf("search skipped (-want +got):\n%s", diff)
	}

	q, err := f.base.Query(ctx, f.grant, QueryRequest{SQL: "SELECT vault, count(*) FROM documents GROUP BY vault ORDER BY vault"})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(q.Rows.Rows) != "[[ours/kb 3] [ours/shared 1]]" {
		t.Errorf("rows = %v", q.Rows.Rows)
	}
	if diff := cmp.Diff(want, q.Skipped); diff != "" {
		t.Errorf("query skipped (-want +got):\n%s", diff)
	}

	// Naming vaults covers exactly those, so nothing was skipped.
	res, err = f.base.Search(ctx, f.grant, SearchRequest{Query: "zeppelin", Vaults: []string{"ours/shared"}})
	if err != nil || res.Skipped != nil {
		t.Errorf("named vault: skipped %v, %v", res.Skipped, err)
	}
	q, err = f.base.Query(ctx, f.grant, QueryRequest{SQL: "SELECT 1", Vaults: []string{"ours/kb"}})
	if err != nil || q.Skipped != nil {
		t.Errorf("named vault: skipped %v, %v", q.Skipped, err)
	}

	vaults, err := f.base.Vaults(ctx, f.grant)
	if err != nil {
		t.Fatal(err)
	}
	wantVaults := []VaultInfo{
		{
			Name:        "ours/kb",
			Title:       "KB",
			Description: "Our knowledge.",
			Guidelines:  "One page per service.",
			CanWrite:    true,
			Documents:   3,
		},
		{Name: "ours/new", Title: "New", State: access.StateBuilding},
		{Name: "ours/old", State: access.StateFailed},
		{Name: "ours/shared", Documents: 1},
	}
	if diff := cmp.Diff(wantVaults, vaults); diff != "" {
		t.Errorf("vaults (-want +got):\n%s", diff)
	}

	// A served vault that goes away after the grant was made is skipped too, not silently left out.
	delete(f.copies, shared.RepoID)
	res, err = f.base.Search(ctx, f.grant, SearchRequest{Query: "zeppelin"})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range res.Hits {
		if h.Source != "ours/kb" {
			t.Errorf("hit from a vault that isn't served: %+v", h)
		}
	}
	want = []Skipped{
		{Name: "ours/new", State: access.StateBuilding},
		{Name: "ours/old", State: access.StateFailed},
		{Name: "ours/shared", State: access.StateGone},
	}
	if diff := cmp.Diff(want, res.Skipped); diff != "" {
		t.Errorf("search skipped (-want +got):\n%s", diff)
	}
	q, err = f.base.Query(ctx, f.grant, QueryRequest{SQL: "SELECT 1"})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, q.Skipped); diff != "" {
		t.Errorf("query skipped (-want +got):\n%s", diff)
	}
}

func TestUnavailableVaultNamed(t *testing.T) {
	f := newFixture(t)
	withUnavailable(t, f)
	ctx := t.Context()
	calls := map[string]func(name string) error{
		"list": func(name string) error {
			_, err := f.base.List(ctx, f.grant, ListRequest{Vault: name})
			return err
		},
		"search": func(name string) error {
			_, err := f.base.Search(ctx, f.grant, SearchRequest{Query: "zeppelin", Vaults: []string{"ours/kb", name}})
			return err
		},
		"query": func(name string) error {
			_, err := f.base.Query(ctx, f.grant, QueryRequest{SQL: "SELECT 1", Vaults: []string{name}})
			return err
		},
		"read": func(name string) error {
			_, err := f.base.Read(ctx, f.grant, ReadRequest{Vault: name, Path: "zeppelin.md"})
			return err
		},
		"history": func(name string) error {
			_, err := f.base.History(ctx, f.grant, HistoryRequest{Vault: name})
			return err
		},
		"write": func(name string) error {
			_, err := f.base.Write(ctx, f.grant, WriteRequest{Vault: name, Path: "a.md", Content: "# A\n"})
			return err
		},
		"edit": func(name string) error {
			_, err := f.base.Edit(ctx, f.grant, EditRequest{Vault: name, Path: "zeppelin.md", OldText: "New", NewText: "Old"})
			return err
		},
		"move": func(name string) error {
			_, err := f.base.Move(ctx, f.grant, MoveRequest{Vault: name, From: "zeppelin.md", To: "z.md", Version: "x"})
			return err
		},
		"delete": func(name string) error {
			_, err := f.base.Delete(ctx, f.grant, DeleteRequest{Vault: name, Path: "zeppelin.md", Version: "x"})
			return err
		},
	}
	tests := []struct {
		name    string
		code    Code
		message string
	}{
		{name: "OURS/new", code: CodeUnavailable, message: "unavailable: ours/new is still building its index. Try again in a minute"},
		{name: "ours/old", code: CodeUnavailable, message: "unavailable: ours/old failed to open"},
		{name: "ours/nothing", code: CodeNotFound, message: `not_found: there's no vault named "ours/nothing"`},
		{name: secret.Name, code: CodeNotFound, message: `not_found: there's no vault named "ours/secret"`},
	}
	for tool, call := range calls {
		for _, tt := range tests {
			t.Run(tool+" "+tt.name, func(t *testing.T) {
				err := call(tt.name)
				wantCode(t, err, tt.code)
				if !strings.HasPrefix(err.Error(), tt.message) {
					t.Errorf("error = %q, want it to start with %q", err, tt.message)
				}
			})
		}
	}
	if f.github.commits != 0 || f.github.repo != "" {
		t.Error("asked GitHub about a vault that isn't served")
	}
}

func paths(entries []index.Entry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.Path)
	}
	return out
}

func TestList(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	root, err := f.base.List(ctx, f.grant, ListRequest{Vault: "ours/kb"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(root.Folders, []string{"services/"}) || !slices.Equal(paths(root.Documents), []string{"README.md"}) {
		t.Errorf("root = %+v", root)
	}
	if root.Guidelines != "One page per service." || root.NextCursor != "" {
		t.Errorf("root guidelines or cursor = %q, %q", root.Guidelines, root.NextCursor)
	}

	sub, err := f.base.List(ctx, f.grant, ListRequest{Vault: "ours/kb", Path: "/services", Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(sub.Folders) != 0 || !slices.Equal(paths(sub.Documents), []string{"services/db/pg.md", "services/esi-proxy.md"}) {
		t.Errorf("recursive = %+v", sub)
	}
	if sub.Guidelines != "" {
		t.Error("guidelines outside the root")
	}

	_, err = f.base.List(ctx, f.grant, ListRequest{Vault: "ours/kb", Path: "nothing"})
	wantCode(t, err, CodeNotFound)
	_, err = f.base.List(ctx, f.grant, ListRequest{Vault: "ours/kb", Path: "services", Cursor: "README.md"})
	wantCode(t, err, CodeInvalid)
}

func TestListPages(t *testing.T) {
	files := map[string]string{}
	for i := range listLimit + 5 {
		files[fmt.Sprintf("n/%03d.md", i)] = "# Note\n"
	}
	f := newFixture(t)
	f.copies[1] = newCopy(t, files)

	var seen []string
	req := ListRequest{Vault: "ours/kb", Path: "n"}
	for pages := 0; ; pages++ {
		page, err := f.base.List(t.Context(), f.grant, req)
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, paths(page.Documents)...)
		if page.NextCursor == "" {
			break
		}
		if pages > 2 {
			t.Fatal("too many pages")
		}
		req.Cursor = page.NextCursor
	}
	if len(seen) != listLimit+5 || !slices.IsSorted(seen) || len(slices.Compact(seen)) != len(seen) {
		t.Errorf("paged through %d documents", len(seen))
	}
}

func TestSearch(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	res, err := f.base.Search(ctx, f.grant, SearchRequest{Query: "zeppelin"})
	if err != nil {
		t.Fatal(err)
	}
	vaults := map[string]bool{}
	for _, h := range res.Hits {
		vaults[h.Source] = true
	}
	if len(res.Hits) != 4 || !vaults["ours/kb"] || !vaults["ours/shared"] {
		t.Errorf("hits = %+v", res.Hits)
	}

	res, err = f.base.Search(ctx, f.grant, SearchRequest{Query: "zeppelin", Vaults: []string{"ours/shared"}})
	if err != nil || len(res.Hits) != 1 || res.Hits[0].Path != "zeppelin.md" {
		t.Errorf("one vault: %+v, %v", res.Hits, err)
	}
	res, err = f.base.Search(ctx, f.grant, SearchRequest{Query: "zeppelin", Path: "services", Tags: []string{"esi"}})
	if err != nil || len(res.Hits) != 1 || res.Hits[0].Path != "services/esi-proxy.md" {
		t.Errorf("path and tags: %+v, %v", res.Hits, err)
	}

	for name, req := range map[string]SearchRequest{
		"empty":           {Query: "  "},
		"only exclusions": {Query: "-zeppelin"},
		"limit":           {Query: "x", Limit: 51},
	} {
		_, err := f.base.Search(ctx, f.grant, req)
		var kerr *Error
		if !errors.As(err, &kerr) || kerr.Code != CodeInvalid {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRead(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	raw := "---\ntitle: ESI proxy\ntags: [esi]\n---\n# ESI proxy\n\nThe proxy caches zeppelin responses.\n\n## Deploying\n\nRun the job.\n"

	p, err := f.base.Read(ctx, f.grant, ReadRequest{Vault: "ours/kb", Path: "services/esi-proxy.md"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Content != raw || p.FromLine != 1 || p.LineCount != 11 || p.TotalLines != 11 || p.Outline != nil {
		t.Errorf("whole document = %+v", p)
	}
	if p.Document.Title != "ESI proxy" || p.Document.Version != blobID([]byte(raw)) {
		t.Errorf("metadata = %+v", p.Document)
	}

	p, err = f.base.Read(ctx, f.grant, ReadRequest{Vault: "ours/kb", Path: "/services/esi-proxy.md", FromLine: 9, LineCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	if p.Content != "## Deploying\n" || p.LineCount != 1 || p.TotalLines != 11 {
		t.Errorf("one line = %+v", p)
	}
	if len(p.Outline) != 2 || p.Outline[1].Text != "Deploying" || p.Outline[1].Line != 9 {
		t.Errorf("outline = %+v", p.Outline)
	}

	_, err = f.base.Read(ctx, f.grant, ReadRequest{Vault: "ours/kb", Path: "services/esi-proxy.md", FromLine: 12})
	wantCode(t, err, CodeInvalid)
	_, err = f.base.Read(ctx, f.grant, ReadRequest{Vault: "ours/kb", Path: "nope.md"})
	wantCode(t, err, CodeNotFound)
}

func TestReadCaps(t *testing.T) {
	f := newFixture(t)
	long := strings.Repeat("x", 1000) + "\n"
	huge := "é" + strings.Repeat("y", maxReadBytes+10) + "\n"
	f.copies[1] = newCopy(t, map[string]string{
		"lines.md": strings.Repeat("a\n", 1500),
		"bytes.md": strings.Repeat(long, 100),
		"huge.md":  huge + "tail\n",
	})
	ctx := t.Context()

	p, _ := f.base.Read(ctx, f.grant, ReadRequest{Vault: "ours/kb", Path: "lines.md"})
	if p.LineCount != maxReadLines || p.TotalLines != 1500 {
		t.Errorf("line cap: %d of %d", p.LineCount, p.TotalLines)
	}
	p, _ = f.base.Read(ctx, f.grant, ReadRequest{Vault: "ours/kb", Path: "bytes.md"})
	if len(p.Content) > maxReadBytes || p.LineCount != maxReadBytes/len(long) {
		t.Errorf("byte cap: %d bytes, %d lines", len(p.Content), p.LineCount)
	}
	p, _ = f.base.Read(ctx, f.grant, ReadRequest{Vault: "ours/kb", Path: "huge.md"})
	if p.LineCount != 1 || len(p.Content) > maxReadBytes || !strings.HasPrefix(p.Content, "é") || len(p.Warnings) != 1 {
		t.Errorf("overlong line: %d lines, %d bytes, warnings %v", p.LineCount, len(p.Content), p.Warnings)
	}
}

func TestReadMissingBlob(t *testing.T) {
	f := newFixture(t)
	c := f.copies[1]
	doc, _ := c.idx.Document(t.Context(), "README.md")
	gone := c.blobs[doc.Version]
	delete(c.blobs, doc.Version)
	_, err := f.base.Read(t.Context(), f.grant, ReadRequest{Vault: "ours/kb", Path: "README.md"})
	if err == nil {
		t.Fatal("read a blob the copy doesn't have")
	}
	if _, ok := errors.AsType[*Error](err); ok {
		t.Errorf("a missing blob is speki's problem, not the agent's: %v", err)
	}
	c.blobs[doc.Version] = gone
}

func TestHistory(t *testing.T) {
	f := newFixture(t)
	f.github.history = []gh.Commit{
		{SHA: "c2", Author: "octocat", Date: time.Unix(2, 0), Message: "Update a.md\n\nWhy.\n\nSpeki-Client: Claude Code\n"},
		{SHA: "c1", Author: "someone", Date: time.Unix(1, 0), Message: "Create a.md\n\nSpeki-Client: fake\nin the body\n\nSigned-off-by: x\n"},
	}
	got, err := f.base.History(t.Context(), f.grant, HistoryRequest{Vault: "ours/kb", Path: "/a.md"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Commit{
		{SHA: "c2", Author: "octocat", Date: time.Unix(2, 0), Message: "Update a.md", Client: "Claude Code"},
		{SHA: "c1", Author: "someone", Date: time.Unix(1, 0), Message: "Create a.md"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("history (-want +got):\n%s", diff)
	}
	h := f.github
	if h.token != "ghu_x" || h.repo != "ours/kb" || h.branch != "main" || h.path != "a.md" || h.limit != 10 {
		t.Errorf("asked GitHub for %+v", h)
	}

	f.github.historyErr = gh.ErrNotFound
	_, err = f.base.History(t.Context(), f.grant, HistoryRequest{Vault: "ours/kb"})
	wantCode(t, err, CodeNotFound)
	_, err = f.base.History(t.Context(), f.grant, HistoryRequest{Vault: "ours/kb", Limit: 51})
	wantCode(t, err, CodeInvalid)
}

// TestOtherVaultsStayHidden tries every read path on a vault the user can't read, which has a document
// at a path that also exists in a vault they can.
func TestOtherVaultsStayHidden(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	const hidden = "secret"

	vaults, _ := f.base.Vaults(ctx, f.grant)
	for _, v := range vaults {
		if v.Name == secret.Name {
			t.Error("Vaults lists the secret vault")
		}
	}
	res, err := f.base.Search(ctx, f.grant, SearchRequest{Query: hidden})
	if err != nil || len(res.Hits) != 0 {
		t.Errorf("search all: %+v, %v", res.Hits, err)
	}
	res, err = f.base.Search(ctx, f.grant, SearchRequest{Query: "zeppelin"})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range res.Hits {
		if h.Source == secret.Name || strings.Contains(h.Snippet, hidden) {
			t.Errorf("search hit from the secret vault: %+v", h)
		}
	}
	_, err = f.base.Search(ctx, f.grant, SearchRequest{Query: hidden, Vaults: []string{secret.Name}})
	wantCode(t, err, CodeNotFound)
	_, err = f.base.Search(ctx, f.grant, SearchRequest{Query: hidden, Vaults: []string{"ours/kb", secret.Name}})
	wantCode(t, err, CodeNotFound)
	_, err = f.base.List(ctx, f.grant, ListRequest{Vault: secret.Name})
	wantCode(t, err, CodeNotFound)
	_, err = f.base.Read(ctx, f.grant, ReadRequest{Vault: secret.Name, Path: "services/esi-proxy.md"})
	wantCode(t, err, CodeNotFound)
	_, err = f.base.History(ctx, f.grant, HistoryRequest{Vault: secret.Name})
	wantCode(t, err, CodeNotFound)
	if f.github.repo != "" {
		t.Error("asked GitHub about the secret vault")
	}

	// A readable vault's document at the same path is that vault's, not the secret one.
	p, err := f.base.Read(ctx, f.grant, ReadRequest{Vault: "ours/kb", Path: "services/esi-proxy.md"})
	if err != nil || strings.Contains(p.Content, hidden) {
		t.Errorf("read: %q, %v", p.Content, err)
	}
	// Names match without regard to case, which doesn't open the secret vault either.
	_, err = f.base.Read(ctx, f.grant, ReadRequest{Vault: "OURS/SECRET", Path: "services/esi-proxy.md"})
	wantCode(t, err, CodeNotFound)

	// Filters aimed at the secret vault don't get past the vault check.
	_, err = f.base.List(ctx, f.grant, ListRequest{Vault: secret.Name, Path: "services"})
	wantCode(t, err, CodeNotFound)
	_, err = f.base.Search(ctx, f.grant, SearchRequest{Query: hidden, Vaults: []string{secret.Name}, Path: "services", Tags: []string{"x"}})
	wantCode(t, err, CodeNotFound)
	_, err = f.base.Query(ctx, f.grant, QueryRequest{SQL: "SELECT 1", Vaults: []string{secret.Name}})
	wantCode(t, err, CodeNotFound)

	// The answer for a vault the user can't read is the answer for one that doesn't exist.
	_, errSecret := f.base.Read(ctx, f.grant, ReadRequest{Vault: secret.Name, Path: "x.md"})
	_, errNone := f.base.Read(ctx, f.grant, ReadRequest{Vault: "ours/nothing", Path: "x.md"})
	if strings.ReplaceAll(errSecret.Error(), secret.Name, "V") != strings.ReplaceAll(errNone.Error(), "ours/nothing", "V") {
		t.Errorf("answers differ:\n%v\n%v", errSecret, errNone)
	}

	// Spelling correction only knows the user's vaults: "secrut" is close to a word only the secret
	// vault has, so it isn't corrected to it.
	res, err = f.base.Search(ctx, f.grant, SearchRequest{Query: "secrut"})
	if err != nil {
		t.Fatal(err)
	}
	for word, instead := range res.Corrected {
		if instead == hidden {
			t.Errorf("corrected %q to %q, a word only the secret vault has", word, instead)
		}
	}
}

func TestVaultNames(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	p, err := f.base.Read(ctx, f.grant, ReadRequest{Vault: "Ours/KB", Path: "README.md"})
	if err != nil || p.Vault != "ours/kb" {
		t.Errorf("read with another letter case: vault %q, %v", p.Vault, err)
	}
	res, err := f.base.Search(ctx, f.grant, SearchRequest{Query: "zeppelin", Vaults: []string{"ours/shared", "OURS/SHARED"}})
	if err != nil || len(res.Hits) != 1 || res.Hits[0].Source != "ours/shared" {
		t.Errorf("a vault named twice: %+v, %v", res.Hits, err)
	}
	qres, err := f.base.Query(ctx, f.grant, QueryRequest{SQL: "SELECT count(*) FROM documents", Vaults: []string{"ours/shared", "ours/shared"}})
	if err != nil || fmt.Sprint(qres.Rows.Rows) != "[[1]]" {
		t.Errorf("query with a vault named twice: %v, %v", qres.Rows.Rows, err)
	}
}

func TestReadEdges(t *testing.T) {
	f := newFixture(t)
	headings := strings.Repeat("# H\n", maxOutline+5)
	straddle := strings.Repeat("y", maxReadBytes-1) + "é\n"
	f.copies[1] = newCopy(t, map[string]string{
		"crlf.md":     "# A\r\n\r\nb\r\n",
		"empty.md":    "",
		"headings.md": headings,
		"straddle.md": straddle,
	})
	ctx := t.Context()

	p, err := f.base.Read(ctx, f.grant, ReadRequest{Vault: "ours/kb", Path: "crlf.md", FromLine: 2})
	if err != nil || p.Content != "\r\nb\r\n" || p.TotalLines != 3 {
		t.Errorf("CRLF: %q of %d, %v", p.Content, p.TotalLines, err)
	}
	p, err = f.base.Read(ctx, f.grant, ReadRequest{Vault: "ours/kb", Path: "empty.md"})
	if err != nil || p.Content != "" || p.TotalLines != 0 || p.LineCount != 0 {
		t.Errorf("empty: %+v, %v", p, err)
	}
	p, _ = f.base.Read(ctx, f.grant, ReadRequest{Vault: "ours/kb", Path: "headings.md", LineCount: 1})
	if len(p.Outline) != maxOutline || len(p.Warnings) != 1 {
		t.Errorf("outline of %d, warnings %v", len(p.Outline), p.Warnings)
	}
	p, _ = f.base.Read(ctx, f.grant, ReadRequest{Vault: "ours/kb", Path: "straddle.md"})
	if len(p.Content) != maxReadBytes-1 || !utf8.ValidString(p.Content) {
		t.Errorf("cut inside a character: %d bytes", len(p.Content))
	}
}

// Long headings can't make a read much bigger than 64 KiB: they're cut, and the outline counts against
// the cap.
func TestReadOutlineBytes(t *testing.T) {
	f := newFixture(t)
	var b strings.Builder
	for range maxOutline {
		b.WriteString("# " + strings.Repeat("é", 2500) + "\n" + strings.Repeat("text ", 200) + "\n")
	}
	f.copies[1] = newCopy(t, map[string]string{"long.md": b.String()})
	for _, n := range []int{1, 0} {
		p, err := f.base.Read(t.Context(), f.grant, ReadRequest{Vault: "ours/kb", Path: "long.md", LineCount: n})
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range p.Outline {
			if len(h.Text) > maxHeading || !utf8.ValidString(h.Text) || !strings.HasSuffix(h.Text, "…") {
				t.Fatalf("heading of %d bytes: %q", len(h.Text), h.Text)
			}
		}
		if size := outlineSize(p.Outline); size > maxOutlineLen || len(p.Content)+size > maxReadBytes {
			t.Errorf("line_count %d: %d bytes of content and %d of outline", n, len(p.Content), size)
		}
		if len(p.Outline) == 0 || len(p.Outline) == maxOutline || len(p.Warnings) != 1 {
			t.Errorf("line_count %d: outline of %d, warnings %v", n, len(p.Outline), p.Warnings)
		}
	}
}

func TestHistoryEdges(t *testing.T) {
	f := newFixture(t)
	f.github.history = []gh.Commit{{SHA: "c1", Message: "x\n\nSpeki-Client: " + strings.Repeat("é", 100)}}
	got, err := f.base.History(t.Context(), f.grant, HistoryRequest{Vault: "ours/kb"})
	if err != nil || len(got[0].Client) > maxClient || !utf8.ValidString(got[0].Client) {
		t.Errorf("long client: %q, %v", got[0].Client, err)
	}
	f.github.history = []gh.Commit{{SHA: "c1", Author: strings.Repeat("a", 1000), Message: strings.Repeat("é", 1000) + "\n\nbody"}}
	got, err = f.base.History(t.Context(), f.grant, HistoryRequest{Vault: "ours/kb"})
	if err != nil || len(got[0].Author) > maxAuthor || len(got[0].Message) > maxSubject || !utf8.ValidString(got[0].Message) {
		t.Errorf("long author and subject: %d and %d bytes, %v", len(got[0].Author), len(got[0].Message), err)
	}
	f.github.historyErr = fmt.Errorf("commits: %w", gh.ErrUnauthorized)
	_, err = f.base.History(t.Context(), f.grant, HistoryRequest{Vault: "ours/kb"})
	wantCode(t, err, CodeUnavailable)
}
