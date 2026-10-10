package mcpserver

import (
	"regexp"
	"strings"
	"unicode"
)

// oneLine makes vault-supplied text safe to show on one line of a text result: line breaks become
// spaces, control and format characters (which can hide or reorder text) are dropped, and fence tags
// are escaped.
func oneLine(s string) string {
	return escapeMarkers(strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t' || unicode.In(r, unicode.Zl, unicode.Zp):
			return ' '
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s))
}

// marker matches the start of a <document> or <guidelines> tag, opening or closing, in any letter case
// and with any spacing, Unicode spaces and invisible format characters included.
var marker = regexp.MustCompile(`(?i)<([\s\p{Z}\p{Cf}]*/?[\s\p{Z}\p{Cf}]*(?:document|guidelines))`)

// escapeMarkers escapes the tags text results use to fence vault content, so content can't fake the end
// of its block.
func escapeMarkers(s string) string {
	return marker.ReplaceAllString(s, "&lt;$1")
}
