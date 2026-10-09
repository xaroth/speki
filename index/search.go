package index

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// ErrEmptyQuery is returned for a query with nothing to search for.
var ErrEmptyQuery = errors.New("the query has no words to search for")

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
	Status      string
	Updated     string
	Version     string
	Snippet     string
	Score       float64 // higher is better
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

// candidatesPerHit is how many ranked candidates an unfiltered search considers per hit it returns.
const candidatesPerHit = 3

// nameHitScore ranks substring matches on names below full-text matches.
const nameHitScore = 0.01

// term is one part of a query.
type term struct {
	text    string // lowercased, without diacritics; words of a phrase separated by spaces
	phrase  bool
	exclude bool
}

// Search looks for text in every source and merges the results, best first.
func Search(ctx context.Context, sources []Source, text string, opt Options) (Results, error) {
	if opt.Limit <= 0 {
		opt.Limit = 10
	}
	opt.Limit = min(opt.Limit, 50)
	terms := parseQuery(text)
	if !slices.ContainsFunc(terms, func(t term) bool { return !t.exclude }) {
		return Results{}, ErrEmptyQuery
	}

	gens := make([]*generation, 0, len(sources))
	defer func() {
		for _, g := range gens {
			g.release()
		}
	}()
	for _, s := range sources {
		g, err := s.Index.acquire()
		if err != nil {
			return Results{}, fmt.Errorf("search %s: %w", s.Name, err)
		}
		gens = append(gens, g)
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
	filter, args := filters(opt)
	// Rank inside the full-text table first and join the documents of the best few only: reading a
	// document row means reading its body, which costs far more than ranking. Deprecated documents are
	// halved after that, so keep enough candidates for them to drop out of the top. Filters can throw
	// away any number of candidates, so a filtered search joins everything that matches.
	ranked := `SELECT rowid AS id, bm25(fts, ` + bm25Weights + `) AS rank FROM fts WHERE fts MATCH ?`
	rankArgs := []any{expr}
	if filter == "" {
		ranked += ` ORDER BY rank LIMIT ?`
		rankArgs = append(rankArgs, opt.Limit*candidatesPerHit)
	}
	q := `WITH ranked AS (` + ranked + `)
		SELECT d.id, d.path, d.title, d.description, d.tags, d.status, d.updated, d.version,
			ranked.rank * CASE d.status WHEN 'deprecated' THEN 0.5 ELSE 1 END AS score
		FROM ranked JOIN documents d ON d.id = ranked.id
		WHERE 1` + filter + `
		ORDER BY score LIMIT ?`
	found, err := queryHits(ctx, g.reader, q, slices.Concat(rankArgs, args, []any{opt.Limit}))
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
	if namesExpr == "" {
		return hits, nil
	}

	nameArgs := []any{namesExpr}
	if excluded != "" { // an excluded word anywhere in the document rules it out, not just in its name
		filter += " AND d.id NOT IN (SELECT rowid FROM fts WHERE fts MATCH ?)"
		args = append(args, excluded)
	}
	q = `SELECT d.id, d.path, d.title, d.description, d.tags, d.status, d.updated, d.version, 0
		FROM names JOIN documents d ON d.id = names.rowid
		WHERE names MATCH ?` + filter + `
		LIMIT ?`
	byName, err := queryHits(ctx, g.reader, q, slices.Concat(nameArgs, args, []any{opt.Limit}))
	if err != nil {
		return nil, err
	}
	for _, f := range byName {
		h := f.Hit
		if slices.ContainsFunc(hits, func(x Hit) bool { return x.Path == h.Path }) {
			continue
		}
		h.Score = nameHitScore
		if h.Status == "deprecated" {
			h.Score /= 2
		}
		h.Snippet = h.Description
		hits = append(hits, h)
	}
	return hits, nil
}

// found is a hit with its document ID, while a search is in progress.
type found struct {
	Hit
	id int64
}

// addSnippets fills in the snippets of hits.
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
		`SELECT rowid, snippet(fts, 5, '**', '**', '…', 32) FROM fts WHERE fts MATCH ? AND rowid IN (`+placeholders+`)`,
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
		hits[i].Snippet = snippets[hits[i].id]
		if !strings.Contains(hits[i].Snippet, "**") && hits[i].Description != "" {
			hits[i].Snippet = hits[i].Description // the match wasn't in the body
		}
	}
	return nil
}

