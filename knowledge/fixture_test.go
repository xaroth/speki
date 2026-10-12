package knowledge_test

import (
	"context"
	"crypto/sha1" //nolint:gosec // git blob IDs are SHA-1
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eve-online-tools/speki/access"
	"github.com/eve-online-tools/speki/gh"
	"github.com/eve-online-tools/speki/index"
	"github.com/eve-online-tools/speki/knowledge"
	"github.com/eve-online-tools/speki/vault"
)

// The fakes below stand in for the interfaces knowledge declares: the vault copies the library serves,
// GitHub and the change notices. Copies keep a real index; only the git tree and blobs are in memory.

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

func (c *fakeCopy) Info() vault.Vault { return c.info }

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
	return blobID(string(content)), nil
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
		c.blobs[blobID(string(content))] = content
	}
	c.trees[commit] = tree
}

// apply changes one file in the copy and applies it to the index, as a sync of a push would.
func (c *fakeCopy) apply(tb testing.TB, commit, path string, content []byte) {
	tb.Helper()
	c.setFile(commit, path, content)
	err := c.idx.Apply(tb.Context(), commit, []index.Change{{Path: path, Version: blobID(string(content)), Content: content}})
	if err != nil {
		tb.Fatalf("apply %s: %v", path, err)
	}
}

// load rebuilds the copy at commit with files. Files that aren't markdown are in the tree but not indexed.
func (c *fakeCopy) load(ctx context.Context, commit string, files map[string][]byte) error {
	c.mu.Lock()
	c.trees[commit] = maps.Clone(files)
	for _, content := range files {
		c.blobs[blobID(string(content))] = content
	}
	c.mu.Unlock()
	changes := func(yield func(index.Change, error) bool) {
		for _, p := range slices.Sorted(maps.Keys(files)) {
			if !strings.HasSuffix(p, ".md") {
				continue
			}
			if !yield(index.Change{Path: p, Version: blobID(string(files[p])), Content: files[p]}, nil) {
				return
			}
		}
	}
	return c.idx.Build(ctx, commit, iter.Seq2[index.Change, error](changes))
}

func newCopy(tb testing.TB, v vault.Vault, files map[string]string) *fakeCopy {
	tb.Helper()
	idx, err := index.Open(tb.Context(), tb.TempDir(), discard())
	if err != nil {
		tb.Fatalf("open index: %v", err)
	}
	tb.Cleanup(idx.Close)
	c := &fakeCopy{info: v, idx: idx, blobs: map[string][]byte{}, trees: map[string]map[string][]byte{}}
	raw := map[string][]byte{}
	for p, content := range files {
		raw[p] = []byte(content)
	}
	err = c.load(tb.Context(), "c1", raw)
	if err != nil {
		tb.Fatalf("build index: %v", err)
	}
	return c
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// blobID is the git blob SHA of content: a document's version.
func blobID(content string) string {
	h := sha1.New() //nolint:gosec // git blob IDs are SHA-1
	h.Write([]byte("blob " + strconv.Itoa(len(content)) + "\x00" + content))
	return hex.EncodeToString(h.Sum(nil))
}

// fakeRepo is a repository on the fake GitHub.
type fakeRepo struct {
	head  string
	files map[string][]byte
}

// fakeGitHub keeps repositories that start out as their copies, and takes commits to them.
type fakeGitHub struct {
	mu      sync.Mutex
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
	lookups                   int
	token, repo, branch, path string
	limit                     int
	history                   []gh.Commit
	historyErr                error
}

func (f *fakeGitHub) Commits(_ context.Context, token, repo, branch, path string, limit int) ([]gh.Commit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	if f.history == nil && f.historyErr == nil { // nothing set up: what was committed
		return f.log, nil
	}
	f.token, f.repo, f.branch, f.path, f.limit = token, repo, branch, path, limit
	return f.history, f.historyErr
}

// repoOf returns the repository behind a vault, starting it from the vault's copy. f.mu must be held.
func (f *fakeGitHub) repoOf(name string) *fakeRepo {
	r, ok := f.repos[name]
	if ok {
		return r
	}
	for _, v := range []vault.Vault{ours, shared, secret} {
		if v.Name == name {
			c := f.f.copy(v.RepoID)
			r = &fakeRepo{head: c.idx.Commit(), files: maps.Clone(c.tree())}
		}
	}
	f.repos[name] = r
	return r
}

// head returns the head commit of a vault's repository.
func (f *fakeGitHub) head(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.repoOf(name).head
}

// sent returns the commit requests made so far.
func (f *fakeGitHub) sent() []gh.CommitRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
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
	mu      sync.Mutex
	syncs   int
	syncErr error
	hold    chan struct{} // when set, Sync waits for it to close or ctx to end
}

