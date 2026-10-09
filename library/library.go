// Package library keeps the local copy of every vault, a shallow git copy plus a search index, in step
// with GitHub. It decides when to clone, fetch, apply and rebuild. It doesn't check who may read what:
// its callers do.
package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/eve-online-tools/speki/gh"
	"github.com/eve-online-tools/speki/gitsync"
	"github.com/eve-online-tools/speki/vault"
)

// github is what the library needs from GitHub.
type github interface {
	BranchHead(ctx context.Context, installationID int64, repo, branch, etag string) (gh.Head, error)
}

// Options configure a Library.
type Options struct {
	DataDir string
	// GitURL is where repositories are cloned from: GitURL/owner/repo.git. Tests point it at local
	// repositories.
	GitURL string
	// Tokens returns the git credentials for an installation. Nil means no credentials.
	Tokens func(installationID int64) gitsync.TokenSource
	// Opening is how many vaults are cloned and indexed at once.
	Opening int
	// StallTimeout cancels a clone or build that makes no progress for this long. Zero means 5 minutes.
	StallTimeout time.Duration
}

// Library is the local copy of every vault.
type Library struct {
	github github
	opts   Options
	logger *slog.Logger

	mu     sync.RWMutex
	vaults map[int64]*Vault

	opening    chan struct{} // limits concurrent clones and builds
	background sync.WaitGroup
}

// New returns an empty library.
func New(github github, opts Options, logger *slog.Logger) *Library {
	if opts.Opening <= 0 {
		opts.Opening = 4
	}
	if opts.StallTimeout <= 0 {
		opts.StallTimeout = 5 * time.Minute
	}
	return &Library{
		github:  github,
		opts:    opts,
		logger:  logger,
		vaults:  make(map[int64]*Vault),
		opening: make(chan struct{}, opts.Opening),
	}
}

// Restore opens every vault that already has a usable copy and index in the data directory, so they can
// be served before GitHub has been asked anything.
func (l *Library) Restore(ctx context.Context) error {
	err := os.MkdirAll(l.opts.DataDir, 0o755)
	if err != nil {
		return fmt.Errorf("data dir: %w", err)
	}
	entries, err := os.ReadDir(l.opts.DataDir)
	if err != nil {
		return fmt.Errorf("data dir: %w", err)
	}
	for _, e := range entries {
		id, err := strconv.ParseInt(e.Name(), 10, 64)
		if err != nil || !e.IsDir() {
			continue
		}
		v, err := l.restore(ctx, id)
		if err != nil {
			l.logger.Warn("can't reuse local copy", "repo_id", id, "err", err)
			continue
		}
		l.mu.Lock()
		l.vaults[id] = v
		l.mu.Unlock()
		l.logger.Info("vault restored from disk", "vault", v.Info().Name, "commit", v.index.Commit())
	}
	return nil
}

func (l *Library) restore(ctx context.Context, id int64) (*Vault, error) {
	dir := l.vaultDir(id)
	raw, err := os.ReadFile(filepath.Join(dir, infoFile))
	if err != nil {
		return nil, err
	}
	var info vault.Vault
	err = json.Unmarshal(raw, &info)
	if err != nil {
		return nil, err
	}
	v := newVault(info, dir)
	v.syncMu.Lock()
	defer v.syncMu.Unlock()
	err = v.open(ctx, l)
	if err == nil && v.index.Commit() == "" {
		err = errors.New("index is empty")
	}
	if err != nil {
		v.close()
		return nil, err
	}
	stats, err := v.index.Stats(ctx)
	if err != nil {
		v.close()
		return nil, err
	}
	v.mu.Lock()
	v.status = Status{
		RepoID:    info.RepoID,
		Name:      info.Name,
		State:     StateServing,
		Commit:    v.index.Commit(),
		Documents: stats.Documents,
		Bytes:     stats.Bytes,
	}
	v.mu.Unlock()
	return v, nil
}

