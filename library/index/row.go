// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/xaroth/speki/document"
)

// row is a parsed document, ready to insert.
type row struct {
	path, key, name, title, description     string
	status                                  string
	tags, aliases, related, extra, warnings string // JSON
	created, updated                        sql.NullString
	headingText, body, version              string
	broken                                  string // why the file can't be parsed
	bytes, lines                            int
	tagList                                 []string
	links                                   []document.Link
	headings                                []document.Heading
}

// rowOf parses c into a row. A file that can't be parsed gets a broken row, with the parse error beside
// it for the log: the index must never keep an older version of a file that changed, or a write based on
// it would overwrite a change nobody saw.
func rowOf(c Change) (row, error) {
	r, err := parse(c)
	if err == nil {
		return r, nil
	}
	return brokenRow(c, err), err
}

// brokenRow is a file that can't be parsed: its new version and size, its path for a title, and no
// content, so search doesn't find what it said before.
func brokenRow(c Change, err error) row {
	return row{
		path:     c.Path,
		key:      norm.NFC.String(c.Path),
		name:     document.LinkName(c.Path),
		title:    c.Path,
		status:   string(StatusBroken),
		tags:     "[]",
		aliases:  "[]",
		related:  "[]",
		extra:    "{}",
		warnings: "[]",
		version:  c.Version,
		bytes:    len(c.Content),
		lines:    bytes.Count(c.Content, []byte("\n")) + 1,
		broken:   err.Error(),
	}
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
		key:         norm.NFC.String(c.Path),
		name:        document.LinkName(c.Path),
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
		(path, key, name, title, description, status, tags, aliases, related, created, updated, extra,
		 headings, body, bytes, lines, version, warnings, broken)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.path, r.key, r.name, r.title, nullIfEmpty(r.description), r.status, r.tags, r.aliases, r.related,
		r.created, r.updated, r.extra, r.headingText, r.body, r.bytes, r.lines, r.version, r.warnings,
		nullIfEmpty(r.broken),
	)
	if err != nil {
		return fmt.Errorf("insert document: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("document id: %w", err)
	}
	for _, t := range r.tagList {
		_, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO tags (doc_id, tag) VALUES (?, ?)", id, t)
		if err != nil {
			return fmt.Errorf("insert tag: %w", err)
		}
	}
	for _, l := range r.links {
		_, err = tx.ExecContext(ctx,
			"INSERT INTO links (doc_id, target, candidate, kind, text) VALUES (?, ?, ?, ?, ?)",
			id, l.Target, nullIfEmpty(l.Candidate), string(l.Kind), nullIfEmpty(l.Text),
		)
		if err != nil {
			return fmt.Errorf("insert link: %w", err)
		}
	}
	for _, h := range r.headings {
		_, err = tx.ExecContext(ctx,
			"INSERT INTO headings (doc_id, level, text, line) VALUES (?, ?, ?, ?)",
			id, h.Level, h.Text, h.Line,
		)
		if err != nil {
			return fmt.Errorf("insert heading: %w", err)
		}
	}
	return nil
}

func nullIfEmpty(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
