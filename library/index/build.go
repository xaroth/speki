// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"runtime"
	"time"

	"golang.org/x/sync/errgroup"
)

// Build creates a new generation from every file at commit and makes it current. Files are parsed in
// parallel and written by one connection. The old generation keeps serving until the new one is ready.
// Everything, the schema included, is written in one transaction: a build that's interrupted leaves a
// file without a schema, which Open discards.
func (x *Index) Build(ctx context.Context, commit string, files iter.Seq2[Change, error]) error {
	start := time.Now()
	old, err := x.acquire()
	if err != nil {
		return err
	}
	n := old.n + 1
	old.release()

	removeGeneration(x.dir, n) // leftovers of an interrupted build
	g, err := connect(ctx, x.dir, n)
	if err != nil {
		return fmt.Errorf("build: new generation %d: %w", n, err)
	}
	count, err := fill(ctx, g.writer, commit, files, x.logger)
	if err != nil {
		g.close(true)
		return err
	}
	g.commit.Store(commit)
	err = x.swap(g)
	if err != nil {
		return err
	}
	x.reloadVocabulary()
	x.logger.InfoContext(ctx, "index built", "commit", commit, "documents", count, "took", time.Since(start).Round(time.Millisecond))
	return nil
}

// fill writes a whole index into db. Without a journal and syncs it's much faster; a failure leaves the
// file unusable, and it's thrown away.
func fill(
	ctx context.Context,
	db *sql.DB,
	commit string,
	files iter.Seq2[Change, error],
	logger *slog.Logger,
) (int, error) {
	_, err := db.ExecContext(ctx, "PRAGMA journal_mode = OFF; PRAGMA synchronous = OFF")
	if err != nil {
		return 0, fmt.Errorf("build: turn the journal off: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("build: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	err = initSchema(ctx, tx)
	if err != nil {
		return 0, fmt.Errorf("build: create the schema: %w", err)
	}

	readCtx, stop := context.WithCancel(ctx) // stops reading and parsing once an insert fails
	defer stop()
	g, gctx := errgroup.WithContext(readCtx)
	in := make(chan Change)
	out := make(chan row)
	g.Go(func() error {
		defer close(in)
		for c, err := range files {
			if err != nil {
				return err
			}
			select {
			case in <- c:
			case <-gctx.Done():
				return gctx.Err()
			}
		}
		return nil
	})
	parsers, pctx := errgroup.WithContext(gctx)
	for range runtime.GOMAXPROCS(0) {
		parsers.Go(func() error {
			for c := range in {
				r, err := rowOf(c)
				if err != nil {
					logger.WarnContext(ctx, "indexing an unusable file as broken", "path", c.Path, "err", err)
				}
				select {
				case out <- r:
				case <-pctx.Done():
					return pctx.Err()
				}
			}
			return nil
		})
	}
	g.Go(func() error {
		defer close(out)
		return parsers.Wait()
	})

	count := 0
	var insertErr error
	for r := range out {
		if insertErr != nil {
			continue // drain, so the parsers can finish
		}
		insertErr = insert(ctx, tx, r)
		if insertErr != nil {
			insertErr = fmt.Errorf("build: %s: %w", r.path, insertErr)
			stop()
			continue
		}
		count++
	}
	err = fillError(ctx, insertErr, g.Wait())
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, rebuildFullText)
	if err != nil {
		return 0, fmt.Errorf("build full-text index: %w", err)
	}
	_, err = tx.ExecContext(ctx, triggers)
	if err != nil {
		return 0, fmt.Errorf("create triggers: %w", err)
	}
	// Last: with the journal off, pages may reach the disk before the commit, and a file without a
	// recorded commit is discarded by Open.
	err = recordCommit(ctx, tx, commit)
	if err != nil {
		return 0, fmt.Errorf("build: record the commit: %w", err)
	}
	err = tx.Commit()
	if err != nil {
		return 0, fmt.Errorf("build: %w", err)
	}
	_, err = db.ExecContext(ctx, "PRAGMA journal_mode = WAL; PRAGMA synchronous = NORMAL")
	if err != nil {
		return 0, fmt.Errorf("switch to WAL: %w", err)
	}
	return count, nil
}

// fillError combines a build's insert error with the error of its reading and parsing. A failed insert
// isn't the file's fault: the whole build fails, and is tried again. The cancellation it caused itself
// is left out; anything else is kept, so a broken git copy is still noticed.
func fillError(ctx context.Context, insertErr, readErr error) error {
	switch {
	case readErr == nil || insertErr != nil && ctx.Err() == nil && errors.Is(readErr, context.Canceled):
		return insertErr
	case insertErr == nil:
		return fmt.Errorf("build: %w", readErr)
	}
	return errors.Join(insertErr, fmt.Errorf("build: %w", readErr))
}
