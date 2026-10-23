package knowledge_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/xaroth/speki/auth/access"
	"github.com/xaroth/speki/gh"
	"github.com/xaroth/speki/knowledge"
	"github.com/xaroth/speki/vault"
)

func TestCommitMessage(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		message string
		client  string
		path    string // of a document to delete, to get the default message; a.md is created when empty
		want    string
	}{
		{name: "default", client: "Claude Code", want: "Create a.md\n\nSpeki-Client: Claude Code"},
		{name: "agent's first line, forged trailer dropped", message: "Fix typo\n\nSpeki-Client: forged", client: "claude.ai", want: "Fix typo\n\nSpeki-Client: claude.ai"},
		{name: "only a forged trailer", message: "Speki-Client: forged", client: "x", want: "Create a.md\n\nSpeki-Client: x"},
		{name: "control characters, no client", message: "  tabs\tand\u202econtrol\x00  ", want: "tabs andcontrol\n\nSpeki-Client: unknown"},
		{name: "long line, client with a line break", message: strings.Repeat("é", 150), client: "c\nlient", want: strings.Repeat("é", 100) + "\n\nSpeki-Client: c lient"},
		// Default messages name paths from git, which may hold line breaks and format characters.
		{name: "path with a line break", path: "a\nSpeki-Client: x.md", client: "c", want: "Delete a Speki-Client: x.md\n\nSpeki-Client: c"},
		{name: "path with a format character", path: "a\u202eb.md", client: "c", want: "Delete ab.md\n\nSpeki-Client: c"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var f *fixture
			var req any = knowledge.WriteRequest{Vault: "ours/kb", Path: "a.md", Content: "# A\n", Message: testCase.message}
			if testCase.path != "" {
				f = newFixture(t, withKB(map[string]string{testCase.path: "# A\n"}))
				req = knowledge.DeleteRequest{Vault: "ours/kb", Path: testCase.path, Version: blobID("# A\n"), Message: testCase.message}
			} else {
				f = newFixture(t, withKB(map[string]string{}))
			}
			g := access.NewGrant(access.User{ID: 7, Token: "ghu_x", Client: testCase.client, Write: true}, []vault.Vault{ours}, []int64{ours.RepoID})
			if _, err := f.change(t.Context(), g, req); err != nil {
				t.Fatalf("change: %v", err)
			}
			if got := withoutRequestID(f.lastCommit(t).Message); got != testCase.want {
				t.Errorf("got message %q, want %q", got, testCase.want)
			}
		})
	}
}

