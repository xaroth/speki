package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/eve-online-tools/speki/gitsync"
	"github.com/eve-online-tools/speki/index"
	"github.com/eve-online-tools/speki/vault"
)

const (
	infoFile = "vault.json" // the vault's details, so a restart can serve it before asking GitHub
	gitDir   = "git"
	indexDir = "index"

	// maxPacks is how many pack files a copy may collect before it's cloned fresh. Every fetch adds one.
	maxPacks = 50

	// Speki is built for vaults up to this size. Larger ones work, with a warning.
	largeDocuments = 50_000
	largeBytes     = 500 << 20
)

// State is where a vault's local copy stands.
type State string

// The states a vault goes through.
const (
	StateBuilding State = "building"
	StateServing  State = "serving"
	StateFailed   State = "failed"
)

// Status describes a vault's local copy, for the status page and metrics.
type Status struct {
	RepoID    int64     `json:"repo_id"`
	Name      string    `json:"name"`
	State     State     `json:"state"`
	Commit    string    `json:"commit"`
	Documents int       `json:"documents"`
	Bytes     int64     `json:"bytes"`
	Large     bool      `json:"large"` // larger than speki is built for
	LastSync  time.Time `json:"last_sync"`
	// Fresh is when speki last confirmed the index matches GitHub.
	Fresh time.Time `json:"fresh"`
	Error string    `json:"error,omitempty"`
}

// Vault is one vault's local copy: a shallow git copy and a search index.
type Vault struct {
	dir string

	mu       sync.Mutex // guards the fields up to syncMu
	info     vault.Vault
	status   Status
	etag     string // of the last branch lookup
	remote   string // the commit GitHub last reported
	failures int
	retryAt  time.Time // when a failed vault, or a failing sync, is tried again

	syncMu       sync.Mutex // one open, sync, reclone or removal at a time; guards the fields below
	removed      bool
	freshCloneAt time.Time    // last attempt, so a failing fresh clone isn't retried on every sync
	gitMu        sync.RWMutex // fetches exclude reads of the git copy
	mirror       *gitsync.Mirror
	index        *index.Index // set once by open, then never changed
}

func newVault(info vault.Vault, dir string) *Vault {
	return &Vault{
		dir:    dir,
		info:   info,
		status: Status{RepoID: info.RepoID, Name: info.Name, State: StateBuilding},
	}
}

// Info returns the vault's details as discovery last saw them.
func (v *Vault) Info() vault.Vault {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.info
}

// Status returns the state of the local copy.
func (v *Vault) Status() Status {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.status
}

// Index returns the vault's search index. Library only hands out vaults whose index is open; after the
// vault is removed, the index answers with index.ErrClosed.
func (v *Vault) Index() *index.Index { return v.index }

// Blob returns a file's content from the git copy.
func (v *Vault) Blob(sha string) ([]byte, error) {
	v.gitMu.RLock()
	defer v.gitMu.RUnlock()
	if v.mirror == nil {
		return nil, errors.New("no git copy")
	}
	return v.mirror.Blob(sha)
}

// setInfo records new details and reports whether the git copy must be cloned again: it follows one
// branch, and the default branch changed.
func (v *Vault) setInfo(info vault.Vault) bool {
	v.mu.Lock()
	reclone := info.DefaultBranch != v.info.DefaultBranch
	v.info = info
	v.status.Name = info.Name
	if reclone {
		v.etag, v.remote = "", ""
	}
	v.mu.Unlock()
	_ = v.writeInfo() // best effort: a stale file only means stale details until discovery runs
	return reclone
}

func (v *Vault) writeInfo() error {
	raw, err := json.Marshal(v.Info())
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(v.dir, infoFile), raw, 0o644)
}

func (v *Vault) setState(s State, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.status.State = s
	v.recordLocked(err)
}

// recordFailure notes a failed sync of a vault that keeps serving its last good index, and backs off.
func (v *Vault) recordFailure(err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.recordLocked(err)
}

func (v *Vault) recordLocked(err error) {
	v.status.Error = ""
	if err == nil {
		v.failures = 0
		v.retryAt = time.Time{}
		return
	}
	v.status.Error = err.Error()
	v.failures++
	backoff := min(time.Minute<<min(v.failures, 5), 30*time.Minute)
	v.retryAt = time.Now().Add(backoff)
}

// due reports whether a failed vault or a failing sync may be tried again.
func (v *Vault) due() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return time.Now().After(v.retryAt)
}

// errRemoved is returned by work on a vault that was removed meanwhile.
var errRemoved = errors.New("vault was removed")

// open creates the vault's directory, writes its details, opens its index and opens or clones its git
// copy. Called with syncMu held.
func (v *Vault) open(ctx context.Context, l *Library) error {
	if v.removed {
		return errRemoved
	}
	info := v.Info()
	err := os.MkdirAll(v.dir, 0o755)
	if err != nil {
		return err
	}
	err = v.writeInfo()
	if err != nil {
		return fmt.Errorf("write vault details: %w", err)
	}
	if v.index == nil {
		v.index, err = index.Open(ctx, filepath.Join(v.dir, indexDir), l.logger.With("vault", info.Name))
		if err != nil {
			return err
		}
	}
	if v.mirror != nil {
		return nil
	}
	m, err := l.openMirror(ctx, v, filepath.Join(v.dir, gitDir))
	if err != nil {
		return err
	}
	v.gitMu.Lock()
	v.mirror = m
	v.gitMu.Unlock()
	return nil
}

