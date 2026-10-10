package mcpserver

import (
	"regexp"
	"strings"
	"unicode"
)

// oneLine makes vault-supplied text safe to show on one line of a text result: line breaks become
// spaces, and control and format characters (which can hide or reorder text) are dropped.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
}

// marker matches the start of a <document> or <guidelines> tag, opening or closing, in any letter case
// and with any spacing.
var marker = regexp.MustCompile(`(?i)<(\s*/?\s*(?:document|guidelines))`)

// escapeMarkers escapes the tags text results use to fence vault content, so content can't fake the end
// of its block.
func escapeMarkers(s string) string {
	return marker.ReplaceAllString(s, "&lt;$1")
}