// The agent read a version GitHub has but speki's index doesn't yet: speki syncs and finds it.
func TestWriteSyncsWhenTheIndexIsBehind(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	newer := "---\ntitle: About\n---\nChanged on GitHub.\n"
	f.github.push("ours/kb", "README.md", []byte(newer))

	_, err := f.base.Write(t.Context(), f.grant, knowledge.WriteRequest{Vault: "ours/kb", Path: "README.md", Version: blobID(newer), Content: "---\ntitle: About\n---\nMine.\n"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := f.lastCommit(t).ExpectedHead; got != "push-1" {
		t.Errorf("got a commit based on %s, want push-1", got)
	}
}

// A blob the copy no longer has (a fresh clone dropped it) is fetched again rather than failing.
func TestWriteRecoversFromAMissingBlob(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.copy(ours.RepoID).failBlobs(1)
	_, err := f.base.Write(t.Context(), f.grant, knowledge.WriteRequest{Vault: "ours/kb", Path: "README.md", Version: blobID(readme), Content: "---\ntitle: About\n---\nNew.\n"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := f.vaults.syncCount(); got != 2 { // the extra one, then the one after the commit
		t.Errorf("got %d syncs, want 2", got)
	}
}

func TestWriteDuringARebuild(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.github.push("ours/kb", "README.md", []byte("# Changed\n"))
	f.vaults.setSyncErr(knowledge.ErrRebuilding)
	_, err := f.base.Write(t.Context(), f.grant, knowledge.WriteRequest{Vault: "ours/kb", Path: "README.md", Version: blobID(readme), Content: "# Mine\n"})
	kerr := wantError(t, err, knowledge.CodeUnavailable)
	if !strings.Contains(kerr.Message, "rebuilding") {
		t.Errorf("got %q, want it to say the vault is rebuilding", kerr.Message)
	}
}

func TestIndexPending(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.github.before = func(*fakeRepo) { f.vaults.setSyncErr(errors.New("fetch timed out")) }
	res, err := f.base.Write(t.Context(), f.grant, knowledge.WriteRequest{Vault: "ours/kb", Path: "new.md", Content: "# New\n"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !res.IndexPending || res.Commit != "commit-1" {
		t.Errorf("got %+v, want commit-1 with the index pending", res)
	}
}

func TestCommitRetriesWhenTheBranchMoves(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name        string
		moves       int // how many attempts find the branch moved
		req         any
		code        knowledge.Code
		wantVersion string
	}{
		{
			name:  "lands on the last attempt",
			moves: knowledge.CommitAttempts - 1,
			req:   knowledge.EditRequest{Vault: "ours/kb", Path: "README.md", OldText: "zeppelin", NewText: "airship"},
		},
		{
			name:        "keeps moving",
			moves:       knowledge.CommitAttempts,
			req:         knowledge.EditRequest{Vault: "ours/kb", Path: "README.md", OldText: "zeppelin", NewText: "airship"},
			code:        knowledge.CodeConflict,
			wantVersion: blobID(readme),
		},
		{
			name:        "keeps moving under a move",
			moves:       knowledge.CommitAttempts,
			req:         knowledge.MoveRequest{Vault: "ours/kb", From: "README.md", To: "about.md", Version: blobID(readme)},
			code:        knowledge.CodeConflict,
			wantVersion: blobID(readme), // of the source
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			moves := testCase.moves
			f.github.before = func(r *fakeRepo) {
				if moves > 0 {
					moves--
					r.head += "+someone-else"
				}
			}
			_, err := f.change(t.Context(), f.grant, testCase.req)
			if n := len(f.github.sent()); n != knowledge.CommitAttempts {
				t.Errorf("got %d commit attempts, want %d", n, knowledge.CommitAttempts)
			}
			if testCase.code == "" {
				if err != nil {
					t.Errorf("got %v, want no error", err)
				}
				return
			}
			kerr := wantError(t, err, testCase.code)
			if kerr.Version != testCase.wantVersion || !strings.Contains(kerr.Message, testCase.wantVersion) {
				t.Errorf("got %+v, want a conflict with version %s", kerr, testCase.wantVersion)
			}
		})
	}
}

// GitHub's refusals reach the agent as codes, without looking for a commit that can't have landed.
func TestCommitErrors(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name string
		err  error
		code knowledge.Code // empty: a plain error
	}{
		{name: "secret", err: &gh.SecretError{Detail: "GitHub Personal Access Token"}, code: knowledge.CodeSecretDetected},
		{name: "branch rules", err: gh.ErrBranchRules, code: knowledge.CodeVaultReadOnly},
		{name: "forbidden", err: gh.ErrForbidden, code: knowledge.CodeNoWriteAccess},
		{name: "rate limited", err: &gh.RateLimitError{RetryAfter: 30 * time.Second}, code: knowledge.CodeRateLimited},
		{name: "signed out", err: gh.ErrUnauthorized, code: knowledge.CodeUnavailable},
		{name: "refused", err: fmt.Errorf("%w: something (UNPROCESSABLE)", gh.ErrRefused)},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.github.err = testCase.err
			_, err := f.base.Write(t.Context(), f.grant, knowledge.WriteRequest{Vault: "ours/kb", Path: "new.md", Content: "# New\n"})
			if testCase.code != "" {
				wantCode(t, err, testCase.code)
			} else if _, ok := errors.AsType[*knowledge.Error](err); ok || err == nil {
				t.Errorf("got %v, want a plain error", err)
			}
			if f.github.lookups != 0 {
				t.Errorf("got %d lookups for the commit, want none", f.github.lookups)
			}
		})
	}
}

// When GitHub's answer to a commit is lost, speki looks for the commit, twice, so the agent doesn't
// retry an edit that would apply twice. synctest stands in for the wait between the lookups.
func TestLostAnswerToACommit(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name        string
		err         error // Commit fails with it, landing nothing
		lostErr     error // Commit lands, then fails with it
		wantLanded  bool
		wantLookups int
	}{
		{name: "landed", lostErr: errors.New("502 Bad Gateway"), wantLanded: true, wantLookups: 1},
		{name: "never landed", err: errors.New("connection reset"), wantLookups: 2},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t)
				f.github.err, f.github.lostErr = testCase.err, testCase.lostErr
				res, err := f.base.Edit(t.Context(), f.grant, knowledge.EditRequest{Vault: "ours/kb", Path: "README.md", OldText: "# About", NewText: "# About\n\nAn intro."})
				if f.github.lookups != testCase.wantLookups {
					t.Errorf("got %d lookups, want %d", f.github.lookups, testCase.wantLookups)
				}
				if !testCase.wantLanded {
					if _, ok := errors.AsType[*knowledge.Error](err); ok || err == nil {
						t.Errorf("got %v, want a plain error", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("Edit: %v", err)
				}
				if res.Commit != f.github.head("ours/kb") || res.Version != f.version(t, "README.md") {
					t.Errorf("got %+v, want the commit speki made, at the indexed version", res)
				}
			})
		})
	}
}