// Update applies what discovery found: new vaults are cloned and indexed in the background, changed ones
// get their new details, removed ones are closed and their files deleted.
func (l *Library) Update(ctx context.Context, changes vault.Changes) {
	for _, info := range slices.Concat(changes.Added, changes.Changed) {
		l.mu.RLock()
		v, ok := l.vaults[info.RepoID]
		l.mu.RUnlock()
		if ok {
			if v.setInfo(info) {
				l.background.Go(func() { l.reclone(ctx, v) })
			}
			continue
		}
		v = newVault(info, l.vaultDir(info.RepoID))
		l.mu.Lock()
		l.vaults[info.RepoID] = v
		l.mu.Unlock()
		l.background.Go(func() { l.initialise(ctx, v) })
	}
	for _, info := range changes.Removed {
		l.mu.Lock()
		v, ok := l.vaults[info.RepoID]
		delete(l.vaults, info.RepoID)
		l.mu.Unlock()
		if ok {
			l.background.Go(func() { l.remove(v) })
		}
	}
}

// Reconcile removes the vaults the library has that aren't in vaults, the complete list discovery found.
// It catches vaults restored from disk that are gone from GitHub.
func (l *Library) Reconcile(ctx context.Context, vaults []vault.Vault) {
	keep := make(map[int64]bool, len(vaults))
	for _, v := range vaults {
		keep[v.RepoID] = true
	}
	var gone []vault.Vault
	for _, v := range l.all() {
		if info := v.Info(); !keep[info.RepoID] {
			gone = append(gone, info)
		}
	}
	if len(gone) > 0 {
		l.Update(ctx, vault.Changes{Removed: gone})
	}
}

// Vault returns one vault's local copy, if the library has it and it can be served.
func (l *Library) Vault(repoID int64) (*Vault, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	v, ok := l.vaults[repoID]
	if !ok || v.Status().State != StateServing {
		return nil, false
	}
	return v, true
}

// WaitIdle waits until the clones and builds started so far are done.
func (l *Library) WaitIdle() { l.background.Wait() }

// Close waits for background work and closes every vault.
func (l *Library) Close() {
	l.background.Wait()
	l.mu.Lock()
	vaults := l.vaults
	l.vaults = map[int64]*Vault{}
	l.mu.Unlock()
	for _, v := range vaults {
		v.syncMu.Lock()
		v.close()
		v.syncMu.Unlock()
	}
}

func (l *Library) all() []*Vault {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return slices.Collect(maps.Values(l.vaults))
}

func (l *Library) vaultDir(id int64) string {
	return filepath.Join(l.opts.DataDir, strconv.FormatInt(id, 10))
}

// initialise clones and indexes a new vault, a few at a time.
func (l *Library) initialise(ctx context.Context, v *Vault) {
	select {
	case l.opening <- struct{}{}:
		defer func() { <-l.opening }()
	case <-ctx.Done():
		return
	}
	v.syncMu.Lock()
	defer v.syncMu.Unlock()
	if v.removed {
		return
	}
	v.setState(StateBuilding, nil)
	err := v.open(ctx, l)
	if err == nil {
		err = l.syncLocked(ctx, v)
	}
	if err != nil {
		v.setState(StateFailed, err)
		l.logger.Warn("vault failed to open", "vault", v.Info().Name, "err", err)
		return
	}
	l.logger.Info("vault ready", "vault", v.Info().Name, "commit", v.index.Commit())
}

// reclone replaces a vault's git copy, after its default branch changed.
func (l *Library) reclone(ctx context.Context, v *Vault) {
	v.syncMu.Lock()
	v.gitMu.Lock()
	if v.mirror != nil {
		v.mirror = nil
		_ = os.RemoveAll(filepath.Join(v.dir, gitDir))
	}
	v.gitMu.Unlock()
	v.syncMu.Unlock()
	l.initialise(ctx, v)
}

func (l *Library) remove(v *Vault) {
	v.syncMu.Lock()
	defer v.syncMu.Unlock()
	v.removed = true
	v.close()
	err := os.RemoveAll(v.dir)
	if err != nil {
		l.logger.Warn("can't delete removed vault", "vault", v.Info().Name, "err", err)
	}
	l.logger.Info("vault removed", "vault", v.Info().Name)
}
