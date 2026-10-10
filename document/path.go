package document

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Limits on paths speki writes.
const (
	maxSegmentBytes = 255
	maxFolders      = 10
)

// forbiddenChars break Obsidian, Windows or markdown links.
const forbiddenChars = `[]#^|:?*<>"\`

// Fold returns the form of p used to find paths that differ only in letter case or Unicode
// normalisation: lowercased, in NFC. It normalises before lowercasing as well, since some letters
// lowercase differently composed and decomposed (İ and I with a combining dot).
func Fold(p string) string {
	return norm.NFC.String(strings.ToLower(norm.NFC.String(p)))
}

// LinkName is the name wikilinks find a document at p by: its file name without .md, folded.
func LinkName(p string) string {
	return Fold(strings.TrimSuffix(path.Base(p), ".md"))
}

// ErrExists is returned by CheckPath for a file that already exists, spelled in another normalisation.
var ErrExists = errors.New("already exists")

// CheckPath reports whether p is a path speki may create, and returns it in NFC. existing looks up a
// folded path (see Fold) and returns the path git has for it, if any. It's called for the file and each
// of its folders, so "Runbooks/x.md" is refused when "runbooks/" exists, and so is a file that exists in
// another normalisation. Where git has a folder that differs only in Unicode normalisation (folders made on
// macOS are often NFD), the returned path uses git's spelling of that folder instead, so the document lands
// in it. A nil existing skips that check.
func CheckPath(p string, existing func(folded string) (string, bool)) (string, error) {
	if !utf8.ValidString(p) {
		return "", errors.New("path: not valid UTF-8")
	}
	p = norm.NFC.String(p)

	switch {
	case p == "":
		return "", errors.New("path: empty")
	case strings.HasPrefix(p, "/"):
		return "", fmt.Errorf("path %q: must be relative to the vault root", p)
	case !strings.HasSuffix(p, ".md"):
		return "", fmt.Errorf("path %q: must end in .md", p)
	case strings.ContainsAny(p, forbiddenChars):
		return "", fmt.Errorf("path %q: may not contain any of %s", p, forbiddenChars)
	case strings.ContainsFunc(p, unicode.IsControl):
		return "", fmt.Errorf("path %q: may not contain control characters", p)
	case strings.ContainsFunc(p, func(r rune) bool { return unicode.Is(unicode.Cf, r) }):
		return "", fmt.Errorf("path %q: may not contain invisible characters such as zero-width spaces or direction marks", p)
	case path.Clean(p) != p || !filepath.IsLocal(p):
		return "", fmt.Errorf("path %q: must be a clean relative path without . or .. segments", p)
	}

	segments := strings.Split(p, "/")
	if len(segments)-1 > maxFolders {
		return "", fmt.Errorf("path %q: at most %d folders deep", p, maxFolders)
	}
	for i, s := range segments {
		if reservedOnWindows(s) {
			return "", fmt.Errorf("path %q: %q is a name Windows reserves; Obsidian users on Windows couldn't open it", p, s)
		}
		if strings.HasSuffix(s, " ") || strings.HasSuffix(s, ".") {
			return "", fmt.Errorf("path %q: no part may end in a space or a dot", p)
		}
		if strings.HasPrefix(s, ".") {
			return "", fmt.Errorf("path %q: no part may start with a dot", p)
		}
		if len(s) > maxSegmentBytes {
			return "", fmt.Errorf("path %q: each part may be at most %d bytes", p, maxSegmentBytes)
		}
		if existing == nil {
			continue
		}
		prefix := strings.Join(segments[:i+1], "/")
		have, ok := existing(Fold(prefix))
		folder := i < len(segments)-1
		switch {
		case !ok || have == prefix:
		case folder && norm.NFC.String(have) == norm.NFC.String(prefix):
			copy(segments, strings.Split(have, "/")) // the same folder, stored decomposed: use git's bytes
		case norm.NFC.String(have) == norm.NFC.String(prefix):
			return "", fmt.Errorf("path %q: %q %w", p, have, ErrExists)
		default:
			return "", fmt.Errorf("path %q: %q already exists and differs only in letter case", p, have)
		}
	}
	p = strings.Join(segments, "/")
	for s := range strings.SplitSeq(p, "/") { // git's spelling of a folder may be longer
		if len(s) > maxSegmentBytes {
			return "", fmt.Errorf("path %q: each part may be at most %d bytes", p, maxSegmentBytes)
		}
	}
	return p, nil
}

// reservedOnWindows reports whether a path segment is a device name Windows reserves (CON, NUL, COM1 and
// the like), with or without an extension.
func reservedOnWindows(segment string) bool {
	base, _, _ := strings.Cut(strings.ToLower(segment), ".")
	switch base {
	case "con", "prn", "aux", "nul":
		return true
	}
	return len(base) == 4 && (strings.HasPrefix(base, "com") || strings.HasPrefix(base, "lpt")) && base[3] >= '1' && base[3] <= '9'
}