func (v *fakeVaults) Copy(id int64) (knowledge.Copy, bool) {
	c := v.f.copy(id)
	if c == nil {
		return nil, false
	}
	return c, true
}

func (v *fakeVaults) setSyncErr(err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.syncErr = err
}

func (v *fakeVaults) syncCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.syncs
}

func (v *fakeVaults) Sync(ctx context.Context, id int64) error {
	v.mu.Lock()
	v.syncs++
	hold, syncErr := v.hold, v.syncErr
	v.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if syncErr != nil {
		return syncErr
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
	c := v.f.copy(id)
	if r == nil || c.idx.Commit() == head {
		return nil
	}
	return c.load(ctx, head, files)
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

func (n *fakeNotices) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.sent)
}

// heldSQL is a query gatedSandbox holds until release closes.
const heldSQL = "SELECT 'held'"

// gatedSandbox is the real sandbox, except that it holds heldSQL, so a test can keep a query running.
type gatedSandbox struct {
	*index.Sandbox
	started chan struct{} // closed when the held query starts
	release chan struct{}
}

func (s *gatedSandbox) Query(ctx context.Context, sources []index.Source, sql string) (index.Rows, error) {
	if sql == heldSQL {
		close(s.started)
		select {
		case <-s.release:
		case <-ctx.Done():
			return index.Rows{}, ctx.Err()
		}
	}
	return s.Sandbox.Query(ctx, sources, sql)
}

var (
	ours   = vault.Vault{RepoID: 1, Name: "ours/kb", DefaultBranch: "main", Config: vault.Config{Title: "KB", Description: "Our knowledge.", Guidelines: "One page per service."}}
	shared = vault.Vault{RepoID: 2, Name: "ours/shared", DefaultBranch: "main"}
	secret = vault.Vault{RepoID: 3, Name: "ours/secret", DefaultBranch: "main"}
	// building and failed are vaults the user may read that speki doesn't serve.
	building = vault.Vault{RepoID: 4, Name: "ours/new", DefaultBranch: "main", Config: vault.Config{Title: "New"}}
	failed   = vault.Vault{RepoID: 5, Name: "ours/old", DefaultBranch: "main"}
)

// The default documents of ours/kb.
const (
	readme   = "---\ntitle: About\n---\n# About\n\nThis vault holds zeppelin notes.\n"
	esiProxy = "---\ntitle: ESI proxy\ntags: [esi]\n---\n# ESI proxy\n\nThe proxy caches zeppelin responses.\n\n## Deploying\n\nRun the job.\n"
	postgres = "# Postgres\n\nZeppelin database.\n"
)

// fixedNow is the fixture's clock.
var fixedNow = time.Date(2026, 10, 9, 14, 2, 11, 0, time.UTC)

const nowStamp = "2026-10-09T14:02:11Z"

// fixture serves three vaults. The test user may read ours and shared, and write ours; secret holds a
// document only others may read.
type fixture struct {
	base    *knowledge.Base
	grant   *access.Grant
	github  *fakeGitHub
	vaults  *fakeVaults
	notices *fakeNotices
	sandbox *gatedSandbox

	mu     sync.Mutex
	copies map[int64]*fakeCopy
}

// copy returns a vault's copy, nil when speki doesn't serve it.
func (f *fixture) copy(id int64) *fakeCopy {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.copies[id]
}

func (f *fixture) setCopy(id int64, c *fakeCopy) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c == nil {
		delete(f.copies, id)
		return
	}
	f.copies[id] = c
}

// fixtureOptions change what newFixture sets up.
type fixtureOptions struct {
	kb     map[string]string // the documents of ours/kb
	logger *slog.Logger
}

type fixtureOption func(*fixtureOptions)

// withKB replaces the documents of ours/kb.
func withKB(files map[string]string) fixtureOption {
	return func(o *fixtureOptions) { o.kb = files }
}

func withLogger(l *slog.Logger) fixtureOption {
	return func(o *fixtureOptions) { o.logger = l }
}

