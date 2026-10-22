//go:build sqlite_fts5

// Package index keeps one SQLite full-text index per vault and searches across several of them. It works
// on files: it knows nothing about vaults, users or git.
//
// Build with the sqlite_fts5 tag: mattn/go-sqlite3 only includes FTS5 with it.
package index

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"sync/atomic"
)

// Index is one vault's search index. A full build writes a new generation (a new file); incremental
// changes go into the current one. Readers hold a reference to the generation they started on, so a
// build never pulls a file out from under them.
type Index struct {
	dir     string
	logger  *slog.Logger
	current atomic.Pointer[generation]

	vocabMu      sync.Mutex // guards vocabPending
	vocabPending bool       // a reload is queued but hasn't started
	vocabLoad    sync.Mutex // one reload runs at a time

	// Close cancels ctx to stop background work, and waits for background.
	ctx        context.Context
	cancel     context.CancelFunc
	background sync.WaitGroup
	lifeMu     sync.Mutex // orders closing against starting background work and swapping generations
	closed     atomic.Bool
	closeOnce  sync.Once
}

// ErrClosed is returned by an index that has been closed.
var ErrClosed = errors.New("index is closed")

// Open opens the newest usable generation in dir, creating dir when needed, and deletes every other
// generation file, including leftovers of interrupted builds. When there's no usable generation the index
// is empty and Commit returns "".
func Open(ctx context.Context, dir string, logger *slog.Logger) (*Index, error) {
	err := os.MkdirAll(dir, 0o755)
	if err != nil {
		return nil, fmt.Errorf("index dir: %w", err)
	}
	x := &Index{dir: dir, logger: logger}
	x.ctx, x.cancel = context.WithCancel(context.WithoutCancel(ctx))

	gens, err := generationsIn(dir)
	if err != nil {
		return nil, err
	}
	var keep *generation
	for _, n := range slices.Backward(gens) {
		g, err := openGeneration(ctx, dir, n)
		if errors.Is(err, errUnusable) {
			logger.WarnContext(ctx, "discarding unusable index", "file", generationPath(dir, n), "err", err)
			continue
		}
		if err != nil { // maybe temporary (too many open files, a cancelled context): keep the files
			return nil, fmt.Errorf("open index: %w", err)
		}
		keep = g
		break
	}
	if keep == nil {
		next := 1
		if len(gens) > 0 {
			next = gens[len(gens)-1] + 1
		}
		keep, err = createGeneration(ctx, dir, next)
		if err != nil {
			return nil, err
		}
	}
	for _, n := range gens {
		if n != keep.n {
			removeGeneration(dir, n)
		}
	}
	x.current.Store(keep)
	x.reloadVocabulary()
	return x, nil
}

// Commit returns the commit the index reflects, "" when it's empty or closed.
func (x *Index) Commit() string {
	g, err := x.acquire()
	if err != nil {
		return ""
	}
	defer g.release()
	return g.commit.Load().(string)
}

// Close stops background work, then closes the index, keeping its file for the next Open. It must
// not be used afterwards. Callers after the first wait until it's done.
func (x *Index) Close() {
	x.closeOnce.Do(func() {
		x.lifeMu.Lock()
		x.closed.Store(true)
		x.lifeMu.Unlock()
		x.cancel()
		x.background.Wait()
		x.current.Load().retire(false)
	})
}

// acquire returns the current generation with a reference held. Call release when done.
func (x *Index) acquire() (*generation, error) {
	for {
		if x.closed.Load() {
			return nil, ErrClosed
		}
		g := x.current.Load()
		if g.hold() {
			return g, nil
		}
		// g was swapped out between the load and the hold: try the new one.
	}
}

// swap makes g current and retires the previous generation, which is closed and deleted once its last
// reader is done. After Close it closes g instead, keeping its file for the next Open.
func (x *Index) swap(g *generation) error {
	x.lifeMu.Lock()
	defer x.lifeMu.Unlock()
	if x.closed.Load() {
		g.retire(false)
		return ErrClosed
	}
	old := x.current.Swap(g)
	old.retire(true)
	return nil
}

// goBackground runs work in the background, tracked for Close; after Close it doesn't run it.
func (x *Index) goBackground(work func()) {
	x.lifeMu.Lock()
	defer x.lifeMu.Unlock()
	if !x.closed.Load() {
		x.background.Go(work)
	}
}

// acquireAll acquires the current generation of every source. Call release when done, also on error.
func acquireAll(sources []Source) (gens []*generation, release func(), err error) {
	gens = make([]*generation, 0, len(sources))
	release = func() {
		for _, g := range gens {
			g.release()
		}
	}
	for _, s := range sources {
		g, err := s.Index.acquire()
		if err != nil {
			return gens, release, fmt.Errorf("%s: %w", s.Name, err)
		}
		gens = append(gens, g)
	}
	return gens, release, nil
}
