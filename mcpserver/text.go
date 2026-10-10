package mcpserver

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
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

// fence wraps vault content in a block named kind. The closing tag carries a random boundary, so
// content can't end the block early: it can't know the boundary. Look-alike tags in the content are
// escaped too.
func fence(kind, content string) string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	boundary := hex.EncodeToString(b[:])
	content = escapeMarkers(content)
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return "<" + kind + " boundary=\"" + boundary + "\">\n" + content + "</" + kind + " boundary=\"" + boundary + "\">\n"
}

// fenceNames are the block names fence uses.
var fenceNames = []string{"document", "guidelines"}

// escapeMarkers escapes every "<" that starts something a reader could take for a fence tag: the name
// after it, with invisible characters, spacing and a slash skipped, and look-alike letters folded to
// ASCII, is one of fenceNames.
func escapeMarkers(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if isLessThan(r) && startsFenceName(s[i+size:]) {
			b.WriteString("&lt;")
		} else {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// isLessThan reports whether r looks like "<".
func isLessThan(r rune) bool {
	return r == '<' || r == '＜' || r == '﹤' || r == '‹' || r == '〈' || r == '⟨'
}

// startsFenceName reports whether s, read loosely, starts with a fence name.
func startsFenceName(s string) bool {
	var name []rune
	for _, r := range s {
		if len(name) == len("guidelines") {
			break
		}
		switch {
		case ignorable(r):
			continue
		case len(name) == 0 && (r == '/' || r == '／' || r == '⁄' || r == '∕'):
			continue
		}
		name = append(name, fold(r))
	}
	folded := string(name)
	for _, n := range fenceNames {
		if strings.HasPrefix(folded, n) {
			return true
		}
	}
	return false
}

// ignorable reports whether r is invisible or blank: spacing, controls, format and combining marks.
func ignorable(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Mn, unicode.Me, unicode.Z)
}

// lookAlikes maps letters that look like the ASCII letters of the fence names to them.
var lookAlikes = map[rune]rune{
	'а': 'a', 'с': 'c', 'ԁ': 'd', 'е': 'e', 'ё': 'e', 'ɡ': 'g', 'һ': 'h', 'і': 'i', 'ӏ': 'l', 'ⅼ': 'l',
	'м': 'm', 'п': 'n', 'о': 'o', 'ο': 'o', 'ѕ': 's', 'т': 't', 'υ': 'u', 'ս': 'u', 'ı': 'i', 'ℓ': 'l',
	'ⅾ': 'd', 'ⅰ': 'i', 'ⅿ': 'm', 'ⅽ': 'c',
}

// fold maps r to the lower-case ASCII letter it looks like, where it looks like one.
func fold(r rune) rune {
	if n := []rune(norm.NFKC.String(string(r))); len(n) == 1 { // fullwidth, mathematical and similar forms
		r = n[0]
	}
	r = unicode.ToLower(r)
	if a, ok := lookAlikes[r]; ok {
		return a
	}
	return r
}
