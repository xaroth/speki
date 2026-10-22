// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// term is one part of a query.
type term struct {
	text    string // lowercased, without diacritics; words of a phrase separated by spaces
	phrase  bool
	quoted  bool // written in quotes: matched as written, no prefix and no correction
	exclude bool
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
				terms = addTerm(terms, term{
					text:    strings.Join(words, " "),
					phrase:  len(words) > 1,
					quoted:  true,
					exclude: exclude,
				})
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
			terms = addTerm(terms, term{text: w, exclude: exclude})
		}
	}
	return terms
}

func addTerm(terms []term, t term) []term {
	if slices.Contains(terms, t) {
		return terms
	}
	return append(terms, t)
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
			if t.quoted {
				return -1
			}
			return i
		}
	}
	return -1
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
	if len(neg) > 0 { // one group, not one NOT per word, which nests the expression a level each
		expr += " NOT (" + strings.Join(neg, " OR ") + ")"
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
