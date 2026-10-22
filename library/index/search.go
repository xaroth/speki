// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"
	"golang.org/x/text/unicode/norm"

	"github.com/xaroth/speki/document"
	"github.com/xaroth/speki/telemetry"
)

// ErrEmptyQuery is returned for a query with nothing to search for.
var ErrEmptyQuery = errors.New("the query has no words to search for")

// ErrTooManyWords means a query has more distinct words than a search looks for.
var ErrTooManyWords = fmt.Errorf("the query has more than %d different words", maxTerms)

const maxTerms = 32

// SearchTimeout is how long a search may take. It's shorter than a request's deadline so a slow search
// can't hold a vault's few reader connections for long.
const SearchTimeout = 5 * time.Second

// Source is one index to search, with the name results carry.
type Source struct {
	Name  string
	Index *Index
}

// Options narrow a search.
type Options struct {
	Limit             int      // 1 to 50, default 10
	Tags              []string // documents must have all of them
	PathPrefix        string   // only documents under this folder
	ExcludeDeprecated bool
}

// Hit is one search result.
type Hit struct {
	Source      string
	Path        string
	Title       string
	Description string
	Tags        []string
	Status      document.Status
	Updated     string
	Version     string
	Snippet     string
	Score       float64 // 0 to 1, higher is better; 1 is the best match in its source
}

// Results is what a search found.
type Results struct {
	Hits []Hit
	// Corrected maps words found in none of the sources to the words searched for instead.
	Corrected map[string]string
	// Relaxed is set when no document matched every word and the hits match only some of them.
	Relaxed bool
}

// bm25 column weights, in the order of the fts table's columns.
const bm25Weights = "10, 10, 6, 4, 3, 1"

// nameHitScore ranks substring matches on names below full-text matches.
const nameHitScore = 0.01

// Search looks for text in every source and merges the results, best first.
func Search(ctx context.Context, sources []Source, text string, opt Options) (Results, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "index.Search", trace.WithAttributes(attribute.Int("speki.vaults", len(sources))))
	defer span.End()
	if opt.Limit <= 0 {
		opt.Limit = 10
	}
	opt.Limit = min(opt.Limit, 50)
	terms := parseQuery(text)
	if !slices.ContainsFunc(terms, func(t term) bool { return !t.exclude }) {
		return Results{}, ErrEmptyQuery
	}
	if len(terms) > maxTerms {
		return Results{}, ErrTooManyWords
	}

	gens, release, err := acquireAll(sources)
	defer release()
	if err != nil {
		return Results{}, fmt.Errorf("search %w", err)
	}

	corrected, err := correct(ctx, gens, terms)
	if err != nil {
		return Results{}, err
	}
	res := Results{Corrected: corrected}

	res.Hits, err = searchAll(ctx, sources, gens, terms, opt, false)
	if err != nil {
		return Results{}, err
	}
	if len(res.Hits) == 0 && positiveTerms(terms) > 1 {
		res.Hits, err = searchAll(ctx, sources, gens, terms, opt, true)
		if err != nil {
			return Results{}, err
		}
		res.Relaxed = len(res.Hits) > 0
	}
	return res, nil
}

func searchAll(ctx context.Context, sources []Source, gens []*generation, terms []term, opt Options, anyWord bool) ([]Hit, error) {
	expr := ftsExpression(terms, anyWord)
	names := namesExpression(terms, anyWord)
	excluded := exclusionExpression(terms)
	perSource := make([][]Hit, len(sources))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for i := range sources {
		g.Go(func() error {
			hits, err := searchOne(gctx, gens[i], expr, names, excluded, opt)
			if err != nil {
				return fmt.Errorf("search %s: %w", sources[i].Name, err)
			}
			for j := range hits {
				hits[j].Source = sources[i].Name
			}
			perSource[i] = hits
			return nil
		})
	}
	err := g.Wait()
	if err != nil {
		return nil, err
	}
	all := slices.Concat(perSource...)
	slices.SortStableFunc(all, func(a, b Hit) int {
		return cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(a.Source, b.Source), cmp.Compare(a.Path, b.Path))
	})
	return all[:min(len(all), opt.Limit)], nil
}

func searchOne(ctx context.Context, g *generation, expr, namesExpr, excluded string, opt Options) ([]Hit, error) {
	// Rank inside the full-text table and join only the best few documents: reading a document row reads
	// its body, which costs far more than ranking. Deprecated documents are halved inside the ranking,
	// through a partial index, so they can't crowd active ones out of the top. Broken documents and the
	// filters are left out there too, through indexes, so the ranking stays limited when filtering.
	filter, args := filters(opt, "rowid")
	q := `WITH ranked AS (
			SELECT rowid AS id, bm25(fts, ` + bm25Weights + `) *
				CASE WHEN rowid IN (SELECT id FROM documents WHERE status = 'deprecated') THEN 0.5 ELSE 1 END AS rank
			FROM fts WHERE fts MATCH ? AND rowid NOT IN (SELECT id FROM documents WHERE status = 'broken')` + filter + `
			ORDER BY rank LIMIT ?)
		SELECT d.id, d.path, d.title, d.description, d.tags, d.status, d.updated, d.version, ranked.rank AS score
		FROM ranked JOIN documents d ON d.id = ranked.id
		ORDER BY score`
	found, err := queryHits(ctx, g.reader, q, slices.Concat([]any{expr}, args, []any{opt.Limit}))
	if err != nil {
		return nil, err
	}
	err = addSnippets(ctx, g.reader, expr, found)
	if err != nil {
		return nil, err
	}
	hits := make([]Hit, len(found))
	for i, f := range found {
		hits[i] = f.Hit
		hits[i].Score = -f.Score // bm25 is negative, lower is better
	}
	normalise(hits)
	if namesExpr == "" {
		return hits, nil
	}

	filter, args = filters(opt, "d.id")
	nameArgs := []any{namesExpr}
	if excluded != "" { // an excluded word anywhere in the document rules it out, not just in its name
		filter += " AND d.id NOT IN (SELECT rowid FROM fts WHERE fts MATCH ?)"
		args = append(args, excluded)
	}
	q = `SELECT d.id, d.path, d.title, d.description, d.tags, d.status, d.updated, d.version, 0
		FROM names JOIN documents d ON d.id = names.rowid
		WHERE names MATCH ? AND d.status != 'broken'` + filter + `
		LIMIT ?`
	byName, err := queryHits(ctx, g.reader, q, slices.Concat(nameArgs, args, []any{opt.Limit}))
	if err != nil {
		return nil, err
	}
	// Substring matches rank below every full-text match in this vault.
	nameScore := nameHitScore
	for _, h := range hits {
		nameScore = min(nameScore, h.Score/2)
	}
	for _, f := range byName {
		h := f.Hit
		if slices.ContainsFunc(hits, func(x Hit) bool { return x.Path == h.Path }) {
			continue
		}
		h.Score = nameScore
		if h.Status == document.StatusDeprecated {
			h.Score /= 2
		}
		h.Snippet = h.Description
		hits = append(hits, h)
	}
	return hits, nil
}

