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
