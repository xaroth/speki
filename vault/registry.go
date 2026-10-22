package vault

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/xaroth/speki/gh"
)

// Vault is a repository that holds knowledge.
type Vault struct {
	RepoID         int64  // the key: survives renames and transfers
	Name           string // owner/repo, what people and agents use
	InstallationID int64
	DefaultBranch  string
	Config         Config
}

// Changes is what one discovery round found.
type Changes struct {
	Added   []Vault
	Changed []Vault // installation, name, branch or configuration differ
	Removed []Vault
}

// Empty reports whether nothing changed.
func (c Changes) Empty() bool {
	return len(c.Added) == 0 && len(c.Changed) == 0 && len(c.Removed) == 0
}

// Problem is a speki.yaml that couldn't be used.
type Problem struct {
	RepoID int64  `json:"repo_id"`
	Name   string `json:"name"`
	Error  string `json:"error"`
}

// githubAPI is what discovery needs from GitHub.
type githubAPI interface {
	Installations(ctx context.Context) ([]gh.Installation, error)
	Repositories(ctx context.Context, installationID int64) ([]gh.Repository, error)
	File(ctx context.Context, installationID int64, repo, ref, path string) ([]byte, error)
	AppOwner(ctx context.Context) (int64, error)
}

// Registry holds the current set of vaults.
type Registry struct {
	github githubAPI
	logger *slog.Logger

	round sync.Mutex // one discovery round at a time
	// allowed holds the account IDs whose installations count. Empty until Run finds the app's owner, when
	// none were configured. Guarded by round.
	allowed map[int64]bool
	// checked holds, per repository, what it looked like when its speki.yaml was last read. Only
	// discover uses it, under round.
	checked map[int64]checkedRepo

	mu       sync.RWMutex // guards vaults and problems, which readers use
	vaults   map[int64]Vault
	problems map[int64]Problem
}

type checkedRepo struct {
	pushedAt time.Time
	branch   string
}

// NewRegistry returns an empty registry. Only installations on the allowed accounts are used; with none,
// those on the account that owns the app.
func NewRegistry(github githubAPI, allowedAccounts []int64, logger *slog.Logger) *Registry {
	allowed := make(map[int64]bool, len(allowedAccounts))
	for _, id := range allowedAccounts {
		allowed[id] = true
	}
	return &Registry{
		github:   github,
		allowed:  allowed,
		logger:   logger,
		vaults:   make(map[int64]Vault),
		checked:  make(map[int64]checkedRepo),
		problems: make(map[int64]Problem),
	}
}

// Vaults returns the current vaults, sorted by name.
func (r *Registry) Vaults() []Vault {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.SortedFunc(maps.Values(r.vaults), func(a, b Vault) int { return cmp.Compare(a.Name, b.Name) })
}

// Vault returns one vault by repository ID.
func (r *Registry) Vault(repoID int64) (Vault, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.vaults[repoID]
	return v, ok
}

// Problems returns the speki.yaml files that couldn't be used, sorted by name.
func (r *Registry) Problems() []Problem {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.SortedFunc(maps.Values(r.problems), func(a, b Problem) int { return cmp.Compare(a.Name, b.Name) })
}

