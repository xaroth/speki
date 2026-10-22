package vault_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/xaroth/speki/gh"
	"github.com/xaroth/speki/vault"
)

// fakeGitHub is an in-memory GitHub with switches for failures.
type fakeGitHub struct {
	mu               sync.Mutex
	installations    []gh.Installation
	installationsErr error
	repos            map[int64][]gh.Repository // by installation
	reposErr         map[int64]error
	files            map[string]string // repo, or repo@branch off main -> speki.yaml
	fileErr          map[string]error
	fileReads        int
}

func (f *fakeGitHub) Installations(context.Context) ([]gh.Installation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.installations, f.installationsErr
}

func (f *fakeGitHub) Repositories(_ context.Context, id int64) ([]gh.Repository, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.reposErr[id]; err != nil {
		return nil, err
	}
	return f.repos[id], nil
}

func (f *fakeGitHub) File(_ context.Context, _ int64, repo, ref, path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fileReads++
	if path != vault.ConfigFile {
		return nil, fmt.Errorf("unexpected file request %q", path)
	}
	if err := f.fileErr[repo]; err != nil {
		return nil, err
	}
	key := repo
	if ref != "main" {
		key += "@" + ref
	}
	content, ok := f.files[key]
	if !ok {
		return nil, gh.ErrNotFound
	}
	return []byte(content), nil
}

const goodConfig = "title: KB\ndescription: Knowledge.\n"

var pushed = time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)

// newFakeGitHub has installation 1 on allowed account 42 with vault ours/kb (100) and plain repository
// ours/code, and installation 2 on account 7 with vault stranger/kb (200).
func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{
		installations: []gh.Installation{
			{ID: 1, AccountID: 42},
			{ID: 2, AccountID: 7},
		},
		repos: map[int64][]gh.Repository{
			1: {
				{ID: 100, FullName: "ours/kb", DefaultBranch: "main", PushedAt: pushed},
				{ID: 101, FullName: "ours/code", DefaultBranch: "main", PushedAt: pushed},
			},
			2: {{ID: 200, FullName: "stranger/kb", DefaultBranch: "main", PushedAt: pushed}},
		},
		reposErr: map[int64]error{},
		files: map[string]string{
			"ours/kb":     goodConfig,
			"stranger/kb": goodConfig,
		},
		fileErr: map[string]error{},
	}
}

// push records a push to ours/kb, so its speki.yaml is read again.
func (f *fakeGitHub) push() {
	f.repos[1][0].PushedAt = f.repos[1][0].PushedAt.Add(time.Hour)
}

