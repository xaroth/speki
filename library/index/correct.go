// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"maps"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// correct replaces words that appear in none of the sources with the closest known word, and returns
// what it replaced. Whether a word exists is asked of the index itself, so a word the agent wrote a
// moment ago counts even before the in-memory vocabulary catches up.
func correct(ctx context.Context, gens []*generation, terms []term) (map[string]string, error) {
	last := lastWord(terms)
	var corrected map[string]string
	for i := range terms {
		t := &terms[i]
		n := utf8.RuneCountInString(t.text)
		if t.quoted || t.exclude || n < 3 || strings.ContainsFunc(t.text, unicode.IsDigit) {
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
// A word that matches by its stem ("runs" where documents say "run") counts as known too: correcting it
// to a nearby word would override stemming.
func knownAnywhere(ctx context.Context, gens []*generation, word string, asPrefix bool) (bool, error) {
	queries := []struct {
		q    string
		args []any
	}{{"SELECT 1 FROM words_vocab WHERE term = ?", []any{word}}}
	if asPrefix {
		queries[0].q, queries[0].args = "SELECT 1 FROM words_vocab WHERE term >= ? AND term < ? LIMIT 1", []any{word, word + endOfRange}
	}
	queries = append(queries, struct {
		q    string
		args []any
	}{"SELECT 1 FROM fts WHERE fts MATCH ? LIMIT 1", []any{quote(word)}})
	for _, q := range queries {
		for _, g := range gens {
			var one int
			err := g.reader.QueryRowContext(ctx, q.q, q.args...).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return false, err
			}
			return true, nil
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