// Run discovers vaults now and then every interval until ctx ends, passing every round's outcome to
// onRound. A failed round still carries the changes from the requests that succeeded. Without allowed
// accounts, Run first asks GitHub for the app's owner until it answers, passing each failed attempt to
// onRound as a failed round.
func (r *Registry) Run(ctx context.Context, interval time.Duration, onRound func(Changes, error)) {
	if !r.findOwner(ctx, onRound) {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		changes, err := r.discover(ctx)
		if ctx.Err() != nil {
			return
		}
		onRound(changes, err)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// findOwner allows the account that owns the app when no accounts were configured, retrying with backoff.
// It reports whether the allowed accounts are known before ctx ended.
func (r *Registry) findOwner(ctx context.Context, onRound func(Changes, error)) bool {
	r.round.Lock()
	known := len(r.allowed) > 0
	r.round.Unlock()
	if known {
		return true
	}
	wait := time.Second
	for {
		owner, err := r.github.AppOwner(ctx)
		if err == nil {
			r.round.Lock()
			r.allowed = map[int64]bool{owner: true}
			r.round.Unlock()
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		onRound(Changes{}, fmt.Errorf("find the app's owner: %w", err))
		select {
		case <-ctx.Done():
			return false
		case <-time.After(wait):
		}
		wait = min(wait*2, 5*time.Minute)
	}
}

// discover runs one discovery round. A vault is only removed on a definite answer: its installation or
// repository is gone, it no longer has speki.yaml, or its account isn't allowed. A failed request changes
// nothing about the vaults it concerns. The returned error describes failed requests; Changes is valid
// either way.
func (r *Registry) discover(ctx context.Context) (Changes, error) {
	r.round.Lock()
	defer r.round.Unlock()

	installations, err := r.github.Installations(ctx)
	if err != nil {
		return Changes{}, err
	}

	r.mu.RLock()
	prev := maps.Clone(r.vaults)
	problems := maps.Clone(r.problems)
	r.mu.RUnlock()

	next := make(map[int64]Vault)
	keep := func(installationID int64) { // carry over this installation's vaults unchanged
		for id, v := range prev {
			if v.InstallationID == installationID {
				next[id] = v
			}
		}
	}

	var errs []error
	seen := make(map[int64]bool) // repositories listed this round
	for _, in := range installations {
		if !r.allowed[in.AccountID] || in.Suspended {
			continue
		}
		repos, err := r.github.Repositories(ctx, in.ID)
		if errors.Is(err, gh.ErrNotFound) {
			continue // the installation is gone
		}
		if err != nil {
			errs = append(errs, err)
			keep(in.ID)
			continue
		}
		for _, repo := range repos {
			seen[repo.ID] = true
			v, ok, err := r.discoverRepo(ctx, in.ID, repo, prev, problems)
			if err != nil {
				errs = append(errs, err)
			}
			if ok {
				next[repo.ID] = v
			}
		}
	}

	if len(errs) == 0 { // with every installation listed, unseen repositories are gone
		for id := range problems {
			if !seen[id] {
				delete(problems, id)
			}
		}
		for id := range r.checked {
			if !seen[id] {
				delete(r.checked, id)
			}
		}
	}
	changes := diff(prev, next)
	r.mu.Lock()
	r.vaults = next
	r.problems = problems
	r.mu.Unlock()
	return changes, errors.Join(errs...)
}

// discoverRepo decides whether repo is (still) a vault. It reads speki.yaml only when the repository was
// pushed to since the last read, so large installations don't cost a request per repository per round.
// It records unusable configurations in problems.
func (r *Registry) discoverRepo(
	ctx context.Context,
	installationID int64,
	repo gh.Repository,
	vaults map[int64]Vault,
	problems map[int64]Problem,
) (Vault, bool, error) {
	prev, wasVault := vaults[repo.ID]
	v := Vault{
		RepoID:         repo.ID,
		Name:           repo.FullName,
		InstallationID: installationID,
		DefaultBranch:  repo.DefaultBranch,
		Config:         prev.Config,
	}

	now := checkedRepo{pushedAt: repo.PushedAt, branch: repo.DefaultBranch}
	last, seen := r.checked[repo.ID]
	if seen && last.pushedAt.Equal(now.pushedAt) && last.branch == now.branch {
		return v, wasVault, nil
	}

	raw, err := r.github.File(ctx, installationID, repo.FullName, repo.DefaultBranch, ConfigFile)
	if errors.Is(err, gh.ErrNotFound) {
		r.checked[repo.ID] = now
		delete(problems, repo.ID)
		return Vault{}, false, nil
	}
	if errors.Is(err, gh.ErrTooLarge) {
		err = fmt.Errorf("%s is larger than speki reads: %w", ConfigFile, err)
	} else if err != nil {
		return v, wasVault, err // try again next round
	}
	r.checked[repo.ID] = now

	cfg := Config{}
	if err == nil {
		owner, _, _ := strings.Cut(repo.FullName, "/")
		cfg, err = ParseConfig(raw, owner)
	}
	if err != nil {
		problems[repo.ID] = Problem{RepoID: repo.ID, Name: repo.FullName, Error: err.Error()}
		r.logger.WarnContext(ctx, "unusable vault configuration", "vault", repo.FullName, "err", err)
		return v, wasVault, nil // an existing vault keeps its last good configuration
	}
	delete(problems, repo.ID)
	v.Config = cfg
	return v, true, nil
}

func diff(prev, next map[int64]Vault) Changes {
	var c Changes
	for id, v := range next {
		old, ok := prev[id]
		switch {
		case !ok:
			c.Added = append(c.Added, v)
		case !reflect.DeepEqual(old, v):
			c.Changed = append(c.Changed, v)
		}
	}
	for id, v := range prev {
		if _, ok := next[id]; !ok {
			c.Removed = append(c.Removed, v)
		}
	}
	byName := func(a, b Vault) int { return cmp.Compare(a.Name, b.Name) }
	slices.SortFunc(c.Added, byName)
	slices.SortFunc(c.Changed, byName)
	slices.SortFunc(c.Removed, byName)
	return c
}