func TestConcurrentEdits(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for _, e := range []knowledge.EditRequest{
		{Path: "services/edge-proxy.md", OldText: "Run the job.", NewText: "Run the deploy job."},
		{Path: "services/edge-proxy.md", OldText: "caches", NewText: "stores"},
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
			t.Errorf("Edit: %v", err)
		}
	}
	got := f.read(t, "services/edge-proxy.md").Content
	if !strings.Contains(got, "Run the deploy job.") || !strings.Contains(got, "stores") {
		t.Errorf("got\n%s\nwant both edits", got)
	}

	// Two writes from the same version: one wins, the other conflicts.
	v := f.version(t, "README.md")
	results := make(chan error, 2)
	for _, text := range []string{"One.", "Two."} {
		wg.Go(func() {
			_, err := f.base.Write(ctx, f.grant, knowledge.WriteRequest{Vault: "ours/kb", Path: "README.md", Version: v, Content: "---\ntitle: About\n---\n" + text + "\n"})
			results <- err
		})
	}
	wg.Wait()
	close(results)
	var ok, conflicts int
	for err := range results {
		kerr, isKerr := errors.AsType[*knowledge.Error](err)
		switch {
		case err == nil:
			ok++
		case isKerr && kerr.Code == knowledge.CodeConflict:
			conflicts++
		default:
			t.Errorf("Write: %v", err)
		}
	}
	if ok != 1 || conflicts != 1 {
		t.Errorf("got %d succeeded and %d conflicted, want 1 and 1", ok, conflicts)
	}
}

// A change queued behind a slow one gives up when its request ends, or once the lock wait runs out
// while its request still has time. synctest stands in for the lock wait.
func TestWriteLockWait(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		timeout time.Duration // of the queued request; none when zero
		code    knowledge.Code
		wantErr error
	}{
		{name: "request ends first", timeout: time.Second, wantErr: context.DeadlineExceeded},
		{name: "lock wait runs out", code: knowledge.CodeUnavailable},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t)
				committing, release := make(chan struct{}), make(chan struct{})
				f.github.before = func(*fakeRepo) {
					close(committing)
					<-release
				}
				slow := make(chan error, 1)
				go func() {
					_, err := f.base.Write(t.Context(), f.grant, knowledge.WriteRequest{Vault: "ours/kb", Path: "slow.md", Content: "# Slow\n"})
					slow <- err
				}()
				<-committing

				ctx := t.Context()
				if testCase.timeout > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, testCase.timeout)
					defer cancel()
				}
				_, err := f.base.Write(ctx, f.grant, knowledge.WriteRequest{Vault: "ours/kb", Path: "queued.md", Content: "# Queued\n"})
				if testCase.wantErr != nil && !errors.Is(err, testCase.wantErr) {
					t.Errorf("got %v, want %v", err, testCase.wantErr)
				}
				if testCase.code != "" {
					kerr := wantError(t, err, testCase.code)
					if !strings.Contains(kerr.Message, "another change to ours/kb is still being committed") {
						t.Errorf("got %q, want it to name the change being committed", kerr.Message)
					}
				}
				close(release)
				if err := <-slow; err != nil {
					t.Errorf("slow write: got %v, want no error", err)
				}
			})
		})
	}
}

// A commit that lands while the sync after it is slow is a success with its SHA. The sync and the notice
// finish after the answer, and Stop waits for them. synctest stands in for the request's deadline.
func TestCommitLandsWhileItsSyncIsSlow(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.vaults.hold = make(chan struct{}) // a new document: the only sync is the one after the commit
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		res, err := f.base.Write(ctx, f.grant, knowledge.WriteRequest{Vault: "ours/kb", Path: "new.md", Content: "# New\n"})
		if err != nil {
			t.Fatalf("write whose sync is slow: %v", err)
		}
		if res.Commit != "commit-1" || !res.IndexPending {
			t.Errorf("got %+v, want commit-1 with the index pending", res)
		}
		if got := f.notices.all(); len(got) != 0 {
			t.Errorf("got notices %v before the sync finished, want none", got)
		}

		close(f.vaults.hold)
		if err := f.base.Stop(t.Context()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if diff := cmp.Diff([]string{"1:commit-1"}, f.notices.all()); diff != "" {
			t.Errorf("notices (-want +got):\n%s", diff)
		}

		// Once stopped, a change still answers; the pollers catch up with it.
		res, err = f.base.Write(t.Context(), f.grant, knowledge.WriteRequest{Vault: "ours/kb", Path: "later.md", Content: "# Later\n"})
		if err != nil || res.Commit != "commit-2" || !res.IndexPending {
			t.Errorf("write after Stop: got %+v, %v, want commit-2 with the index pending", res, err)
		}
	})
}

