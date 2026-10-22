package library

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/xaroth/speki/library/gitsync"
	"github.com/xaroth/speki/library/index"
	"github.com/xaroth/speki/telemetry"
	"github.com/xaroth/speki/vault"
)

// ErrRebuildNeeded means a quick sync found the index can't be brought up to date by applying changes:
// it needs a full rebuild, which is left to the poller.
var ErrRebuildNeeded = errors.New("the index needs a full rebuild")

// Sync brings a vault's copy and index up to date with GitHub.
func (l *Library) Sync(ctx context.Context, v *Vault) error {
	v.syncMu.Lock()
	defer v.syncMu.Unlock()
	return l.syncLocked(ctx, v, false)
}

// SyncChanges is Sync for a request that's waiting: it gives up waiting for the vault's sync lock when ctx
// ends, and only applies changes. A full rebuild or a fresh clone is left to the poller
// (ErrRebuildNeeded), so a write doesn't wait for one.
func (l *Library) SyncChanges(ctx context.Context, v *Vault) error {
	err := v.syncMu.LockContext(ctx)
	if err != nil {
		return fmt.Errorf("wait for the vault's sync: %w", err)
	}
	defer v.syncMu.Unlock()
	return l.syncLocked(ctx, v, true)
}

func (l *Library) syncLocked(ctx context.Context, v *Vault, quick bool) (err error) {
	ctx, span := telemetry.Tracer().Start(ctx, "library.Sync", trace.WithAttributes(attribute.String("speki.vault", v.Info().Name)))
	result := "unchanged"
	defer func() {
		switch {
		case errors.Is(err, ErrRebuildNeeded):
			result = "deferred" // to the poller, which rebuilds
		case err != nil:
			result = "failed"
			span.SetStatus(codes.Error, "sync failed")
		}
		span.SetAttributes(attribute.String("speki.result", result))
		span.End()
		syncTotal.WithLabelValues(result).Inc()
	}()
	if v.removed {
		return errRemoved
	}
	if v.mirror == nil || v.index == nil {
		return errors.New("vault isn't open")
	}

	// A fetch goes by name, and a name can point at another repository by now: renamed, with a new one
	// under the old name, before discovery has caught up. Fetch only while the name is still the vault's.
	info := v.Info()
	id, err := l.github.RepositoryID(ctx, info.InstallationID, info.Name)
	if err != nil {
		return fmt.Errorf("check repository ID: %w", err)
	}
	if id != info.RepoID {
		return fmt.Errorf("%s now names repository %d, not this vault's %d: waiting for discovery to find the new name", info.Name, id, info.RepoID)
	}

	var head string
	if v.broken.Load() {
		// The copy may not even be there after a failed fresh clone swap: don't fetch into it.
		err = errMarkedBroken
	} else {
		fetchCtx, touch, stop := stallContext(ctx, l.opts.StallTimeout)
		v.gitMu.Lock()
		v.mirror.SetSource(l.source(info)) // follows renames and transfers: the vault is its repository ID
		head, err = v.mirror.Fetch(fetchCtx, progressWriter(touch))
		v.gitMu.Unlock()
		stop()
	}
	if err == nil {
		commit := v.index.Commit()
		switch {
		case commit == head:
		case commit != "" && v.mirror.Has(commit):
			result = "applied"
			err = l.apply(ctx, v, commit, head)
		case quick:
			return ErrRebuildNeeded
		default:
			result = "rebuilt"
			err = l.build(ctx, v, head)
		}
	}
	if err == nil && v.broken.Load() {
		err = errMarkedBroken
	}
	if errors.Is(err, gitsync.ErrBroken) {
		if quick {
			return ErrRebuildNeeded // the poller repairs it
		}
		// A copy that broke while in use: clone it again, next to the old one, and rebuild from it.
		l.logger.WarnContext(ctx, "git copy is broken; cloning it again", "vault", info.Name, "err", err)
		result = "rebuilt"
		err = l.freshClone(ctx, v, "")
		if err == nil {
			head, err = v.mirror.Head()
		}
		if err == nil {
			err = l.build(ctx, v, head)
		}
	}
	if err != nil {
		return err
	}

	if !quick {
		l.compactLocked(ctx, v)
	}

	stats, err := v.index.Stats(context.WithoutCancel(ctx)) // the index caught up; say so even if ctx just ran out
	if err != nil {
		return fmt.Errorf("index stats: %w", err)
	}
	now := v.now()
	v.mu.Lock()
	v.status.Commit = head
	v.status.Documents = stats.Documents
	v.status.Bytes = stats.Bytes
	v.status.Large = stats.Documents > largeDocuments || stats.Bytes > largeBytes
	v.status.LastSync = now
	v.status.Fresh = now
	changed := v.status.State != StateServing
	v.status.State = StateServing
	v.recordLocked(nil)
	large := v.status.Large
	v.mu.Unlock()
	if changed {
		l.notify()
	}
	if large {
		l.logger.WarnContext(ctx, "vault is larger than speki is built for", "vault", v.Info().Name,
			"documents", stats.Documents, "bytes", stats.Bytes)
	}
	return nil
}

func (l *Library) apply(ctx context.Context, v *Vault, from, to string) error {
	v.gitMu.RLock()
	defer v.gitMu.RUnlock()
	diff, err := v.mirror.Diff(from, to)
	if err != nil {
		return err // gitsync says which commits
	}
	changes := make([]index.Change, 0, len(diff))
	for _, d := range diff {
		c := index.Change{Path: d.Path, Version: d.Blob}
		if d.Blob != "" {
			c.Content, err = v.mirror.Blob(d.Blob)
			if err != nil {
				return fmt.Errorf("read %s: %w", d.Path, err)
			}
		}
		changes = append(changes, c)
	}
	return v.index.Apply(ctx, to, changes)
}

