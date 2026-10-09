package vault

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/eve-online-tools/speki/gh"
)

func TestParseConfig(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    Config
		wantErr []string
	}{
		{
			name: "full",
			raw:  "title: Infra\ndescription: >\n  How we run things.\nguidelines: |\n  One doc per service.\nfuture_key: ignored\n",
			want: Config{Title: "Infra", Description: "How we run things.", Guidelines: "One doc per service."},
		},
		{name: "missing fields", raw: "guidelines: x\n", wantErr: []string{"title is required", "description is required"}},
		{name: "too long", raw: "title: T\ndescription: " + strings.Repeat("d", 301) + "\n", wantErr: []string{"description is 301 characters"}},
		{name: "not yaml", raw: "title: [\n", wantErr: []string{"speki.yaml"}},
		{name: "empty", raw: "", wantErr: []string{"title is required"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseConfig([]byte(tt.raw))
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatal("ParseConfig succeeded")
				}
				for _, w := range tt.wantErr {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("error %q doesn't mention %q", err, w)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseConfig: %v", err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("config (-want +got):\n%s", diff)
			}
		})
	}
}

// fakeGitHub is an in-memory GitHub with switches for failures.
type fakeGitHub struct {
	mu               sync.Mutex
	installations    []gh.Installation
	installationsErr error
	repos            map[int64][]gh.Repository // by installation
	reposErr         map[int64]error
	files            map[string]string // repo -> speki.yaml
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
	if path != ConfigFile {
		return nil, errors.New("unexpected file request")
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

func newFake() *fakeGitHub {
	pushed := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
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

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func names(vs []Vault) []string {
	var out []string
	for _, v := range vs {
		out = append(out, v.Name)
	}
	return out
}

func mustDiscover(t *testing.T, r *Registry) Changes {
	t.Helper()
	c, err := r.Discover(t.Context())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return c
}

func TestDiscoverFindsAllowedVaults(t *testing.T) {
	r := NewRegistry(newFake(), []int64{42}, discard())

	c := mustDiscover(t, r)

	if diff := cmp.Diff([]string{"ours/kb"}, names(c.Added)); diff != "" {
		t.Errorf("added (-want +got):\n%s", diff)
	}
	v, ok := r.Vault(100)
	if !ok || v.Config.Title != "KB" || v.InstallationID != 1 || v.DefaultBranch != "main" {
		t.Errorf("vault 100 = %+v, %v", v, ok)
	}
	if _, ok := r.Vault(200); ok {
		t.Error("a stranger's installation was used")
	}
}

func TestDiscoverReadsConfigOnlyAfterPush(t *testing.T) {
	f := newFake()
	r := NewRegistry(f, []int64{42}, discard())
	mustDiscover(t, r)
	reads := f.fileReads

	c := mustDiscover(t, r)
	if !c.Empty() || f.fileReads != reads {
		t.Errorf("unchanged round: changes %+v, %d new reads", c, f.fileReads-reads)
	}

	f.files["ours/kb"] = "title: New title\ndescription: Knowledge.\n"
	f.repos[1][0].PushedAt = f.repos[1][0].PushedAt.Add(time.Hour)
	c = mustDiscover(t, r)
	if len(c.Changed) != 1 || c.Changed[0].Config.Title != "New title" {
		t.Errorf("after push: %+v", c)
	}
}

func TestDiscoverRemovalRules(t *testing.T) {
	tests := []struct {
		name        string
		change      func(f *fakeGitHub)
		wantRemoved bool
		wantErr     bool
	}{
		{"installation list fails", func(f *fakeGitHub) { f.installationsErr = errors.New("502") }, false, true},
		{"repository list fails", func(f *fakeGitHub) { f.reposErr[1] = errors.New("502") }, false, true},
		{"installation gone", func(f *fakeGitHub) { f.reposErr[1] = gh.ErrNotFound }, true, false},
		{"installation missing from list", func(f *fakeGitHub) { f.installations = f.installations[1:] }, true, false},
		{"repository gone", func(f *fakeGitHub) { f.repos[1] = f.repos[1][1:] }, true, false},
		{"speki.yaml removed", func(f *fakeGitHub) { delete(f.files, "ours/kb"); f.repos[1][0].PushedAt = time.Now() }, true, false},
		{"speki.yaml read fails", func(f *fakeGitHub) { f.fileErr["ours/kb"] = errors.New("502"); f.repos[1][0].PushedAt = time.Now() }, false, true},
		{"speki.yaml broken", func(f *fakeGitHub) { f.files["ours/kb"] = "title: [\n"; f.repos[1][0].PushedAt = time.Now() }, false, false},
		{"installation suspended", func(f *fakeGitHub) { f.installations[0].Suspended = true }, true, false},
		{"account no longer allowed", func(f *fakeGitHub) { f.installations[0].AccountID = 99 }, true, false},
		{"default branch changed, no speki.yaml there", func(f *fakeGitHub) {
			f.repos[1][0].DefaultBranch = "trunk" // pushed_at stays the same
		}, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			r := NewRegistry(f, []int64{42}, discard())
			mustDiscover(t, r)

			tt.change(f)
			c, err := r.Discover(t.Context())

			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, want error %v", err, tt.wantErr)
			}
			_, still := r.Vault(100)
			if still == tt.wantRemoved {
				t.Errorf("vault still there = %v, want removed %v (changes %+v)", still, tt.wantRemoved, c)
			}
			if tt.wantRemoved && len(c.Removed) != 1 {
				t.Errorf("removed = %v", names(c.Removed))
			}
		})
	}
}

func TestOneInstallationFailingDoesNotStopOthers(t *testing.T) {
	f := newFake()
	f.installations[1].AccountID = 42 // both installations allowed
	r := NewRegistry(f, []int64{42}, discard())
	mustDiscover(t, r)

	f.reposErr[1] = errors.New("502")
	f.files["stranger/kb"] = "title: Renamed\ndescription: Knowledge.\n"
	f.repos[2][0].PushedAt = time.Now()
	c, err := r.Discover(t.Context())

	if err == nil {
		t.Error("no error for the failed installation")
	}
	if _, ok := r.Vault(100); !ok {
		t.Error("vault of the failed installation was dropped")
	}
	if len(c.Changed) != 1 || c.Changed[0].Config.Title != "Renamed" {
		t.Errorf("the other installation wasn't updated: %+v", c)
	}
}

func TestRepositoryMovesBetweenInstallations(t *testing.T) {
	f := newFake()
	f.installations[1].AccountID = 42
	r := NewRegistry(f, []int64{42}, discard())
	mustDiscover(t, r)

	moved := f.repos[1][0]
	moved.FullName = "stranger/moved-kb"
	f.repos[1] = f.repos[1][1:]
	f.repos[2] = append(f.repos[2], moved)
	c := mustDiscover(t, r)

	v, ok := r.Vault(100)
	if !ok || v.InstallationID != 2 || v.Name != "stranger/moved-kb" {
		t.Errorf("moved vault = %+v, %v", v, ok)
	}
	if len(c.Changed) != 1 || len(c.Removed) != 0 {
		t.Errorf("changes = %+v", c)
	}
}

func TestBrokenConfigOfNewRepoIsReadAgainOnlyAfterPush(t *testing.T) {
	f := newFake()
	f.files["ours/kb"] = "title: [\n"
	r := NewRegistry(f, []int64{42}, discard())
	mustDiscover(t, r)
	reads := f.fileReads

	mustDiscover(t, r)
	if f.fileReads != reads {
		t.Errorf("read speki.yaml again without a push")
	}
}

func TestBrokenConfigKeepsLastGood(t *testing.T) {
	f := newFake()
	r := NewRegistry(f, []int64{42}, discard())
	mustDiscover(t, r)

	f.files["ours/kb"] = "title: [\n"
	f.repos[1][0].PushedAt = time.Now()
	mustDiscover(t, r)

	v, ok := r.Vault(100)
	if !ok || v.Config.Title != "KB" {
		t.Errorf("vault = %+v, %v; want last good configuration", v, ok)
	}
	p := r.Problems()
	if len(p) != 1 || p[0].Name != "ours/kb" {
		t.Errorf("problems = %+v", p)
	}

	f.files["ours/kb"] = goodConfig
	f.repos[1][0].PushedAt = time.Now().Add(time.Minute)
	mustDiscover(t, r)
	if p := r.Problems(); len(p) != 0 {
		t.Errorf("problem not cleared: %+v", p)
	}
}

func TestNewVaultWithBrokenConfigIsNotAdded(t *testing.T) {
	f := newFake()
	f.files["ours/kb"] = "description: no title\n"
	r := NewRegistry(f, []int64{42}, discard())

	c := mustDiscover(t, r)

	if len(c.Added) != 0 {
		t.Errorf("added %v", names(c.Added))
	}
	if p := r.Problems(); len(p) != 1 || !strings.Contains(p[0].Err, "title is required") {
		t.Errorf("problems = %+v", p)
	}
}

func TestRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		r := NewRegistry(f, []int64{42}, discard())
		ctx, cancel := context.WithCancel(t.Context())
		rounds := make(chan Changes, 10)
		done := make(chan struct{})
		go func() {
			r.Run(ctx, 5*time.Minute, func(c Changes, err error) {
				if err != nil {
					t.Errorf("round failed: %v", err)
				}
				rounds <- c
			})
			close(done)
		}()

		synctest.Wait()
		if c := <-rounds; len(c.Added) != 1 {
			t.Errorf("first round: %+v", c)
		}

		f.mu.Lock()
		f.repos[1] = nil // the vault's repository disappears
		f.mu.Unlock()
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if c := <-rounds; len(c.Removed) != 1 {
			t.Errorf("second round: %+v", c)
		}

		cancel()
		<-done
	})
}