// normalise scales full-text scores so the best match in a vault scores 1: bm25 depends on the vault's
// size and word counts, so raw scores from different vaults don't compare. A deprecated document is
// measured unhalved, so it still scores half of what it would have.
func normalise(hits []Hit) {
	best := 0.0
	for _, h := range hits {
		s := h.Score
		if h.Status == document.StatusDeprecated {
			s *= 2
		}
		best = max(best, s)
	}
	if best <= 0 {
		return
	}
	for i := range hits {
		hits[i].Score /= best
	}
}

// found is a hit with its document ID, while a search is in progress.
type found struct {
	Hit
	id int64
}

// boldMatches turns a snippet with matches between \x01 and \x02 into markdown with the matches in
// bold, at most maxDescription bytes. A cut inside a match closes it.
func boldMatches(s string) string {
	head := s[:min(len(s), maxDescription)]
	markers := strings.Count(head, "\x01") + strings.Count(head, "\x02") // each grows by a byte
	s = clip(s, maxDescription-markers-2)
	if strings.Count(s, "\x01") > strings.Count(s, "\x02") {
		if cut, ok := strings.CutSuffix(s, "…"); ok {
			s = cut + "\x02…"
		} else {
			s += "\x02"
		}
	}
	return strings.NewReplacer("\x01", "**", "\x02", "**").Replace(s)
}

func addSnippets(ctx context.Context, db *sql.DB, expr string, hits []found) error {
	if len(hits) == 0 {
		return nil
	}
	ids := make([]any, len(hits))
	for i, h := range hits {
		ids[i] = h.id
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(ids)), ", ")
	rows, err := db.QueryContext(ctx,
		// Private markers around matches, so markdown bold in the body isn't taken for one.
		`SELECT rowid, snippet(fts, 5, char(1), char(2), '…', 32) FROM fts WHERE fts MATCH ? AND rowid IN (`+placeholders+`)`,
		slices.Concat([]any{expr}, ids)...,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	snippets := make(map[int64]string, len(hits))
	for rows.Next() {
		var id int64
		var snippet string
		err = rows.Scan(&id, &snippet)
		if err != nil {
			return err
		}
		snippets[id] = strings.TrimSpace(snippet)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range hits {
		s := snippets[hits[i].id]
		if !strings.Contains(s, "\x01") && hits[i].Description != "" {
			hits[i].Snippet = hits[i].Description // the match wasn't in the body
			continue
		}
		hits[i].Snippet = boldMatches(s)
	}
	return nil
}

// filters returns conditions on the document ID in col, each answered by an index without reading
// document rows, and their arguments.
func filters(opt Options, col string) (string, []any) {
	// The unary plus keeps SQLite from handing an IN list to the full-text table, which then runs the
	// match once per listed ID: several times slower than checking each match against the list.
	col = "+" + col
	var b strings.Builder
	var args []any
	if opt.ExcludeDeprecated {
		b.WriteString(" AND " + col + " NOT IN (SELECT id FROM documents WHERE status = 'deprecated')")
	}
	if p := norm.NFC.String(strings.Trim(opt.PathPrefix, "/")); p != "" {
		// Keys under p/ sort from p/ to just before p0, '0' being the byte after '/'. In NFC, so a folder
		// matches however it's typed.
		b.WriteString(" AND " + col + " IN (SELECT id FROM documents WHERE key >= ? AND key < ?)")
		args = append(args, p+"/", p+"0")
	}
	for _, t := range opt.Tags {
		b.WriteString(" AND " + col + " IN (SELECT doc_id FROM tags WHERE tag = ?)")
		args = append(args, strings.ToLower(strings.TrimPrefix(t, "#")))
	}
	return b.String(), args
}

func queryHits(ctx context.Context, db *sql.DB, q string, args []any) ([]found, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []found
	for rows.Next() {
		var h found
		var desc, updated sql.NullString
		var tags string
		err = rows.Scan(&h.id, &h.Path, &h.Title, &desc, &tags, &h.Status, &updated, &h.Version, &h.Score)
		if err != nil {
			return nil, err
		}
		h.Title, h.Description, h.Updated = clip(h.Title, maxTitle), clip(desc.String, maxDescription), updated.String
		err = json.Unmarshal([]byte(tags), &h.Tags)
		if err != nil {
			return nil, fmt.Errorf("tags of %s: %w", h.Path, err)
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}
