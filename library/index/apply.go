// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/xaroth/speki/telemetry"
)

// Change is one file to put in the index, or take out of it.
type Change struct {
	Path    string
	Version string // git blob SHA; empty means the file was deleted
	Content []byte
}

// Apply brings the current generation from its commit to commit. It runs in one transaction, so readers
// see the index before or after, never halfway. A file that's unusable is indexed as broken and logged, so
// one bad file can't stop a vault from updating. Any other failure (out of memory, a full disk, a cancelled
// context) aborts: the commit doesn't move, and the next sync tries again.
func (x *Index) Apply(ctx context.Context, commit string, changes []Change) error {
	ctx, span := telemetry.Tracer().Start(ctx, "index.Apply", trace.WithAttributes(attribute.Int("speki.changes", len(changes))))
	defer span.End()
	g, err := x.acquire()
	if err != nil {
		return err
	}
	defer g.release()

	tx, err := g.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("apply: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, c := range changes {
		err = applyOne(ctx, tx, c, x.logger)
		if err != nil {
			return fmt.Errorf("apply %s: %w", c.Path, err)
		}
	}
	_, err = tx.ExecContext(ctx, "UPDATE meta SET value = ? WHERE key = 'commit'", commit)
	if err != nil {
		return fmt.Errorf("set commit: %w", err)
	}
	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("apply: %w", err)
	}
	g.commit.Store(commit)
	x.reloadVocabulary()
	return nil
}

// applyOne replaces one document inside a savepoint, so a failure leaves nothing half-written.
func applyOne(ctx context.Context, tx *sql.Tx, c Change, logger *slog.Logger) error {
	var r row
	if c.Version != "" {
		var perr error
		r, perr = rowOf(c)
		if perr != nil {
			logger.WarnContext(ctx, "indexing an unusable file as broken", "path", c.Path, "err", perr)
		}
	}
	_, err := tx.ExecContext(ctx, "SAVEPOINT change")
	if err != nil {
		return fmt.Errorf("savepoint: %w", err)
	}
	err = replaceDocument(ctx, tx, c.Path, c.Version != "", r)
	if err != nil {
		_, _ = tx.ExecContext(ctx, "ROLLBACK TO change")
		_, _ = tx.ExecContext(ctx, "RELEASE change")
		return err
	}
	_, err = tx.ExecContext(ctx, "RELEASE change")
	if err != nil {
		return fmt.Errorf("release savepoint: %w", err)
	}
	return nil
}

func replaceDocument(ctx context.Context, tx *sql.Tx, path string, add bool, r row) error {
	_, err := tx.ExecContext(ctx, "DELETE FROM documents WHERE path = ?", path)
	if err != nil {
		return fmt.Errorf("delete document: %w", err)
	}
	if !add {
		return nil
	}
	return insert(ctx, tx, r)
}