func newRegistry(f *fakeGitHub) *vault.Registry {
	return vault.NewRegistry(f, []int64{42}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func names(vs []vault.Vault) []string {
	var out []string
	for _, v := range vs {
		out = append(out, v.Name)
	}
	return out
}

func mustDiscover(tb testing.TB, r *vault.Registry) vault.Changes {
	tb.Helper()
	c, err := r.Discover(tb.Context())
	if err != nil {
		tb.Fatalf("Discover: %v", err)
	}
	return c
}

func TestDiscoverFindsAllowedVaults(t *testing.T) {
	t.Parallel()
	r := newRegistry(newFakeGitHub())

	got := mustDiscover(t, r)

	kb := vault.Vault{
		RepoID:         100,
		Name:           "ours/kb",
		InstallationID: 1,
		DefaultBranch:  "main",
		Config:         vault.Config{Title: "KB", Description: "Knowledge."},
	}
	if diff := cmp.Diff(vault.Changes{Added: []vault.Vault{kb}}, got); diff != "" {
		t.Errorf("changes (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]vault.Vault{kb}, r.Vaults()); diff != "" {
		t.Errorf("vaults (-want +got):\n%s", diff)
	}
}

func TestDiscoverReadsConfigOnlyAfterPush(t *testing.T) {
	t.Parallel()
	f := newFakeGitHub()
	r := newRegistry(f)
	mustDiscover(t, r)
	reads := f.fileReads

	c := mustDiscover(t, r)
	if !c.Empty() || f.fileReads != reads {
		t.Errorf("unchanged round: got changes %+v and %d reads, want none", c, f.fileReads-reads)
	}

	f.files["ours/kb"] = "title: New title\ndescription: Knowledge.\n"
	f.push()
	c = mustDiscover(t, r)
	if len(c.Changed) != 1 || c.Changed[0].Config.Title != "New title" {
		t.Errorf("after push: got changes %+v, want ours/kb changed to New title", c)
	}
}

func TestDiscoverRemovalRules(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name        string
		change      func(f *fakeGitHub)
		wantRemoved bool
		wantErr     bool
	}{
		{name: "installation list fails", change: func(f *fakeGitHub) { f.installationsErr = errors.New("502") }, wantErr: true},
		{name: "repository list fails", change: func(f *fakeGitHub) { f.reposErr[1] = errors.New("502") }, wantErr: true},
		{name: "installation gone", change: func(f *fakeGitHub) { f.reposErr[1] = gh.ErrNotFound }, wantRemoved: true},
		{name: "installation missing from list", change: func(f *fakeGitHub) { f.installations = f.installations[1:] }, wantRemoved: true},
		{name: "repository gone", change: func(f *fakeGitHub) { f.repos[1] = f.repos[1][1:] }, wantRemoved: true},
		{name: "speki.yaml removed", change: func(f *fakeGitHub) { delete(f.files, "ours/kb"); f.push() }, wantRemoved: true},
		{name: "speki.yaml read fails", change: func(f *fakeGitHub) { f.fileErr["ours/kb"] = errors.New("502"); f.push() }, wantErr: true},
		{name: "speki.yaml broken", change: func(f *fakeGitHub) { f.files["ours/kb"] = "title: [\n"; f.push() }},
		{name: "installation suspended", change: func(f *fakeGitHub) { f.installations[0].Suspended = true }, wantRemoved: true},
		{name: "account no longer allowed", change: func(f *fakeGitHub) { f.installations[0].AccountID = 99 }, wantRemoved: true},
		{
			name:        "default branch changed, no speki.yaml there",
			change:      func(f *fakeGitHub) { f.repos[1][0].DefaultBranch = "trunk" }, // pushed_at stays the same
			wantRemoved: true,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeGitHub()
			r := newRegistry(f)
			mustDiscover(t, r)

			testCase.change(f)
			c, err := r.Discover(t.Context())

			if (err != nil) != testCase.wantErr {
				t.Errorf("got error %v, want error %v", err, testCase.wantErr)
			}
			_, kept := r.Vault(100)
			if kept == testCase.wantRemoved {
				t.Errorf("got vault kept %v, want removed %v (changes %+v)", kept, testCase.wantRemoved, c)
			}
			wantRemoved := []string(nil)
			if testCase.wantRemoved {
				wantRemoved = []string{"ours/kb"}
			}
			if diff := cmp.Diff(wantRemoved, names(c.Removed)); diff != "" {
				t.Errorf("removed (-want +got):\n%s", diff)
			}
		})
	}
}

func TestOneInstallationFailingDoesNotStopOthers(t *testing.T) {
	t.Parallel()
	f := newFakeGitHub()
	f.installations[1].AccountID = 42 // both installations allowed
	r := newRegistry(f)
	mustDiscover(t, r)

	f.reposErr[1] = errors.New("502")
	f.files["stranger/kb"] = "title: Renamed\ndescription: Knowledge.\n"
	f.repos[2][0].PushedAt = pushed.Add(time.Hour)
	c, err := r.Discover(t.Context())

	if err == nil {
		t.Error("got no error, want the failed installation's")
	}
	if _, ok := r.Vault(100); !ok {
		t.Error("got the failed installation's vault dropped, want it kept")
	}
	if len(c.Changed) != 1 || c.Changed[0].Config.Title != "Renamed" {
		t.Errorf("got changes %+v, want stranger/kb changed to Renamed", c)
	}
}

func TestRepositoryMovesBetweenInstallations(t *testing.T) {
	t.Parallel()
	f := newFakeGitHub()
	f.installations[1].AccountID = 42
	r := newRegistry(f)
	mustDiscover(t, r)

	moved := f.repos[1][0]
	moved.FullName = "stranger/moved-kb"
	f.repos[1] = f.repos[1][1:]
	f.repos[2] = append(f.repos[2], moved)
	c := mustDiscover(t, r)

	want := vault.Vault{
		RepoID:         100,
		Name:           "stranger/moved-kb",
		InstallationID: 2,
		DefaultBranch:  "main",
		Config:         vault.Config{Title: "KB", Description: "Knowledge."},
	}
	if diff := cmp.Diff(vault.Changes{Changed: []vault.Vault{want}}, c); diff != "" {
		t.Errorf("changes (-want +got):\n%s", diff)
	}
}

// An unusable speki.yaml on a new repository is that repository's problem: the round still completes, the
// problem is listed, and the file isn't fetched again until the next push.
func TestNewVaultWithUnusableConfig(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name      string
		file      string
		fileErr   error
		wantError string
	}{
		{name: "invalid", file: "description: no title\n", wantError: "title is required"},
		{name: "oversized", fileErr: fmt.Errorf("get speki.yaml: %w", gh.ErrTooLarge), wantError: "larger than speki reads"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeGitHub()
			f.files["ours/kb"] = testCase.file
			if testCase.fileErr != nil {
				f.fileErr["ours/kb"] = testCase.fileErr
			}
			r := newRegistry(f)

			c := mustDiscover(t, r)

			if !c.Empty() {
				t.Errorf("got changes %+v, want none", c)
			}
			problems := r.Problems()
			if len(problems) != 1 || problems[0].RepoID != 100 || !strings.Contains(problems[0].Error, testCase.wantError) {
				t.Errorf("got problems %+v, want one for ours/kb mentioning %q", problems, testCase.wantError)
			}
			reads := f.fileReads
			mustDiscover(t, r)
			if f.fileReads != reads {
				t.Errorf("got %d reads without a push, want 0", f.fileReads-reads)
			}
		})
	}
}