// openMirror opens or clones the vault's git copy in dir. A clone that makes no progress for the stall
// timeout is cancelled; a slow one that keeps going is left to finish.
func (l *Library) openMirror(ctx context.Context, v *Vault, dir string) (*gitsync.Mirror, error) {
	info := v.Info()
	ctx, touch, stop := stallContext(ctx, l.opts.StallTimeout)
	defer stop()
	var auth gitsync.TokenSource
	if l.opts.Tokens != nil {
		auth = l.opts.Tokens(info.InstallationID)
	}
	return gitsync.Open(ctx, dir, gitsync.Source{
		URL:      l.opts.GitURL + "/" + info.Name + ".git",
		Branch:   info.DefaultBranch,
		Auth:     auth,
		Progress: progressWriter(touch),
	})
}

// close closes the index. Called with syncMu held.
func (v *Vault) close() {
	if v.index != nil {
		v.index.Close() // further use returns index.ErrClosed
	}
}

// Sync brings a vault's copy and index up to date with GitHub.
func (l *Library) Sync(ctx context.Context, v *Vault) error {
	v.syncMu.Lock()
	defer v.syncMu.Unlock()
	return l.syncLocked(ctx, v)
}

func (l *Library) syncLocked(ctx context.Context, v *Vault) error {
	if v.removed {
		return errRemoved
	}
	if v.mirror == nil || v.index == nil {
		return errors.New("vault isn't open")
	}

	fetchCtx, touch, stop := stallContext(ctx, l.opts.StallTimeout)
	v.gitMu.Lock()
	head, err := v.mirror.Fetch(fetchCtx, progressWriter(touch))
	v.gitMu.Unlock()
	stop()
	if err != nil {
		return err
	}

	commit := v.index.Commit()
	switch {
	case commit == head:
	case commit != "" && v.mirror.Has(commit):
		err = l.apply(ctx, v, commit, head)
	default:
		err = l.build(ctx, v, head)
	}
	if err != nil {
		return err
	}

	if v.mirror.Packs() > maxPacks && time.Since(v.freshCloneAt) > time.Hour {
		v.freshCloneAt = time.Now()
		err = l.freshClone(ctx, v)
		if err != nil {
			l.logger.Warn("fresh clone failed", "vault", v.Info().Name, "err", err)
		}
	}

	stats, err := v.index.Stats(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	v.mu.Lock()
	v.status.Commit = head
	v.status.Documents = stats.Documents
	v.status.Bytes = stats.Bytes
	v.status.Large = stats.Documents > largeDocuments || stats.Bytes > largeBytes
	v.status.LastSync = now
	v.status.Fresh = now
	v.status.State = StateServing
	v.recordLocked(nil)
	large := v.status.Large
	v.mu.Unlock()
	if large {
		l.logger.Warn("vault is larger than speki is built for", "vault", v.Info().Name,
			"documents", stats.Documents, "bytes", stats.Bytes)
	}
	return nil
}

func (l *Library) apply(ctx context.Context, v *Vault, from, to string) error {
	v.gitMu.RLock()
	defer v.gitMu.RUnlock()
	diff, err := v.mirror.Diff(from, to)
	if err != nil {
		return err
	}
	changes := make([]index.Change, 0, len(diff))
	for _, d := range diff {
		c := index.Change{Path: d.Path, Version: d.Blob}
		if d.Blob != "" {
			c.Content, err = v.mirror.Blob(d.Blob)
			if err != nil {
				return err
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
		return err
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

// freshClone replaces a copy that collected too many pack files. It clones next to the old copy and swaps
// the directories, so a failure at any step leaves a working copy in place. Called with syncMu held.
func (l *Library) freshClone(ctx context.Context, v *Vault) error {
	current := filepath.Join(v.dir, gitDir)
	next, old := current+".next", current+".old"
	_ = os.RemoveAll(next)
	_ = os.RemoveAll(old)
	_, err := l.openMirror(ctx, v, next)
	if err != nil {
		_ = os.RemoveAll(next)
		return err
	}

	v.gitMu.Lock()
	defer v.gitMu.Unlock()
	err = os.Rename(current, old)
	if err != nil {
		_ = os.RemoveAll(next)
		return fmt.Errorf("swap fresh copy: %w", err)
	}
	err = os.Rename(next, current)
	if err != nil {
		_ = os.Rename(old, current) // put the old copy back
		_ = os.RemoveAll(next)
		return fmt.Errorf("swap fresh copy: %w", err)
	}
	m, err := l.openMirror(ctx, v, current) // a healthy copy opens without network access
	if err != nil {
		return err // v.mirror still reads from the old copy's open files; the next sync reopens
	}
	v.mirror = m
	_ = os.RemoveAll(old)
	return nil
}
