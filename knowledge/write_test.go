package knowledge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"golang.org/x/text/unicode/norm"

	"github.com/eve-online-tools/speki/access"
	"github.com/eve-online-tools/speki/document"
	"github.com/eve-online-tools/speki/gh"
	"github.com/eve-online-tools/speki/index"
	"github.com/eve-online-tools/speki/vault"
)

func (f *fixture) read(t *testing.T, path string) Page {
	t.Helper()
	p, err := f.base.Read(t.Context(), f.grant, ReadRequest{Vault: "ours/kb", Path: path})
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return p
}

func (f *fixture) version(t *testing.T, path string) string {
	t.Helper()
	return f.read(t, path).Document.Version
}

func (f *fixture) lastCommit(t *testing.T) gh.CommitRequest {
	t.Helper()
	if len(f.github.requests) == 0 {
		t.Fatal("no commit")
	}
	return f.github.requests[len(f.github.requests)-1]
}

func TestWriteCreates(t *testing.T) {
	f := newFixture(t)
	res, err := f.base.Write(t.Context(), f.grant, WriteRequest{
		Vault:   "ours/kb",
		Path:    "/runbooks/deploy.md",
		Content: "---\ntitle: Deploying\n---\n# Deploying\n\nRun the zeppelin job.\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "---\ntitle: Deploying\ncreated: 2026-10-09T14:02:11Z\nupdated: 2026-10-09T14:02:11Z\n---\n# Deploying\n\nRun the zeppelin job.\n"
	c := f.lastCommit(t)
	if len(c.Changes) != 1 || c.Changes[0].Path != "runbooks/deploy.md" || string(c.Changes[0].Content) != want {
		t.Errorf("commit changes = %+v", c.Changes)
	}
	if !strings.HasPrefix(c.Message, "Create runbooks/deploy.md\n\nSpeki-Client: Claude Code\nSpeki-Request: ") || c.Repo != "ours/kb" || c.Branch != "main" || c.ExpectedHead != "c1" {
		t.Errorf("commit = %+v", c)
	}
	if res.Version != blobID([]byte(want)) || res.Commit == "" || res.IndexPending {
		t.Errorf("result = %+v", res)
	}
	if len(f.notices.sent) != 1 || f.notices.sent[0] != "1:"+res.Commit {
		t.Errorf("notices = %v", f.notices.sent)
	}

	// The change is searchable right after.
	found, err := f.base.Search(t.Context(), f.grant, SearchRequest{Query: "deploying zeppelin"})
	if err != nil || len(found.Hits) == 0 || found.Hits[0].Path != "runbooks/deploy.md" || found.Hits[0].Version != res.Version {
		t.Errorf("search after write: %+v, %v", found.Hits, err)
	}
}

func TestWriteRefusesTakenPaths(t *testing.T) {
	f := newFixture(t)
	f.copies[1].setFile("c1", "huge.md", []byte("too large to index"))
	ctx := t.Context()
	content := "# New\n"

	_, err := f.base.Write(ctx, f.grant, WriteRequest{Vault: "ours/kb", Path: "README.md", Content: content})
	wantCode(t, err, CodeAlreadyExists)
	var kerr *Error
	if errors.As(err, &kerr); kerr.Version != f.version(t, "README.md") {
		t.Errorf("already_exists without the current version: %+v", kerr)
	}
	_, err = f.base.Write(ctx, f.grant, WriteRequest{Vault: "ours/kb", Path: "huge.md", Content: content})
	wantCode(t, err, CodeAlreadyExists)
	_, err = f.base.Write(ctx, f.grant, WriteRequest{Vault: "ours/kb", Path: "readme.md", Content: content})
	wantCode(t, err, CodeInvalid)
	_, err = f.base.Write(ctx, f.grant, WriteRequest{Vault: "ours/kb", Path: "Services/new.md", Content: content})
	wantCode(t, err, CodeInvalid)
	_, err = f.base.Write(ctx, f.grant, WriteRequest{Vault: "ours/kb", Path: "notes.txt", Content: content})
	wantCode(t, err, CodeInvalid)
	if len(f.github.requests) != 0 {
		t.Errorf("committed %d times", len(f.github.requests))
	}
}

func TestWriteReplaces(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	v := f.version(t, "README.md")

	res, err := f.base.Write(ctx, f.grant, WriteRequest{Vault: "ours/kb", Path: "README.md", Version: v, Content: "---\ntitle: About\n---\nNew text.\n", Message: "Rewrite the intro"})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.read(t, "README.md"); got.Content != "---\ntitle: About\nupdated: 2026-10-09T14:02:11Z\n---\nNew text.\n" || got.Document.Version != res.Version {
		t.Errorf("after replace: %q", got.Content)
	}
	if m := f.lastCommit(t).Message; !strings.HasPrefix(m, "Rewrite the intro\n\nSpeki-Client: Claude Code\n") {
		t.Errorf("message = %q", m)
	}

	_, err = f.base.Write(ctx, f.grant, WriteRequest{Vault: "ours/kb", Path: "README.md", Version: v, Content: "# Stale\n"})
	wantCode(t, err, CodeConflict)
	var kerr *Error
	if errors.As(err, &kerr); kerr.Version != res.Version {
		t.Errorf("conflict without the current version: %+v", kerr)
	}
	_, err = f.base.Write(ctx, f.grant, WriteRequest{Vault: "ours/kb", Path: "gone.md", Version: v, Content: "# Gone\n"})
	wantCode(t, err, CodeNotFound)
}

func TestWriteChecksContent(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	for name, tt := range map[string]struct {
		content string
		code    Code
	}{
		"bad status":       {"---\ntitle: A\nstatus: wip\n---\n", CodeInvalid},
		"no title":         {"just text\n", CodeInvalid},
		"broken yaml":      {"---\ntitle: [\n---\n# A\n", CodeInvalid},
		"too large":        {"# A\n" + strings.Repeat("x", 1<<20), CodeTooLarge},
		"not UTF-8":        {"# A\n\xff\n", CodeInvalid},
		"tags as a string": {"---\ntitle: A\ntags: a, b\n---\n", CodeInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.base.Write(ctx, f.grant, WriteRequest{Vault: "ours/kb", Path: "new.md", Content: tt.content})
			wantCode(t, err, tt.code)
		})
	}
}

// The agent read a version GitHub has but speki's index doesn't yet: speki syncs and finds it.
func TestWriteSyncsWhenTheIndexIsBehind(t *testing.T) {
	f := newFixture(t)
	newer := []byte("---\ntitle: About\n---\nChanged on GitHub.\n")
	f.github.push("ours/kb", "README.md", newer)

	_, err := f.base.Write(t.Context(), f.grant, WriteRequest{Vault: "ours/kb", Path: "README.md", Version: blobID(newer), Content: "---\ntitle: About\n---\nMine.\n"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.lastCommit(t).ExpectedHead, "push-") {
		t.Errorf("commit based on %s", f.lastCommit(t).ExpectedHead)
	}
}

func TestEdit(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	edit := func(req EditRequest) (Result, error) {
		req.Vault = "ours/kb"
		if req.Path == "" {
			req.Path = "services/esi-proxy.md"
		}
		return f.base.Edit(ctx, f.grant, req)
	}

	res, err := edit(EditRequest{OldText: "Run the job.", NewText: "Run the deploy job."})
	if err != nil {
		t.Fatal(err)
	}
	got := f.read(t, "services/esi-proxy.md")
	if !strings.Contains(got.Content, "Run the deploy job.") || !strings.Contains(got.Content, "updated: 2026-10-09T14:02:11Z") || got.Document.Version != res.Version {
		t.Errorf("after edit:\n%s", got.Content)
	}

	_, err = edit(EditRequest{OldText: "not there", NewText: "x"})
	wantCode(t, err, CodeInvalid)
	_, err = edit(EditRequest{OldText: "ESI proxy", NewText: "Proxy"})
	wantCode(t, err, CodeInvalid)
	if !strings.Contains(err.Error(), "2 times") {
		t.Errorf("count missing from %v", err)
	}
	_, err = edit(EditRequest{OldText: "ESI proxy", NewText: "ESI gateway", ReplaceAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.read(t, "services/esi-proxy.md"); strings.Contains(got.Content, "ESI proxy") || got.Document.Title != "ESI gateway" {
		t.Errorf("replace_all: %s", got.Content)
	}
	_, err = edit(EditRequest{OldText: "tags: [esi]", NewText: "status: wip"})
	wantCode(t, err, CodeInvalid)
	_, err = edit(EditRequest{OldText: "Postgres", NewText: "PG", Version: res.Version, Path: "services/db/pg.md"})
	wantCode(t, err, CodeConflict)
	_, err = edit(EditRequest{OldText: "", NewText: "x"})
	wantCode(t, err, CodeInvalid)
	_, err = edit(EditRequest{OldText: "x", NewText: "y", Path: "gone.md"})
	wantCode(t, err, CodeNotFound)
}

// A document whose frontmatter was broken outside speki can be fixed, but not changed while broken.
func TestEditExistingProblems(t *testing.T) {
	f := newFixture(t)
	f.copies[1] = newCopy(t, map[string]string{
		"odd.md":    "---\ntitle: Odd\nstatus: wip\n---\nBody text.\n",
		"broken.md": "---\ntitle: [\n---\nBody text.\n",
	})
	ctx := t.Context()
	_, err := f.base.Edit(ctx, f.grant, EditRequest{Vault: "ours/kb", Path: "odd.md", OldText: "Body text.", NewText: "Better text."})
	if err != nil {
		t.Errorf("edit keeping an old problem: %v", err)
	}
	_, err = f.base.Edit(ctx, f.grant, EditRequest{Vault: "ours/kb", Path: "broken.md", OldText: "Body text.", NewText: "Better text."})
	wantCode(t, err, CodeInvalid)
	_, err = f.base.Edit(ctx, f.grant, EditRequest{Vault: "ours/kb", Path: "broken.md", OldText: "title: [", NewText: "title: Fixed"})
	if err != nil {
		t.Errorf("edit fixing the block: %v", err)
	}
}

func TestMoveAndDelete(t *testing.T) {
	f := newFixture(t)
	f.copies[1] = newCopy(t, map[string]string{
		"a.md":     "# A\n",
		"b.md":     "# B\n\nSee [A](a.md).\n",
		"c.md":     "# C\n\nSee [[a]].\n",
		"taken.md": "# Taken\n",
	})
	ctx := t.Context()
	v := f.version(t, "a.md")

	_, err := f.base.Move(ctx, f.grant, MoveRequest{Vault: "ours/kb", From: "a.md", To: "z.md"})
	wantCode(t, err, CodeInvalid)
	_, err = f.base.Move(ctx, f.grant, MoveRequest{Vault: "ours/kb", From: "a.md", To: "taken.md", Version: v})
	wantCode(t, err, CodeAlreadyExists)
	_, err = f.base.Move(ctx, f.grant, MoveRequest{Vault: "ours/kb", From: "a.md", To: "z.md", Version: "0000"})
	wantCode(t, err, CodeConflict)

	res, err := f.base.Move(ctx, f.grant, MoveRequest{Vault: "ours/kb", From: "a.md", To: "archive/a.md", Version: v})
	if err != nil {
		t.Fatal(err)
	}
	c := f.lastCommit(t)
	if len(c.Changes) != 2 || c.Changes[0].Path != "a.md" || c.Changes[0].Content != nil || c.Changes[1].Path != "archive/a.md" || string(c.Changes[1].Content) != "# A\n" {
		t.Errorf("move changes = %+v", c.Changes)
	}
	if res.Version != v || strings.Join(res.LinkedFrom, ",") != "b.md,c.md" || !strings.HasPrefix(c.Message, "Move a.md to archive/a.md\n\nSpeki-Client: Claude Code\n") {
		t.Errorf("move result = %+v, message %q", res, c.Message)
	}

	// Renaming to another letter case of the same name is a move, not a collision.
	_, err = f.base.Move(ctx, f.grant, MoveRequest{Vault: "ours/kb", From: "taken.md", To: "Taken.md", Version: f.version(t, "taken.md")})
	if err != nil {
		t.Errorf("case-only rename: %v", err)
	}

	_, err = f.base.Delete(ctx, f.grant, DeleteRequest{Vault: "ours/kb", Path: "b.md"})
	wantCode(t, err, CodeInvalid)
	res, err = f.base.Delete(ctx, f.grant, DeleteRequest{Vault: "ours/kb", Path: "archive/a.md", Version: v})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.lastCommit(t).Changes) != 1 || res.Version != "" {
		t.Errorf("delete = %+v", res)
	}
	_, err = f.base.Read(ctx, f.grant, ReadRequest{Vault: "ours/kb", Path: "archive/a.md"})
	wantCode(t, err, CodeNotFound)
}

func TestWriteAccess(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	req := WriteRequest{Vault: "ours/kb", Path: "new.md", Content: "# New\n"}

	readOnly := access.NewGrant(access.User{Login: "octocat", Token: "ghu_x"}, []vault.Vault{ours, shared}, []int64{1})
	_, err := f.base.Write(ctx, readOnly, req)
	wantCode(t, err, CodeReadOnlySession)

	req.Vault = "ours/shared"
	_, err = f.base.Write(ctx, f.grant, req)
	wantCode(t, err, CodeNoWriteAccess)
	for _, err := range []error{
		func() error {
			_, err := f.base.Write(ctx, f.grant, WriteRequest{Vault: "ours/secret", Path: "x.md", Content: "# X\n"})
			return err
		}(),
		func() error {
			_, err := f.base.Edit(ctx, f.grant, EditRequest{Vault: "ours/secret", Path: "services/esi-proxy.md", OldText: "Hidden", NewText: "x"})
			return err
		}(),
		func() error {
			_, err := f.base.Move(ctx, f.grant, MoveRequest{Vault: "ours/secret", From: "services/esi-proxy.md", To: "x.md", Version: "v"})
			return err
		}(),
		func() error {
			_, err := f.base.Delete(ctx, f.grant, DeleteRequest{Vault: "ours/secret", Path: "services/esi-proxy.md", Version: "v"})
			return err
		}(),
	} {
		wantCode(t, err, CodeNotFound)
	}
	if len(f.github.requests) != 0 {
		t.Errorf("committed %d times", len(f.github.requests))
	}
}

func TestCommitRetriesWhenTheBranchMoves(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	moves := commitAttempts - 1 // every attempt but the last finds the branch moved
	f.github.before = func(r *fakeRepo) {
		if moves > 0 {
			moves--
			r.head += "+someone-else"
		}
	}
	_, err := f.base.Edit(ctx, f.grant, EditRequest{Vault: "ours/kb", Path: "README.md", OldText: "zeppelin", NewText: "airship"})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.github.requests); n != commitAttempts {
		t.Errorf("%d commit attempts, want %d", n, commitAttempts)
	}

	f.github.before = func(r *fakeRepo) { r.head += "+" }
	_, err = f.base.Edit(ctx, f.grant, EditRequest{Vault: "ours/kb", Path: "README.md", OldText: "airship", NewText: "zeppelin"})
	wantCode(t, err, CodeConflict)
	var kerr *Error
	if errors.As(err, &kerr); kerr.Version == "" || !strings.Contains(kerr.Message, kerr.Version) {
		t.Errorf("conflict without the current version: %+v", kerr)
	}
	if n := len(f.github.requests); n != 2*commitAttempts {
		t.Errorf("%d commit attempts in all, want %d", n, 2*commitAttempts)
	}
}

func TestCommitErrors(t *testing.T) {
	f := newFixture(t)
	for _, tt := range []struct {
		err  error
		code Code
	}{
		{&gh.SecretError{Detail: "GitHub Personal Access Token"}, CodeSecretDetected},
		{gh.ErrBranchRules, CodeVaultReadOnly},
		{gh.ErrForbidden, CodeNoWriteAccess},
		{&gh.RateLimitError{RetryAfter: 30 * time.Second}, CodeRateLimited},
		{gh.ErrUnauthorized, CodeUnavailable},
	} {
		f.github.err = tt.err
		_, err := f.base.Write(t.Context(), f.grant, WriteRequest{Vault: "ours/kb", Path: "new.md", Content: "# New\n"})
		wantCode(t, err, tt.code)
	}
	f.github.err = errors.New("connection reset")
	_, err := f.base.Write(t.Context(), f.grant, WriteRequest{Vault: "ours/kb", Path: "new.md", Content: "# New\n"})
	var kerr *Error
	if err == nil || errors.As(err, &kerr) {
		t.Errorf("unknown GitHub failure = %v, want a plain error", err)
	}
}

func TestIndexPending(t *testing.T) {
	f := newFixture(t)
	f.github.before = func(*fakeRepo) { f.vaults.syncErr = errors.New("fetch timed out") }
	res, err := f.base.Write(t.Context(), f.grant, WriteRequest{Vault: "ours/kb", Path: "new.md", Content: "# New\n"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IndexPending || res.Commit == "" {
		t.Errorf("result = %+v", res)
	}
}

func TestConcurrentEdits(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for _, e := range []EditRequest{
		{Path: "services/esi-proxy.md", OldText: "Run the job.", NewText: "Run the deploy job."},
		{Path: "services/esi-proxy.md", OldText: "caches", NewText: "stores"},
		{Path: "services/db/pg.md", OldText: "Zeppelin database.", NewText: "Main database."},
	} {
		wg.Go(func() {
			e.Vault = "ours/kb"
			_, err := f.base.Edit(ctx, f.grant, e)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	got := f.read(t, "services/esi-proxy.md").Content
	if !strings.Contains(got, "Run the deploy job.") || !strings.Contains(got, "stores") {
		t.Errorf("an edit was lost:\n%s", got)
	}

	// Two writes from the same version: one wins, the other conflicts.
	v := f.version(t, "README.md")
	results := make(chan error, 2)
	for _, text := range []string{"One.", "Two."} {
		wg.Go(func() {
			_, err := f.base.Write(ctx, f.grant, WriteRequest{Vault: "ours/kb", Path: "README.md", Version: v, Content: "---\ntitle: About\n---\n" + text + "\n"})
			results <- err
		})
	}
	wg.Wait()
	close(results)
	var ok, conflicts int
	for err := range results {
		var kerr *Error
		switch {
		case err == nil:
			ok++
		case errors.As(err, &kerr) && kerr.Code == CodeConflict:
			conflicts++
		default:
			t.Error(err)
		}
	}
	if ok != 1 || conflicts != 1 {
		t.Errorf("%d succeeded, %d conflicted", ok, conflicts)
	}
}

func TestCommitMessage(t *testing.T) {
	for _, tt := range []struct{ message, client, want string }{
		{"", "Claude Code", "Update a.md\n\nSpeki-Client: Claude Code"},
		{"Fix typo\n\nSpeki-Client: forged", "claude.ai", "Fix typo\n\nSpeki-Client: claude.ai"},
		{"Speki-Client: forged", "x", "Update a.md\n\nSpeki-Client: x"},
		{"  tabs\tand\u202econtrol\x00  ", "", "tabs andcontrol\n\nSpeki-Client: unknown"},
		{strings.Repeat("é", 150), "c\nlient", strings.Repeat("é", 100) + "\n\nSpeki-Client: c lient"},
	} {
		if got := commitMessage(tt.message, "Update a.md", tt.client); got != tt.want {
			t.Errorf("commitMessage(%q, %q) = %q, want %q", tt.message, tt.client, got, tt.want)
		}
	}
	// Default messages name paths from git, which may hold line breaks and format characters.
	for _, tt := range []struct{ fallback, want string }{
		{"Update a\nSpeki-Client: x.md", "Update a Speki-Client: x.md\n\nSpeki-Client: c"},
		{"Delete a\u202eb.md", "Delete ab.md\n\nSpeki-Client: c"},
	} {
		if got := commitMessage("", tt.fallback, "c"); got != tt.want {
			t.Errorf("commitMessage with fallback %q = %q, want %q", tt.fallback, got, tt.want)
		}
	}
}

func TestCreatedIsSpekis(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.copies[1] = newCopy(t, map[string]string{
		"a.md": "---\ntitle: A\ncreated: 2020-01-02T03:04:05Z\n---\nBody.\n",
	})

	_, err := f.base.Write(ctx, f.grant, WriteRequest{Vault: "ours/kb", Path: "new.md", Content: "---\ntitle: New\ncreated: 1999-01-01T00:00:00Z\n---\n"})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.read(t, "new.md").Document.Created; got != "2026-10-09T14:02:11Z" {
		t.Errorf("created on create = %s, want now", got)
	}

	_, err = f.base.Write(ctx, f.grant, WriteRequest{Vault: "ours/kb", Path: "a.md", Version: f.version(t, "a.md"), Content: "---\ntitle: A\n---\nNew body.\n"})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.read(t, "a.md").Document.Created; got != "2020-01-02T03:04:05Z" {
		t.Errorf("created after a replace that left it out = %q", got)
	}
	_, err = f.base.Edit(ctx, f.grant, EditRequest{Vault: "ours/kb", Path: "a.md", OldText: "created: 2020-01-02T03:04:05Z\n", NewText: ""})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.read(t, "a.md").Document.Created; got != "2020-01-02T03:04:05Z" {
		t.Errorf("created after an edit that removed it = %q", got)
	}
}

func TestWriteEdges(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.copies[1] = newCopy(t, map[string]string{"empty.md": "", "dir.md/inside.md": "# Inside\n"})

	_, err := f.base.Write(ctx, f.grant, WriteRequest{Vault: "ours/kb", Path: "dir.md", Content: "# Dir\n"})
	wantCode(t, err, CodeAlreadyExists)

	_, err = f.base.Move(ctx, f.grant, MoveRequest{Vault: "ours/kb", From: "empty.md", To: "still-empty.md", Version: f.version(t, "empty.md")})
	if err != nil {
		t.Fatal(err)
	}
	if c := f.lastCommit(t); c.Changes[1].Content == nil || len(c.Changes[1].Content) != 0 {
		t.Errorf("moving an empty document deletes it: %+v", c.Changes)
	}
}

// A blob the copy no longer has (a fresh clone dropped it) is fetched again rather than failing.
func TestWriteRecoversFromAMissingBlob(t *testing.T) {
	f := newFixture(t)
	v := f.version(t, "README.md")
	f.copies[1].failBlobs(1)
	_, err := f.base.Write(t.Context(), f.grant, WriteRequest{Vault: "ours/kb", Path: "README.md", Version: v, Content: "---\ntitle: About\n---\nNew.\n"})
	if err != nil {
		t.Fatal(err)
	}
	if f.vaults.syncs.Load() < 2 { // the extra one, then the one after the commit
		t.Errorf("syncs = %d", f.vaults.syncs.Load())
	}
}

func TestWriteLockHonoursTheContext(t *testing.T) {
	f := newFixture(t)
	unlock, err := f.base.writes.lock(t.Context(), ours.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err = f.base.Write(ctx, f.grant, WriteRequest{Vault: "ours/kb", Path: "new.md", Content: "# New\n"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiting on a held lock: %v", err)
	}
}

// A change queued behind a slow one hears the vault is busy once the lock wait runs out, while its
// request still has time.
func TestWriteLockWaitIsCapped(t *testing.T) {
	f := newFixture(t)
	f.base.lockWait = 50 * time.Millisecond
	committing, release := make(chan struct{}), make(chan struct{})
	f.github.before = func(*fakeRepo) {
		close(committing)
		<-release
	}
	slow := make(chan error, 1)
	go func() {
		_, err := f.base.Write(t.Context(), f.grant, WriteRequest{Vault: "ours/kb", Path: "slow.md", Content: "# Slow\n"})
		slow <- err
	}()
	<-committing
	_, err := f.base.Write(t.Context(), f.grant, WriteRequest{Vault: "ours/kb", Path: "queued.md", Content: "# Queued\n"})
	wantCode(t, err, CodeUnavailable)
	if !strings.Contains(err.Error(), "another change to ours/kb is still being committed") {
		t.Errorf("error = %v", err)
	}
	close(release)
	if err := <-slow; err != nil {
		t.Errorf("slow write: %v", err)
	}
}

// A commit that lands while the sync after it is slow is a success with its SHA. The sync and the notice
// finish after the answer, and Stop waits for them.
func TestCommitLandsWhileItsSyncIsSlow(t *testing.T) {
	f := newFixture(t)
	f.vaults.hold = make(chan struct{}) // a new document: the only sync is the one after the commit
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	res, err := f.base.Write(ctx, f.grant, WriteRequest{Vault: "ours/kb", Path: "new.md", Content: "# New\n"})
	if err != nil {
		t.Fatalf("write whose sync is slow: %v", err)
	}
	if res.Commit != f.github.repoOf("ours/kb").head || !res.IndexPending {
		t.Errorf("result = %+v", res)
	}
	f.notices.mu.Lock()
	early := len(f.notices.sent)
	f.notices.mu.Unlock()
	if early != 0 {
		t.Errorf("notice sent before the sync finished")
	}

	close(f.vaults.hold)
	err = f.base.Stop(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.notices.sent) != 1 || f.notices.sent[0] != "1:"+res.Commit {
		t.Errorf("notices = %v", f.notices.sent)
	}

	// Once stopped, a change still answers; the pollers catch up with it.
	res, err = f.base.Write(t.Context(), f.grant, WriteRequest{Vault: "ours/kb", Path: "later.md", Content: "# Later\n"})
	if err != nil || res.Commit == "" || !res.IndexPending {
		t.Errorf("write after Stop: %+v, %v", res, err)
	}
}

// Names that git stores in NFD, through the write path with the mirror's real paths.
func TestWriteNormalisation(t *testing.T) {
	f := newFixture(t)
	nfd := norm.NFD.String
	f.copies[1] = newCopy(t, map[string]string{nfd("menü/brötchen.md"): "# Brötchen\n"})
	ctx := t.Context()

	_, err := f.base.Write(ctx, f.grant, WriteRequest{Vault: "ours/kb", Path: "menü/käse.md", Content: "# Käse\n"})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.lastCommit(t).Changes[0].Path; got != nfd("menü")+"/käse.md" {
		t.Errorf("new document at %q, not in the existing NFD folder", got)
	}
	_, err = f.base.Write(ctx, f.grant, WriteRequest{Vault: "ours/kb", Path: "menü/brötchen.md", Content: "# Copy\n"})
	wantCode(t, err, CodeAlreadyExists)
}

// The grant is checked before the request itself, so a read-only session always hears so.
func TestGrantIsCheckedFirst(t *testing.T) {
	f := newFixture(t)
	readOnly := access.NewGrant(access.User{Login: "octocat"}, []vault.Vault{ours}, []int64{1})
	ctx := t.Context()
	_, err := f.base.Move(ctx, readOnly, MoveRequest{Vault: "ours/kb", From: "a.md", To: "a.md"})
	wantCode(t, err, CodeReadOnlySession)
	_, err = f.base.Delete(ctx, readOnly, DeleteRequest{Vault: "ours/kb", Path: "a.md"})
	wantCode(t, err, CodeReadOnlySession)
	_, err = f.base.Edit(ctx, readOnly, EditRequest{Vault: "ours/kb", Path: "a.md"})
	wantCode(t, err, CodeReadOnlySession)
}

func TestWritePathChecks(t *testing.T) {
	f := newFixture(t)
	f.copies[1] = newCopy(t, map[string]string{"README.md": "# R\n", "notes.txt": "x", "café.md": "# Café\n", "body.md": "# B\n\nab ab ab\n"})
	ctx := t.Context()

	// Nothing goes under a file.
	for _, p := range []string{"README.md/x.md", "notes.txt/a.md"} {
		_, err := f.base.Write(ctx, f.grant, WriteRequest{Vault: "ours/kb", Path: p, Content: "# X\n"})
		wantCode(t, err, CodeAlreadyExists)
	}
	_, err := f.base.Move(ctx, f.grant, MoveRequest{Vault: "ours/kb", From: "café.md", To: "README.md/café.md", Version: f.version(t, "café.md")})
	wantCode(t, err, CodeAlreadyExists)

	// A move to the same path in another normalisation isn't a move.
	_, err = f.base.Move(ctx, f.grant, MoveRequest{Vault: "ours/kb", From: "café.md", To: norm.NFD.String("café.md"), Version: f.version(t, "café.md")})
	wantCode(t, err, CodeInvalid)

	// Overlapping matches count: "ab ab" appears twice in "ab ab ab".
	_, err = f.base.Edit(ctx, f.grant, EditRequest{Vault: "ours/kb", Path: "body.md", OldText: "ab ab", NewText: "x"})
	wantCode(t, err, CodeInvalid)
	if len(f.github.requests) != 0 {
		t.Errorf("committed %d times", len(f.github.requests))
	}
}

// GitHub made the commit but its answer was lost: speki finds the commit and reports success, so the
// agent doesn't retry an edit that would apply twice.
func TestLostAnswerToACommit(t *testing.T) {
	f := newFixture(t)
	f.github.lostErr = errors.New("502 Bad Gateway")
	res, err := f.base.Edit(t.Context(), f.grant, EditRequest{Vault: "ours/kb", Path: "README.md", OldText: "# About", NewText: "# About\n\nAn intro."})
	if err != nil {
		t.Fatalf("edit whose answer was lost: %v", err)
	}
	if res.Commit == "" || res.Version != f.version(t, "README.md") {
		t.Errorf("result = %+v", res)
	}

	if res.Commit != f.github.repoOf("ours/kb").head {
		t.Errorf("result commit %s, want the commit speki made", res.Commit)
	}

	// A failure where nothing landed is still a failure.
	f.github.lostErr = nil
	f.github.err = errors.New("connection reset")
	_, err = f.base.Edit(t.Context(), f.grant, EditRequest{Vault: "ours/kb", Path: "README.md", OldText: "An intro.", NewText: "Intro."})
	if err == nil {
		t.Error("a commit that never landed reported success")
	}

	// A definite refusal isn't looked for at all.
	f.github.err = fmt.Errorf("%w: something (UNPROCESSABLE)", gh.ErrRefused)
	_, err = f.base.Edit(t.Context(), f.grant, EditRequest{Vault: "ours/kb", Path: "README.md", OldText: "An intro.", NewText: "Intro."})
	if err == nil {
		t.Error("a refusal reported success")
	}
}

func TestWriteDuringARebuild(t *testing.T) {
	f := newFixture(t)
	v := f.version(t, "README.md")
	f.github.push("ours/kb", "README.md", []byte("# Changed\n"))
	f.vaults.syncErr = ErrRebuilding
	_, err := f.base.Write(t.Context(), f.grant, WriteRequest{Vault: "ours/kb", Path: "README.md", Version: v, Content: "# Mine\n"})
	wantCode(t, err, CodeUnavailable)
	if !strings.Contains(err.Error(), "rebuilding") {
		t.Errorf("error = %v", err)
	}
}

func TestMoveConflictCarriesTheSourceVersion(t *testing.T) {
	f := newFixture(t)
	v := f.version(t, "README.md")
	f.github.before = func(r *fakeRepo) { r.head += "+" }
	_, err := f.base.Move(t.Context(), f.grant, MoveRequest{Vault: "ours/kb", From: "README.md", To: "about.md", Version: v})
	var kerr *Error
	if !errors.As(err, &kerr) || kerr.Code != CodeConflict || kerr.Version != v {
		t.Errorf("conflict = %+v, want the version of README.md", err)
	}
}

// A document that already has an over-long title can still be edited; a new one can't be written with
// one.
func TestLongTitles(t *testing.T) {
	f := newFixture(t)
	long := strings.Repeat("t", document.MaxTitle+1)
	f.copies[1] = newCopy(t, map[string]string{"long.md": "---\ntitle: " + long + "\n---\nBody text.\n"})
	_, err := f.base.Edit(t.Context(), f.grant, EditRequest{Vault: "ours/kb", Path: "long.md", OldText: "Body", NewText: "New"})
	if err != nil {
		t.Errorf("editing a document with a long title: %v", err)
	}
	_, err = f.base.Write(t.Context(), f.grant, WriteRequest{Vault: "ours/kb", Path: "new.md", Content: "---\ntitle: " + long + "\n---\nBody.\n"})
	wantCode(t, err, CodeInvalid)
	if err != nil && !strings.Contains(err.Error(), "longer than") {
		t.Errorf("refused for another reason: %v", err)
	}
	_, err = f.base.Write(t.Context(), f.grant, WriteRequest{Vault: "ours/kb", Path: "ok.md", Content: "---\ntitle: " + long[1:] + "\n---\nBody.\n"})
	if err != nil {
		t.Errorf("writing a title of exactly %d bytes: %v", document.MaxTitle, err)
	}
}

// apply changes one file in the copy and applies it to the index, as a sync of a push would.
func (c *fakeCopy) apply(t *testing.T, commit, path string, content []byte) {
	t.Helper()
	c.setFile(commit, path, content)
	err := c.idx.Apply(t.Context(), commit, []index.Change{{Path: path, Version: blobID(content), Content: content}})
	if err != nil {
		t.Fatal(err)
	}
}

// A push that leaves a document unparseable must not let an agent overwrite it with the version it read
// before. Delete with the current version is the only change allowed.
func TestBrokenDocument(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	const path = "services/esi-proxy.md"
	old := f.version(t, path)
	bad := []byte("---\ntitle: ESI proxy\n---\n# ESI proxy\n\nA human's edit \xff\n")
	f.copies[1].apply(t, "c2", path, bad)
	now := blobID(bad)

	_, err := f.base.Read(ctx, f.grant, ReadRequest{Vault: "ours/kb", Path: path})
	wantCode(t, err, CodeBroken)
	var kerr *Error
	if errors.As(err, &kerr); kerr.Version != now || !strings.Contains(kerr.Message, "not UTF-8") {
		t.Errorf("read error = %+v", kerr)
	}

	content := "---\ntitle: ESI proxy\n---\nOverwritten.\n"
	changes := map[string]func(version string) error{
		"edit": func(v string) error {
			_, err := f.base.Edit(ctx, f.grant, EditRequest{Vault: "ours/kb", Path: path, OldText: "ESI", NewText: "X", Version: v})
			return err
		},
		"replace": func(v string) error {
			_, err := f.base.Write(ctx, f.grant, WriteRequest{Vault: "ours/kb", Path: path, Content: content, Version: v})
			return err
		},
		"create": func(string) error {
			_, err := f.base.Write(ctx, f.grant, WriteRequest{Vault: "ours/kb", Path: path, Content: content})
			return err
		},
		"move": func(v string) error {
			_, err := f.base.Move(ctx, f.grant, MoveRequest{Vault: "ours/kb", From: path, To: "x.md", Version: v})
			return err
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			for _, v := range []string{old, now} {
				wantCode(t, change(v), CodeBroken)
			}
		})
	}
	if len(f.github.requests) != 0 {
		t.Fatalf("committed %d times", len(f.github.requests))
	}

	_, err = f.base.Delete(ctx, f.grant, DeleteRequest{Vault: "ours/kb", Path: path, Version: old})
	wantCode(t, err, CodeConflict)
	_, err = f.base.Delete(ctx, f.grant, DeleteRequest{Vault: "ours/kb", Path: path, Version: now})
	if err != nil {
		t.Fatal(err)
	}
	if c := f.lastCommit(t); len(c.Changes) != 1 || c.Changes[0].Path != path || c.Changes[0].Content != nil {
		t.Errorf("delete changes = %+v", c.Changes)
	}
}

// Before committing, the index's version is checked against git: an index that fell behind the tree must
// not let a change overwrite what it doesn't know about.
func TestChangeRefusesWhenTheIndexDisagreesWithGit(t *testing.T) {
	const path = "services/esi-proxy.md"
	changed := []byte("# ESI proxy\n\nChanged in git.\n")
	for _, tt := range []struct {
		name    string
		drift   []byte // nil deletes the file
		version string
	}{
		{"changed", changed, blobID(changed)},
		{"deleted", nil, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			var logs bytes.Buffer
			f.base.logger = slog.New(slog.NewTextHandler(&logs, nil))
			ctx := t.Context()
			v := f.version(t, path)
			f.copies[1].setFile("c1", path, tt.drift)
			before := testutil.ToFloat64(guardMismatches)

			changes := map[string]func() error{
				"edit": func() error {
					_, err := f.base.Edit(ctx, f.grant, EditRequest{Vault: "ours/kb", Path: path, OldText: "Run the job.", NewText: "Run it.", Version: v})
					return err
				},
				"replace": func() error {
					_, err := f.base.Write(ctx, f.grant, WriteRequest{Vault: "ours/kb", Path: path, Content: "# ESI proxy\n\nReplaced.\n", Version: v})
					return err
				},
				"move": func() error {
					_, err := f.base.Move(ctx, f.grant, MoveRequest{Vault: "ours/kb", From: path, To: "services/proxy.md", Version: v})
					return err
				},
				"delete": func() error {
					_, err := f.base.Delete(ctx, f.grant, DeleteRequest{Vault: "ours/kb", Path: path, Version: v})
					return err
				},
			}
			for name, change := range changes {
				err := change()
				wantCode(t, err, CodeConflict)
				var kerr *Error
				if errors.As(err, &kerr); kerr.Version != tt.version {
					t.Errorf("%s: conflict version = %q, want %q", name, kerr.Version, tt.version)
				}
			}
			if len(f.github.requests) != 0 {
				t.Errorf("committed %d times", len(f.github.requests))
			}
			if got := testutil.ToFloat64(guardMismatches) - before; got != float64(len(changes)) {
				t.Errorf("guard mismatches counted %v, want %d", got, len(changes))
			}
			want := fmt.Sprintf("level=WARN msg=\"index and git still disagree on a document after a sync\" vault=ours/kb path=%s commit=c1 index_version=%s git_version=%s", path, v, tt.version)
			if n := strings.Count(logs.String(), want); n != len(changes) {
				t.Errorf("%d warnings like %q in:\n%s", n, want, logs.String())
			}
		})
	}
}

// The guard checks the document at the index's commit, not at a newer one the mirror already fetched.
func TestChangeChecksTheIndexHead(t *testing.T) {
	const path = "services/esi-proxy.md"
	f := newFixture(t)
	v := f.version(t, path)
	f.copies[1].setFile("c2", path, []byte("# ESI proxy\n\nFetched, not indexed yet.\n"))
	before := testutil.ToFloat64(guardMismatches)

	_, err := f.base.Edit(t.Context(), f.grant, EditRequest{Vault: "ours/kb", Path: path, OldText: "Run the job.", NewText: "Run it.", Version: v})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.lastCommit(t).ExpectedHead; got != "c1" {
		t.Errorf("expected head = %q, want c1", got)
	}
	if got := testutil.ToFloat64(guardMismatches) - before; got != 0 {
		t.Errorf("guard mismatches counted %v", got)
	}
}
