package index

import (
	"unicode/utf8"

	"github.com/eve-online-tools/speki/document"
)

// How much of a title and a description (or snippet) answers carry, in bytes. The index keeps them
// whole, for searching; a careless document can't fill an answer with them.
const (
	maxTitle       = document.MaxTitle
	maxDescription = document.MaxDescription
)

// clip returns s if it's at most n bytes, or its start with "…" added, n bytes in all, without
// splitting a character.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	n -= len("…")
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
