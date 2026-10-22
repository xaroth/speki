// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index

import (
	"context"
	"errors"
	"unicode/utf8"
)

// vocabulary is a generation's unstemmed words with their document counts, for typo correction and
// prefix checks. It may lag a moment behind the index; known-word checks go to SQL instead.
type vocabulary struct {
	words    []string
	docs     []int
	trigrams map[string][]int32 // trigram -> indexes into words
}

// reloadVocabulary rebuilds the current generation's vocabulary in the background. Reloads run one at a
// time, and requests made while one is queued are folded into it.
func (x *Index) reloadVocabulary() {
	x.vocabMu.Lock()
	if x.vocabPending {
		x.vocabMu.Unlock()
		return
	}
	x.vocabPending = true
	x.vocabMu.Unlock()

	x.goBackground(func() {
		x.vocabLoad.Lock()
		defer x.vocabLoad.Unlock()
		x.vocabMu.Lock()
		x.vocabPending = false
		x.vocabMu.Unlock()

		g, err := x.acquire()
		if err != nil {
			return // closed
		}
		defer g.release()
		v, err := loadVocabulary(x.ctx, g)
		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil {
			x.logger.Warn("loading vocabulary failed", "err", err)
			return
		}
		g.vocab.Store(v)
	})
}

func loadVocabulary(ctx context.Context, g *generation) (*vocabulary, error) {
	rows, err := g.reader.QueryContext(ctx, "SELECT term, doc FROM words_vocab ORDER BY term")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	v := &vocabulary{trigrams: make(map[string][]int32)}
	for rows.Next() {
		var term string
		var docs int
		err = rows.Scan(&term, &docs)
		if err != nil {
			return nil, err
		}
		i := int32(len(v.words))
		v.words = append(v.words, term)
		v.docs = append(v.docs, docs)
		for _, t := range trigramsOf(term) {
			v.trigrams[t] = append(v.trigrams[t], i)
		}
	}
	return v, rows.Err()
}

type suggestion struct {
	word     string
	distance int
	docs     int
}

// suggest returns the words within maxDistance edits of w.
func (v *vocabulary) suggest(w string, maxDistance int) []suggestion {
	seen := make(map[int32]bool)
	var out []suggestion
	n := utf8.RuneCountInString(w)
	if n <= 4 {
		// A short word can share no trigram with its correction ("cot" and "cat"): compare it with every
		// word of a nearby length instead, which is cheap for words this short.
		for i, cand := range v.words {
			if abs(utf8.RuneCountInString(cand)-n) > maxDistance {
				continue
			}
			if d := distance(w, cand, maxDistance); d <= maxDistance {
				out = append(out, suggestion{word: cand, distance: d, docs: v.docs[i]})
			}
		}
		return out
	}
	for _, t := range trigramsOf(w) {
		for _, i := range v.trigrams[t] {
			if seen[i] {
				continue
			}
			seen[i] = true
			cand := v.words[i]
			if abs(utf8.RuneCountInString(cand)-n) > maxDistance {
				continue
			}
			d := distance(w, cand, maxDistance)
			if d <= maxDistance {
				out = append(out, suggestion{word: cand, distance: d, docs: v.docs[i]})
			}
		}
	}
	return out
}

// trigramsOf returns the trigrams of w padded with spaces, so short words and word edges count.
func trigramsOf(w string) []string {
	r := []rune(" " + w + " ")
	var out []string
	for i := 0; i+3 <= len(r); i++ {
		out = append(out, string(r[i:i+3]))
	}
	return out
}

// distance is the optimal string alignment distance (Levenshtein plus adjacent transpositions) between a
// and b, cut short at limit+1.
func distance(a, b string, limit int) int {
	ra, rb := []rune(a), []rune(b)
	prev2 := make([]int, len(rb)+1)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		best := cur[0]
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] {
				cur[j] = min(cur[j], prev2[j-2]+1)
			}
			best = min(best, cur[j])
		}
		if best > limit {
			return limit + 1
		}
		prev2, prev, cur = prev, cur, prev2
	}
	return prev[len(rb)]
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