func (l *Library) build(ctx context.Context, v *Vault, commit string) error {
	v.gitMu.RLock()
	defer v.gitMu.RUnlock()
	files, err := v.mirror.Files(commit)
	if err != nil {
		return fmt.Errorf("list files at %s: %w", commit, err)
	}
	ctx, touch, stop := stallContext(ctx, l.opts.StallTimeout)
	defer stop()
	return v.index.Build(ctx, commit, l.readFiles(v, files, touch))
}

func (l *Library) readFiles(v *Vault, files []gitsync.File, touch func()) iter.Seq2[index.Change, error] {
	return func(yield func(index.Change, error) bool) {
		for _, f := range files {
			touch()
			content, err := v.mirror.Blob(f.Blob)
			if !yield(index.Change{Path: f.Path, Version: f.Blob, Content: content}, err) {
				return
			}
		}
	}
}

// movedOnRetry is how long after a spoiled fresh clone the next one is tried.
const movedOnRetry = 10 * time.Minute

// errMovedOn means a fresh clone no longer has the index's commit.
var errMovedOn = errors.New("the branch moved on during the clone")

// errMarkedBroken means a read found objects missing, or a failed fresh clone swap couldn't put the old
// copy back.
var errMarkedBroken = fmt.Errorf("%w: marked broken", gitsync.ErrBroken)

// compactLocked replaces a copy with too many pack files by a fresh clone, at most once an hour, or ten
// minutes after a commit landing during the clone spoiled the last try. Called with syncMu held.
func (l *Library) compactLocked(ctx context.Context, v *Vault) {
	if v.mirror.Packs() <= l.maxPacks || v.now().Sub(v.freshCloneAt) < time.Hour {
		return
	}
	v.freshCloneAt = v.now()
	err := l.freshClone(ctx, v, v.index.Commit())
	switch {
	case errors.Is(err, errMovedOn):
		v.freshCloneAt = v.now().Add(movedOnRetry - time.Hour)
		l.logger.InfoContext(ctx, "fresh clone missed the index's commit; trying again later", "vault", v.Info().Name)
	case err != nil:
		l.logger.WarnContext(ctx, "fresh clone failed", "vault", v.Info().Name, "err", err)
	}
}

// freshClone replaces the local repository with a new clone. It clones next to the old copy and swaps the
// directories. A failed swap or reopen puts the old copy back, and v.mirror keeps reading it; if that
// fails too, the vault is marked broken. With keep set, the new copy
// must have that commit (the index's), or it's thrown away with errMovedOn: a commit landed during the
// clone, and the index could only catch up by a full rebuild. Called with syncMu held.
func (l *Library) freshClone(ctx context.Context, v *Vault, keep string) error {
	current := filepath.Join(v.dir, gitDir)
	next, old := current+".next", current+".old"
	_ = os.RemoveAll(next)
	_ = os.RemoveAll(old)
	m, err := l.openMirror(ctx, v, next)
	if err != nil {
		_ = os.RemoveAll(next)
		return fmt.Errorf("fresh clone: %w", err)
	}
	if keep != "" && !m.Has(keep) {
		_ = os.RemoveAll(next)
		return errMovedOn
	}

	v.gitMu.Lock()
	defer v.gitMu.Unlock()
	err = l.rename(current, old)
	if err != nil && !errors.Is(err, fs.ErrNotExist) { // missing after a failed put-back: nothing to keep
		_ = os.RemoveAll(next)
		return fmt.Errorf("swap fresh copy: %w", err)
	}
	err = l.rename(next, current)
	if err != nil {
		l.putBack(v, current, next, old)
		return fmt.Errorf("swap fresh copy: %w", err)
	}
	info := v.Info()
	url, auth := l.source(info)
	m, err = gitsync.OpenLocal(current, gitsync.Source{URL: url, Branch: info.DefaultBranch, Auth: auth})
	if err != nil {
		l.putBack(v, current, next, old)
		return fmt.Errorf("reopen fresh copy: %w", err)
	}
	v.mirror = m
	v.broken.Store(false)
	_ = os.RemoveAll(old)
	return nil
}

// putBack moves the old copy back after a failed swap. go-git resolves paths on every read, so v.mirror
// reads whatever is at current: if the old copy can't go back, the vault is marked broken, and the next
// sync clones it again. Called with gitMu held.
func (l *Library) putBack(v *Vault, current, next, old string) {
	_ = os.RemoveAll(next)
	_ = os.RemoveAll(current) // the fresh copy, if it got there
	err := l.rename(old, current)
	if err != nil {
		v.broken.Store(true)
	}
}

// openMirror opens or clones the vault's git copy in dir. A clone that makes no progress for the stall
// timeout is cancelled; a slow one that keeps going is left to finish.
func (l *Library) openMirror(ctx context.Context, v *Vault, dir string) (*gitsync.Mirror, error) {
	info := v.Info()
	ctx, touch, stop := stallContext(ctx, l.opts.StallTimeout)
	defer stop()
	url, auth := l.source(info)
	return gitsync.Open(ctx, dir, gitsync.Source{
		URL:      url,
		Branch:   info.DefaultBranch,
		Auth:     auth,
		Progress: progressWriter(touch),
	})
}

// source returns where a vault's copy fetches from, and the credentials, by its current name and
// installation.
func (l *Library) source(info vault.Vault) (string, gitsync.TokenSource) {
	var auth gitsync.TokenSource
	if l.opts.Tokens != nil {
		auth = l.opts.Tokens(info.InstallationID)
	}
	return l.opts.GitURL + "/" + info.Name + ".git", auth
}
