package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	pathpkg "path"
	"strings"
)

// ErrNotFound means the index has no document at that path.
var ErrNotFound = errors.New("no such document")

// Document is what the index knows about one document.
type Document struct {
	Path        string
	Title       string
	Description string
	Status      string
	Tags        []string
	Aliases     []string
	Related     []string
	Created     string // RFC 3339, empty when missing
	Updated     string
	Extra       map[string]any
	Bytes       int
	Lines       int
	Version     string
	Warnings    []string
}

// Entry is one item in a folder listing: a folder (Path ends in "/") or a document.
type Entry struct {
	Path        string
	Title       string
	Description string
	Status      string
	Updated     string
	Version     string
}

// IsFolder reports whether the entry is a folder.
func (e Entry) IsFolder() bool { return strings.HasSuffix(e.Path, "/") }

// Document returns one document's metadata.
func (x *Index) Document(ctx context.Context, path string) (Document, error) {
	g, err := x.acquire()
	if err != nil {
		return Document{}, err
	}
	defer g.release()

	var d Document
	var desc, created, updated sql.NullString
	var tags, aliases, related, extra, warnings string
	err = g.reader.QueryRowContext(ctx, `SELECT path, title, description, status, tags, aliases, related,
			created, updated, extra, bytes, lines, version, warnings
		FROM documents WHERE path = ?`, path).Scan(
		&d.Path, &d.Title, &desc, &d.Status, &tags, &aliases, &related,
		&created, &updated, &extra, &d.Bytes, &d.Lines, &d.Version, &warnings,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, ErrNotFound
	}
	if err != nil {
		return Document{}, fmt.Errorf("document %s: %w", path, err)
	}
	d.Description, d.Created, d.Updated = desc.String, created.String, updated.String
	for _, f := range []struct {
		src string
		dst any
	}{{tags, &d.Tags}, {aliases, &d.Aliases}, {related, &d.Related}, {extra, &d.Extra}, {warnings, &d.Warnings}} {
		err = json.Unmarshal([]byte(f.src), f.dst)
		if err != nil {
			return Document{}, fmt.Errorf("document %s: %w", path, err)
		}
	}
	return d, nil
}

// List returns the folders and documents under folder ("" for the root), sorted by path, starting after
// the entry with path after. Without recursive, documents in subfolders show up as one folder entry each.
// Paging by path keeps a listing consistent while documents come and go.
func (x *Index) List(ctx context.Context, folder string, recursive bool, after string, limit int) ([]Entry, error) {
	g, err := x.acquire()
	if err != nil {
		return nil, err
	}
	defer g.release()

	prefix := strings.Trim(folder, "/")
	if prefix != "" {
		prefix += "/"
	}
	start, exclusive := prefix, false
	if after > prefix {
		start, exclusive = after, true
		if strings.HasSuffix(after, "/") {
			start += endOfRange // after a folder: skip everything in it
		}
	}
	var out []Entry
	for len(out) < limit {
		e, ok, err := nextDocument(ctx, g.reader, prefix, start, exclusive)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", folder, err)
		}
		if !ok {
			break
		}
		start, exclusive = e.Path, true
		rest := strings.TrimPrefix(e.Path, prefix)
		if sub, _, inFolder := strings.Cut(rest, "/"); inFolder && !recursive {
			e = Entry{Path: prefix + sub + "/"}
			start = e.Path + endOfRange // skip the rest of the folder in one step
		}
		out = append(out, e)
	}
	return out, nil
}

// endOfRange sorts after any path that starts with what it's appended to.
const endOfRange = "\U0010FFFF"

// nextDocument returns the first document under prefix with a path after start (or at start, unless
// exclusive). Range conditions on path let SQLite use its index.
func nextDocument(ctx context.Context, db *sql.DB, prefix, start string, exclusive bool) (Entry, bool, error) {
	op := ">="
	if exclusive {
		op = ">"
	}
	var e Entry
	var desc, updated sql.NullString
	err := db.QueryRowContext(ctx, `SELECT path, title, description, status, updated, version
		FROM documents
		WHERE path `+op+` ? AND path < ?
		ORDER BY path LIMIT 1`, start, prefix+endOfRange).Scan(
		&e.Path, &e.Title, &desc, &e.Status, &updated, &e.Version,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, err
	}
	e.Description, e.Updated = desc.String, updated.String
	return e, true, nil
}

// LinkedFrom returns the documents that link to path, sorted. Call it before deleting or moving path:
// wikilinks are resolved against the documents that exist.
func (x *Index) LinkedFrom(ctx context.Context, path string) ([]string, error) {
	g, err := x.acquire()
	if err != nil {
		return nil, err
	}
	defer g.release()

	name := strings.ToLower(strings.TrimSuffix(pathpkg.Base(path), ".md"))
	rows, err := g.reader.QueryContext(ctx, `SELECT DISTINCT d.path
		FROM links l JOIN documents d ON d.id = l.doc_id
		WHERE d.path != ? AND (
			(l.kind != 'wikilink' AND l.candidate = ?)
			OR (l.kind = 'wikilink' AND l.candidate = ? AND ? = (
				SELECT w.path FROM documents w WHERE w.name = ? ORDER BY length(w.path), w.path LIMIT 1))
		)
		ORDER BY d.path`, path, path, name, path, name)
	if err != nil {
		return nil, fmt.Errorf("links to %s: %w", path, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		err = rows.Scan(&p)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Stats is the size of an index.
type Stats struct {
	Documents int
	Bytes     int64 // markdown indexed
}

// Stats counts the documents in the index.
func (x *Index) Stats(ctx context.Context) (Stats, error) {
	g, err := x.acquire()
	if err != nil {
		return Stats{}, err
	}
	defer g.release()
	var s Stats
	err = g.reader.QueryRowContext(ctx, "SELECT count(*), coalesce(sum(bytes), 0) FROM documents").Scan(&s.Documents, &s.Bytes)
	if err != nil {
		return Stats{}, fmt.Errorf("stats: %w", err)
	}
	return s, nil
}
