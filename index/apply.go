package index

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"path"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/eve-online-tools/speki/document"
)

// Change is one file to put in the index, or take out of it.
type Change struct {
	Path    string
	Version string // git blob SHA; empty means the file was deleted
	Content []byte
}

// Apply brings the current generation from its commit to commit. It runs in one transaction, so readers
// see the index before or after, never halfway. A file that can't be indexed is skipped and logged, so
// one bad file can't stop a vault from updating.
func (x *Index) Apply(ctx context.Context, commit string, changes []Change) error {
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
		err = applyOne(ctx, tx, c)
		if err != nil {
			x.logger.Warn("skipping file", "path", c.Path, "err", err)
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

// applyOne replaces one document inside a savepoint, so a failure leaves nothing half-written: the old
// document stays when the new one can't be indexed.
func applyOne(ctx context.Context, tx *sql.Tx, c Change) error {
	var r row
	if c.Version != "" {
		var err error
		r, err = parse(c)
		if err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, "SAVEPOINT change")
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "DELETE FROM documents WHERE path = ?", c.Path)
	if err == nil && c.Version != "" {
		err = insert(ctx, tx, r)
	}
	if err != nil {
		_, _ = tx.ExecContext(ctx, "ROLLBACK TO change")
		_, _ = tx.ExecContext(ctx, "RELEASE change")
		return err
	}
	_, err = tx.ExecContext(ctx, "RELEASE change")
	return err
}

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
		return err
	}
	count, err := fill(ctx, g.writer, commit, files, x.logger)
	if err != nil {
		g.close(true)
		return err
	}
	g.commit.Store(commit)
	x.swap(g)
	x.reloadVocabulary()
	x.logger.Info("index built", "commit", commit, "documents", count, "took", time.Since(start).Round(time.Millisecond))
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
		return 0, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	err = initSchema(ctx, tx)
	if err != nil {
		return 0, err
	}

	g, gctx := errgroup.WithContext(ctx)
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
				r, err := parse(c)
				if err != nil {
					logger.Warn("skipping file", "path", c.Path, "err", err)
					continue
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
	for r := range out {
		err := insert(ctx, tx, r)
		if err != nil {
			logger.Warn("skipping file", "path", r.path, "err", err)
			continue
		}
		count++
	}
	err = g.Wait()
	if err != nil {
		return 0, fmt.Errorf("build: %w", err)
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
		return 0, err
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

// row is a parsed document, ready to insert.
type row struct {
	path, name, title, description, status  string
	tags, aliases, related, extra, warnings string // JSON
	created, updated                        sql.NullString
	headingText, body, version              string
	bytes, lines                            int
	tagList                                 []string
	links                                   []document.Link
	headings                                []document.Heading
}

func parse(c Change) (row, error) {
	d, err := document.Parse(c.Path, c.Content)
	if err != nil {
		return row{}, err
	}
	m := d.Meta
	extra := m.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	var headingTexts []string
	for _, h := range d.Headings {
		headingTexts = append(headingTexts, h.Text)
	}
	r := row{
		path:        c.Path,
		name:        strings.ToLower(strings.TrimSuffix(path.Base(c.Path), ".md")),
		title:       m.Title,
		description: m.Description,
		status:      string(m.Status),
		created:     timeValue(m.Created),
		updated:     timeValue(m.Updated),
		headingText: strings.Join(headingTexts, "\n"),
		body:        d.Body,
		version:     c.Version,
		bytes:       len(c.Content),
		lines:       bytes.Count(c.Content, []byte("\n")) + 1,
		tagList:     m.Tags,
		links:       d.Links,
		headings:    d.Headings,
	}
	for _, f := range []struct {
		dst *string
		v   any
	}{
		{&r.tags, nonNil(m.Tags)},
		{&r.aliases, nonNil(m.Aliases)},
		{&r.related, nonNil(m.Related)},
		{&r.extra, extra},
		{&r.warnings, nonNil(d.Warnings)},
	} {
		b, err := json.Marshal(f.v)
		if err != nil {
			return row{}, fmt.Errorf("encode frontmatter: %w", err)
		}
		*f.dst = string(b)
	}
	return r, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func timeValue(t time.Time) sql.NullString {
	if t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: t.UTC().Format(time.RFC3339), Valid: true}
}

func insert(ctx context.Context, tx *sql.Tx, r row) error {
	res, err := tx.ExecContext(ctx, `INSERT INTO documents
		(path, name, title, description, status, tags, aliases, related, created, updated, extra,
		 headings, body, bytes, lines, version, warnings)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.path, r.name, r.title, nullIfEmpty(r.description), r.status, r.tags, r.aliases, r.related,
		r.created, r.updated, r.extra, r.headingText, r.body, r.bytes, r.lines, r.version, r.warnings,
	)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	for _, t := range r.tagList {
		_, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO tags (doc_id, tag) VALUES (?, ?)", id, t)
		if err != nil {
			return err
		}
	}
	for _, l := range r.links {
		_, err = tx.ExecContext(ctx,
			"INSERT INTO links (doc_id, target, candidate, kind, text) VALUES (?, ?, ?, ?, ?)",
			id, l.Target, nullIfEmpty(l.Candidate), string(l.Kind), nullIfEmpty(l.Text),
		)
		if err != nil {
			return err
		}
	}
	for _, h := range r.headings {
		_, err = tx.ExecContext(ctx,
			"INSERT INTO headings (doc_id, level, text, line) VALUES (?, ?, ?, ?)",
			id, h.Level, h.Text, h.Line,
		)
		if err != nil {
			return err
		}
	}
	return nil
}

func nullIfEmpty(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