// newFixture returns a ready fixture; its indexes close when the test ends.
func newFixture(tb testing.TB, opts ...fixtureOption) *fixture {
	tb.Helper()
	o := fixtureOptions{
		kb:     map[string]string{"README.md": readme, "services/esi-proxy.md": esiProxy, "services/db/pg.md": postgres},
		logger: discard(),
	}
	for _, opt := range opts {
		opt(&o)
	}
	f := &fixture{
		copies: map[int64]*fakeCopy{
			ours.RepoID:   newCopy(tb, ours, o.kb),
			shared.RepoID: newCopy(tb, shared, map[string]string{"zeppelin.md": "# Zeppelin\n\nShared zeppelin facts.\n"}),
			secret.RepoID: newCopy(tb, secret, map[string]string{"services/esi-proxy.md": "# Hidden\n\nThe secret zeppelin plan.\n"}),
		},
	}
	f.github = &fakeGitHub{f: f, repos: map[string]*fakeRepo{}}
	f.vaults = &fakeVaults{f: f}
	sandbox, err := index.NewSandbox()
	if err != nil {
		tb.Fatalf("NewSandbox: %v", err)
	}
	tb.Cleanup(func() { _ = sandbox.Close() })
	f.sandbox = &gatedSandbox{Sandbox: sandbox, started: make(chan struct{}), release: make(chan struct{})}
	f.notices = &fakeNotices{}
	f.base = knowledge.New(f.vaults, f.github, f.sandbox, f.notices, o.logger)
	knowledge.SetNow(f.base, func() time.Time { return fixedNow })
	f.grant = access.NewGrant(access.User{ID: 7, Login: "octocat", Token: "ghu_x", Client: "Claude Code", Write: true},
		[]vault.Vault{ours, shared}, []int64{ours.RepoID})
	return f
}

// withUnavailable gives f's user a grant that also knows building and failed, which they may read but
// speki doesn't serve. building has a copy with matching documents, which must stay out of every answer.
func withUnavailable(tb testing.TB, f *fixture) {
	tb.Helper()
	f.setCopy(building.RepoID, newCopy(tb, building, map[string]string{"zeppelin.md": "# Zeppelin\n\nNew zeppelin facts.\n"}))
	f.grant = access.NewGrant(f.grant.User(), []vault.Vault{ours, shared}, []int64{ours.RepoID},
		access.Unavailable{Vault: failed, State: access.StateFailed},
		access.Unavailable{Vault: building, State: access.StateBuilding},
	)
}

func (f *fixture) read(tb testing.TB, path string) knowledge.Page {
	tb.Helper()
	p, err := f.base.Read(tb.Context(), f.grant, knowledge.ReadRequest{Vault: ours.Name, Path: path})
	if err != nil {
		tb.Fatalf("read %s: %v", path, err)
	}
	return p
}

func (f *fixture) version(tb testing.TB, path string) string {
	tb.Helper()
	return f.read(tb, path).Document.Version
}

func (f *fixture) lastCommit(tb testing.TB) gh.CommitRequest {
	tb.Helper()
	sent := f.github.sent()
	if len(sent) == 0 {
		tb.Fatal("got no commit, want one")
	}
	return sent[len(sent)-1]
}

// change runs a WriteRequest, EditRequest, MoveRequest or DeleteRequest.
func (f *fixture) change(ctx context.Context, g *access.Grant, req any) (knowledge.Result, error) {
	switch r := req.(type) {
	case knowledge.WriteRequest:
		return f.base.Write(ctx, g, r)
	case knowledge.EditRequest:
		return f.base.Edit(ctx, g, r)
	case knowledge.MoveRequest:
		return f.base.Move(ctx, g, r)
	case knowledge.DeleteRequest:
		return f.base.Delete(ctx, g, r)
	}
	panic(fmt.Sprintf("not a change: %T", req))
}

func wantCode(tb testing.TB, err error, code knowledge.Code) {
	tb.Helper()
	_ = wantError(tb, err, code)
}

// wantError returns err as a *knowledge.Error with code, or fails.
func wantError(tb testing.TB, err error, code knowledge.Code) *knowledge.Error {
	tb.Helper()
	kerr, ok := errors.AsType[*knowledge.Error](err)
	if !ok || kerr.Code != code {
		tb.Fatalf("got error %v, want code %s", err, code)
	}
	return kerr
}

// withoutRequestID drops the Speki-Request trailer, which is random, from a commit message.
func withoutRequestID(message string) string {
	before, _, _ := strings.Cut(message, "\nSpeki-Request: ")
	return before
}