func filters(opt Options) (string, []any) {
	var b strings.Builder
	var args []any
	if opt.ExcludeDeprecated {
		b.WriteString(" AND d.status != 'deprecated'")
	}
	if p := strings.Trim(opt.PathPrefix, "/"); p != "" {
		b.WriteString(" AND substr(d.path, 1, length(?)) = ?")
		args = append(args, p+"/", p+"/")
	}
	for _, t := range opt.Tags {
		b.WriteString(" AND EXISTS (SELECT 1 FROM tags t WHERE t.doc_id = d.id AND t.tag = ?)")
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
		h.Description, h.Updated = desc.String, updated.String
		err = json.Unmarshal([]byte(tags), &h.Tags)
		if err != nil {
			return nil, fmt.Errorf("tags of %s: %w", h.Path, err)
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// parseQuery splits a query into words, "quoted phrases" and -excluded words. Words are split like the
// index splits them: at anything that isn't a letter or digit.
func parseQuery(text string) []term {
	var terms []term
	for text != "" {
		text = strings.TrimLeftFunc(text, unicode.IsSpace)
		if text == "" {
			break
		}
		exclude := false
		if rest, ok := strings.CutPrefix(text, "-"); ok {
			exclude, text = true, rest
		}
		if rest, ok := strings.CutPrefix(text, `"`); ok {
			phrase, after, _ := strings.Cut(rest, `"`)
			text = after
			if words := splitWords(phrase); len(words) > 0 {
				terms = append(terms, term{text: strings.Join(words, " "), phrase: len(words) > 1, exclude: exclude})
			}
			continue
		}
		end := strings.IndexFunc(text, unicode.IsSpace)
		if end < 0 {
			end = len(text)
		}
		chunk := text[:end]
		text = text[end:]
		for _, w := range splitWords(chunk) {
			terms = append(terms, term{text: w, exclude: exclude})
		}
	}
	return terms
}

func splitWords(s string) []string {
	return strings.FieldsFunc(fold(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// fold lowercases and removes diacritics, like the index's tokenizer.
func fold(s string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	out, _, err := transform.String(t, strings.ToLower(s))
	if err != nil {
		return strings.ToLower(s)
	}
	return out
}

func positiveTerms(terms []term) int {
	n := 0
	for _, t := range terms {
		if !t.exclude {
			n++
		}
	}
	return n
}

// lastWord returns the index of the last positive single word, which also matches as a prefix, or -1.
func lastWord(terms []term) int {
	for i, t := range slices.Backward(terms) {
		if !t.exclude {
			if t.phrase {
				return -1
			}
			return i
		}
	}
	return -1
}

// correct replaces words that appear in none of the sources with the closest known word, and returns
// what it replaced. Whether a word exists is asked of the index itself, so a word the agent wrote a
// moment ago counts even before the in-memory vocabulary catches up.
func correct(ctx context.Context, gens []*generation, terms []term) (map[string]string, error) {
	last := lastWord(terms)
	var corrected map[string]string
	for i := range terms {
		t := &terms[i]
		n := utf8.RuneCountInString(t.text)
		if t.phrase || t.exclude || n < 3 || strings.ContainsFunc(t.text, unicode.IsDigit) {
			continue // numbers aren't typos: 2026 shouldn't become 2025
		}
		known, err := knownAnywhere(ctx, gens, t.text, i == last)
		if err != nil {
			return nil, err
		}
		if known {
			continue
		}
		maxDistance := 1
		if n > 5 {
			maxDistance = 2
		}
		best, ok := bestSuggestion(gens, t.text, maxDistance)
		if !ok {
			continue
		}
		if corrected == nil {
			corrected = make(map[string]string)
		}
		corrected[t.text] = best
		t.text = best
	}
	return corrected, nil
}

// knownAnywhere asks each index whether it has word, or, for the word typed last, a word starting with it.
func knownAnywhere(ctx context.Context, gens []*generation, word string, asPrefix bool) (bool, error) {
	q, args := "SELECT 1 FROM words_vocab WHERE term = ?", []any{word}
	if asPrefix {
		q, args = "SELECT 1 FROM words_vocab WHERE term >= ? AND term < ? LIMIT 1", []any{word, word + endOfRange}
	}
	for _, g := range gens {
		var one int
		err := g.reader.QueryRowContext(ctx, q, args...).Scan(&one)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
	}
	return false, nil
}

// bestSuggestion picks the closest candidate across all sources, the most common one on a tie.
func bestSuggestion(gens []*generation, word string, maxDistance int) (string, bool) {
	total := make(map[string]suggestion)
	for _, g := range gens {
		v := g.vocab.Load()
		if v == nil {
			continue
		}
		for _, s := range v.suggest(word, maxDistance) {
			prev, ok := total[s.word]
			if ok {
				s.docs += prev.docs
			}
			total[s.word] = s
		}
	}
	if len(total) == 0 {
		return "", false
	}
	best := slices.MinFunc(slices.Collect(maps.Values(total)), func(a, b suggestion) int {
		return cmp.Or(cmp.Compare(a.distance, b.distance), cmp.Compare(b.docs, a.docs), cmp.Compare(a.word, b.word))
	})
	return best.word, true
}

// ftsExpression builds the FTS5 query: every positive term quoted, the last word as a prefix, joined with
// AND (or OR when relaxing), exclusions with NOT.
func ftsExpression(terms []term, anyWord bool) string {
	last := lastWord(terms)
	var pos, neg []string
	for i, t := range terms {
		q := quote(t.text)
		if i == last {
			q += "*"
		}
		if t.exclude {
			neg = append(neg, q)
		} else {
			pos = append(pos, q)
		}
	}
	join := " AND "
	if anyWord {
		join = " OR "
	}
	expr := "(" + strings.Join(pos, join) + ")"
	for _, n := range neg {
		expr += " NOT " + n
	}
	return expr
}

// exclusionExpression matches documents with any excluded word, "" when nothing is excluded.
func exclusionExpression(terms []term) string {
	var parts []string
	for _, t := range terms {
		if t.exclude {
			parts = append(parts, quote(t.text))
		}
	}
	return strings.Join(parts, " OR ")
}

// namesExpression matches every positive word of three or more letters inside titles, aliases and
// paths. It returns "" when there's nothing to match that way.
func namesExpression(terms []term, anyWord bool) string {
	var parts []string
	for _, t := range terms {
		if t.exclude {
			continue
		}
		if utf8.RuneCountInString(t.text) < 3 {
			if !anyWord {
				return "" // the trigram index can't match it, so AND can't hold
			}
			continue
		}
		parts = append(parts, quote(t.text))
	}
	if len(parts) == 0 {
		return ""
	}
	join := " AND "
	if anyWord {
		join = " OR "
	}
	return strings.Join(parts, join)
}

func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
