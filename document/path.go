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
// normalisation: NFC, lowercased.
func Fold(p string) string {
	return strings.ToLower(norm.NFC.String(p))
}

// CheckPath reports whether p is a path speki may create, and returns it in NFC. existing looks up a
// folded path (see Fold) and returns the path git has for it, if any. It's called for the file and each
// of its folders, so "Runbooks/x.md" is refused when "runbooks/" exists. A nil existing skips that check.
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
	case path.Clean(p) != p || !filepath.IsLocal(p):
		return "", fmt.Errorf("path %q: must be a clean relative path without . or .. segments", p)
	}

	segments := strings.Split(p, "/")
	if len(segments)-1 > maxFolders {
		return "", fmt.Errorf("path %q: at most %d folders deep", p, maxFolders)
	}
	for i, s := range segments {
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
		if ok && have != prefix {
			return "", fmt.Errorf("path %q: %q already exists and differs only in letter case or accents", p, have)
		}
	}
	return p, nil
}