func TestBrokenConfigKeepsLastGood(t *testing.T) {
	t.Parallel()
	f := newFakeGitHub()
	r := newRegistry(f)
	mustDiscover(t, r)

	f.files["ours/kb"] = "title: [\n"
	f.push()
	mustDiscover(t, r)

	v, ok := r.Vault(100)
	if !ok || v.Config.Title != "KB" {
		t.Errorf("got vault %+v, %v, want the last good configuration", v, ok)
	}
	if p := r.Problems(); len(p) != 1 || p[0].Name != "ours/kb" {
		t.Errorf("got problems %+v, want one for ours/kb", p)
	}

	f.files["ours/kb"] = goodConfig
	f.push()
	mustDiscover(t, r)
	if p := r.Problems(); len(p) != 0 {
		t.Errorf("got problems %+v, want none", p)
	}
}

func TestRun(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFakeGitHub()
		r := newRegistry(f)
		ctx, cancel := context.WithCancel(t.Context())
		rounds := make(chan vault.Changes, 10)
		done := make(chan struct{})
		go func() {
			r.Run(ctx, 5*time.Minute, func(c vault.Changes, err error) {
				if err != nil {
					t.Errorf("round failed: %v", err)
				}
				rounds <- c
			})
			close(done)
		}()

		synctest.Wait()
		if c := <-rounds; len(c.Added) != 1 {
			t.Errorf("first round: got %+v, want one vault added", c)
		}

		f.mu.Lock()
		f.repos[1] = nil // the vault's repository disappears
		f.mu.Unlock()
		time.Sleep(5 * time.Minute) // fake time in the bubble: one tick
		synctest.Wait()
		if c := <-rounds; len(c.Removed) != 1 {
			t.Errorf("second round: got %+v, want one vault removed", c)
		}

		cancel()
		<-done
	})
}
