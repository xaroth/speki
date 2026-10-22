package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xaroth/speki/library/gitsync"
	"github.com/xaroth/speki/library/index"
	"github.com/xaroth/speki/vault"
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
	now func() time.Time

	mu       sync.Mutex // guards the fields up to syncMu
	info     vault.Vault
	status   Status
	etag     string // of the last branch lookup
	remote   string // the commit GitHub last reported
	failures int
	retryAt  time.Time // when a failed vault, or a failing sync, is tried again

	syncMu       syncLock    // one open, sync, reclone or removal at a time; guards the fields below
	scheduled    atomic.Bool // a rebuild, re-initialisation or reclone is queued in the background
	broken       atomic.Bool // a read found the git copy missing objects: the poller clones it again
	removed      bool
	freshCloneAt time.Time    // last attempt, so a failing fresh clone isn't retried on every sync
	gitMu        sync.RWMutex // fetches exclude reads of the git copy
	mirror       *gitsync.Mirror
	index        *index.Index // set once by open, then never changed
}

func newVault(info vault.Vault, dir string, now func() time.Time) *Vault {
	return &Vault{
		dir:    dir,
		now:    now,
		info:   info,
		status: Status{RepoID: info.RepoID, Name: info.Name, State: StateBuilding},
		syncMu: make(syncLock, 1),
	}
}

// syncLock is a mutex a caller can stop waiting for. Make it with make(syncLock, 1): newVault does.
type syncLock chan struct{}

func (l syncLock) Lock() { l <- struct{}{} }

func (l syncLock) Unlock() {
	select {
	case <-l:
	default:
		panic("library: unlock of an unlocked sync lock")
	}
}

// TryLock takes the lock if it's free, and reports whether it did.
func (l syncLock) TryLock() bool {
	select {
	case l <- struct{}{}:
		return true
	default:
		return false
	}
}

// LockContext takes the lock, or gives up when ctx ends.
func (l syncLock) LockContext(ctx context.Context) error {
	select {
	case l <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
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
	content, err := v.mirror.Blob(sha)
	if errors.Is(err, gitsync.ErrBroken) {
		// Only a copy at the index's commit must have every blob the index names. Just after a fresh
		// clone, before the rebuild, older blobs are missing for good reason.
		if head, herr := v.mirror.Head(); herr == nil && v.index != nil && head == v.index.Commit() {
			v.broken.Store(true)
		}
	}
	return content, err
}

// Paths returns every file path in the git copy at commit.
func (v *Vault) Paths(commit string) ([]string, error) {
	v.gitMu.RLock()
	defer v.gitMu.RUnlock()
	if v.mirror == nil {
		return nil, errors.New("no git copy")
	}
	return v.mirror.Paths(commit)
}

// Version returns the blob SHA of the file at path in the git copy at commit, "" when there's none.
func (v *Vault) Version(commit, path string) (string, error) {
	v.gitMu.RLock()
	defer v.gitMu.RUnlock()
	if v.mirror == nil {
		return "", errors.New("no git copy")
	}
	return v.mirror.BlobAt(commit, path)
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
	if s == StateBuilding && err == nil {
		return // starting over isn't a success: the failure count and backoff stay until one
	}
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
	if index.OutOfMemory(err) {
		// Usually other work, such as a heavy query, used the memory: no growing backoff, just a short
		// delay in case the vault needs that much itself.
		v.retryAt = v.now().Add(time.Minute)
		return
	}
	v.failures++
	backoff := min(time.Minute<<min(v.failures, 5), 30*time.Minute)
	v.retryAt = v.now().Add(backoff)
}

// due reports whether a failed vault or a failing sync may be tried again.
func (v *Vault) due() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.now().After(v.retryAt)
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
		return fmt.Errorf("create vault folder: %w", err)
	}
	err = v.writeInfo()
	if err != nil {
		return fmt.Errorf("write vault details: %w", err)
	}
	if v.index == nil {
		v.index, err = index.Open(ctx, filepath.Join(v.dir, indexDir), l.logger.With("vault", info.Name))
		if err != nil {
			return fmt.Errorf("open index: %w", err)
		}
	}
	if v.mirror != nil {
		return nil
	}
	m, err := l.openMirror(ctx, v, filepath.Join(v.dir, gitDir))
	if err != nil {
		return fmt.Errorf("open git copy: %w", err)
	}
	v.gitMu.Lock()
	v.mirror = m
	v.gitMu.Unlock()
	return nil
}

// close closes the index. Called with syncMu held.
func (v *Vault) close() {
	if v.index != nil {
		v.index.Close() // further use returns index.ErrClosed
	}
}
