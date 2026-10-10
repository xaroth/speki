package knowledge

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
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"

	"github.com/eve-online-tools/speki/access"
	"github.com/eve-online-tools/speki/gh"
	"github.com/eve-online-tools/speki/index"
	"github.com/eve-online-tools/speki/vault"
)

// fakeCopy is a vault copy with a real index and blobs in memory.
type fakeCopy struct {
	idx   *index.Index
	blobs map[string][]byte
}

func (c *fakeCopy) Index() *index.Index { return c.idx }

func (c *fakeCopy) Blob(sha string) ([]byte, error) {
	b, ok := c.blobs[sha]
	if !ok {
		return nil, errors.New("object not found")
	}
	return b, nil
}

func blobID(content []byte) string {
	h := sha1.New() //nolint:gosec // git blob IDs are SHA-1
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

func newCopy(t *testing.T, files map[string]string) *fakeCopy {
	t.Helper()
	idx, err := index.Open(t.Context(), t.TempDir(), discard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(idx.Close)
	c := &fakeCopy{idx: idx, blobs: map[string][]byte{}}
	changes := func(yield func(index.Change, error) bool) {
		for _, p := range slices.Sorted(maps.Keys(files)) {
			content := []byte(files[p])
			c.blobs[blobID(content)] = content
			if !yield(index.Change{Path: p, Version: blobID(content), Content: content}, nil) {
				return
			}
		}
	}
	err = idx.Build(t.Context(), "c1", iter.Seq2[index.Change, error](changes))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type fakeHistorian struct {
	token, repo, branch, path string
	limit                     int
	commits                   []gh.Commit
	err                       error
}

func (f *fakeHistorian) Commits(_ context.Context, token, repo, branch, path string, limit int) ([]gh.Commit, error) {
	f.token, f.repo, f.branch, f.path, f.limit = token, repo, branch, path, limit
	return f.commits, f.err
}

var (
	ours   = vault.Vault{RepoID: 1, Name: "ours/kb", DefaultBranch: "main", Config: vault.Config{Title: "KB", Description: "Our knowledge.", Guidelines: "One page per service."}}
	shared = vault.Vault{RepoID: 2, Name: "ours/shared", DefaultBranch: "main"}
	secret = vault.Vault{RepoID: 3, Name: "ours/secret", DefaultBranch: "main"}
)

// fixture serves three vaults. The test user may read ours and shared, and write ours; secret holds a
// document only others may read.
type fixture struct {
	base    *Base
	grant   *access.Grant
	copies  map[int64]*fakeCopy
	history *fakeHistorian
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		copies: map[int64]*fakeCopy{
			1: newCopy(t, map[string]string{
				"README.md":             "---\ntitle: About\n---\n# About\n\nThis vault holds zeppelin notes.\n",
				"services/esi-proxy.md": "---\ntitle: ESI proxy\ntags: [esi]\n---\n# ESI proxy\n\nThe proxy caches zeppelin responses.\n\n## Deploying\n\nRun the job.\n",
				"services/db/pg.md":     "# Postgres\n\nZeppelin database.\n",
			}),
			2: newCopy(t, map[string]string{"zeppelin.md": "# Zeppelin\n\nShared zeppelin facts.\n"}),
			3: newCopy(t, map[string]string{"services/esi-proxy.md": "# Hidden\n\nThe secret zeppelin plan.\n"}),
		},
		history: &fakeHistorian{},
	}
	f.base = New(func(id int64) (Copy, bool) {
		c, ok := f.copies[id]
		return c, ok
	}, f.history, discard())
	f.grant = access.NewGrant(access.User{ID: 7, Login: "octocat", Token: "ghu_x", Write: true},
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

	delete(f.copies, 2) // readable, but not served right now
	got, _ = f.base.Vaults(t.Context(), f.grant)
	if len(got) != 1 {
		t.Errorf("unserved vault listed: %+v", got)
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
	wantCode(t, err, NotFound)
	_, err = f.base.List(ctx, f.grant, ListRequest{Vault: "ours/kb", Path: "services", Cursor: "README.md"})
	wantCode(t, err, Invalid)
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
		if !errors.As(err, &kerr) || kerr.Code != Invalid {
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
	wantCode(t, err, Invalid)
	_, err = f.base.Read(ctx, f.grant, ReadRequest{Vault: "ours/kb", Path: "nope.md"})
	wantCode(t, err, NotFound)
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
	var kerr *Error
	if errors.As(err, &kerr) {
		t.Errorf("a missing blob is speki's problem, not the agent's: %v", err)
	}
	c.blobs[doc.Version] = gone
}

func TestHistory(t *testing.T) {
	f := newFixture(t)
	f.history.commits = []gh.Commit{
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
	h := f.history
	if h.token != "ghu_x" || h.repo != "ours/kb" || h.branch != "main" || h.path != "a.md" || h.limit != 10 {
		t.Errorf("asked GitHub for %+v", h)
	}

	f.history.err = gh.ErrNotFound
	_, err = f.base.History(t.Context(), f.grant, HistoryRequest{Vault: "ours/kb"})
	wantCode(t, err, NotFound)
	_, err = f.base.History(t.Context(), f.grant, HistoryRequest{Vault: "ours/kb", Limit: 51})
	wantCode(t, err, Invalid)
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
	wantCode(t, err, NotFound)
	_, err = f.base.Search(ctx, f.grant, SearchRequest{Query: hidden, Vaults: []string{"ours/kb", secret.Name}})
	wantCode(t, err, NotFound)
	_, err = f.base.List(ctx, f.grant, ListRequest{Vault: secret.Name})
	wantCode(t, err, NotFound)
	_, err = f.base.Read(ctx, f.grant, ReadRequest{Vault: secret.Name, Path: "services/esi-proxy.md"})
	wantCode(t, err, NotFound)
	_, err = f.base.History(ctx, f.grant, HistoryRequest{Vault: secret.Name})
	wantCode(t, err, NotFound)
	if f.history.repo != "" {
		t.Error("asked GitHub about the secret vault")
	}

	// A readable vault's document at the same path is that vault's, not the secret one.
	p, err := f.base.Read(ctx, f.grant, ReadRequest{Vault: "ours/kb", Path: "services/esi-proxy.md"})
	if err != nil || strings.Contains(p.Content, hidden) {
		t.Errorf("read: %q, %v", p.Content, err)
	}
	// Names are matched exactly.
	_, err = f.base.Read(ctx, f.grant, ReadRequest{Vault: "OURS/SECRET", Path: "services/esi-proxy.md"})
	wantCode(t, err, NotFound)
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

func TestHistoryEdges(t *testing.T) {
	f := newFixture(t)
	f.history.commits = []gh.Commit{{SHA: "c1", Message: "x\n\nSpeki-Client: " + strings.Repeat("é", 100)}}
	got, err := f.base.History(t.Context(), f.grant, HistoryRequest{Vault: "ours/kb"})
	if err != nil || len(got[0].Client) > maxClient || !utf8.ValidString(got[0].Client) {
		t.Errorf("long client: %q, %v", got[0].Client, err)
	}
	f.history.err = fmt.Errorf("commits: %w", gh.ErrUnauthorized)
	_, err = f.base.History(t.Context(), f.grant, HistoryRequest{Vault: "ours/kb"})
	wantCode(t, err, Unavailable)
}