// guardMismatches reads speki_write_guard_mismatches_total.
func guardMismatches(tb testing.TB) float64 {
	tb.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		tb.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() == "speki_write_guard_mismatches_total" {
			return mf.GetMetric()[0].GetCounter().GetValue()
		}
	}
	tb.Fatal("got no speki_write_guard_mismatches_total, want the counter")
	return 0
}

// Before committing, the index's version is checked against git: an index that fell behind the tree must
// not let a change overwrite what it doesn't know about. Not parallel: it counts a global metric.
func TestChangeRefusesWhenTheIndexDisagreesWithGit(t *testing.T) {
	const path = "services/edge-proxy.md"
	v := blobID(edgeProxy)
	changed := "# Edge proxy\n\nChanged in git.\n"
	changes := []struct {
		name string
		req  any
	}{
		{name: "edit", req: knowledge.EditRequest{Vault: "ours/kb", Path: path, OldText: "Run the job.", NewText: "Run it.", Version: v}},
		{name: "replace", req: knowledge.WriteRequest{Vault: "ours/kb", Path: path, Content: "# Edge proxy\n\nReplaced.\n", Version: v}},
		{name: "move", req: knowledge.MoveRequest{Vault: "ours/kb", From: path, To: "services/proxy.md", Version: v}},
		{name: "delete", req: knowledge.DeleteRequest{Vault: "ours/kb", Path: path, Version: v}},
	}
	testCases := []struct {
		name    string
		drift   []byte // nil deletes the file
		version string
	}{
		{name: "changed", drift: []byte(changed), version: blobID(changed)},
		{name: "deleted", drift: nil, version: ""},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var logs syncBuffer
			f := newFixture(t, withLogger(slog.New(slog.NewTextHandler(&logs, nil))))
			f.copy(ours.RepoID).setFile("c1", path, testCase.drift)
			before := guardMismatches(t)

			for _, change := range changes {
				_, err := f.change(t.Context(), f.grant, change.req)
				kerr := wantError(t, err, knowledge.CodeConflict)
				if kerr.Version != testCase.version {
					t.Errorf("%s: got conflict version %q, want %q", change.name, kerr.Version, testCase.version)
				}
			}
			if n := len(f.github.sent()); n != 0 {
				t.Errorf("got %d commits, want none", n)
			}
			if got := guardMismatches(t) - before; got != float64(len(changes)) {
				t.Errorf("got %v guard mismatches counted, want %d", got, len(changes))
			}
			want := fmt.Sprintf("level=WARN msg=\"index and git still disagree on a document after a sync\" vault=ours/kb path=%s commit=c1 index_version=%s git_version=%s", path, v, testCase.version)
			if n := strings.Count(logs.String(), want); n != len(changes) {
				t.Errorf("got %d warnings like %q in:\n%s\nwant %d", n, want, logs.String(), len(changes))
			}
		})
	}
}

// syncBuffer is a bytes.Buffer that work after a commit may log to while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The guard checks the document at the index's commit, not at a newer one the mirror already fetched.
// Not parallel: it counts a global metric.
func TestChangeChecksTheIndexHead(t *testing.T) {
	const path = "services/edge-proxy.md"
	f := newFixture(t)
	f.copy(ours.RepoID).setFile("c2", path, []byte("# Edge proxy\n\nFetched, not indexed yet.\n"))
	before := guardMismatches(t)

	_, err := f.base.Edit(t.Context(), f.grant, knowledge.EditRequest{Vault: "ours/kb", Path: path, OldText: "Run the job.", NewText: "Run it.", Version: blobID(edgeProxy)})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if got := f.lastCommit(t).ExpectedHead; got != "c1" {
		t.Errorf("got expected head %q, want c1", got)
	}
	if got := guardMismatches(t) - before; got != 0 {
		t.Errorf("got %v guard mismatches counted, want 0", got)
	}
}
