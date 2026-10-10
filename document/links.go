package document

import (
	"net/url"
	"path"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// markdownLink resolves a markdown link destination relative to the document it appears in.
func markdownLink(dest, text, docPath string) Link {
	return Link{
		Target:    dest,
		Candidate: pathCandidate(dest, docPath),
		Kind:      LinkMarkdown,
		Text:      text,
	}
}

// relatedLink turns an entry of the related field into a link. Entries written as wikilinks ("[[x]]",
// how Obsidian stores links in properties) are treated as wikilinks.
func relatedLink(entry, docPath string) Link {
	if inner, ok := strings.CutPrefix(entry, "[["); ok {
		if inner, ok := strings.CutSuffix(inner, "]]"); ok {
			target, _, _ := strings.Cut(inner, "|")
			target, _, _ = strings.Cut(target, "#")
			return Link{Target: entry, Candidate: wikiCandidate(target), Kind: LinkWiki}
		}
	}
	return Link{Target: entry, Candidate: pathCandidate(entry, docPath), Kind: LinkRelated}
}

// pathCandidate returns the vault path a link destination points to, or "" when it leaves the vault:
// it has a scheme, it's only a fragment, or it climbs above the vault root.
func pathCandidate(dest, docPath string) string {
	u, err := url.Parse(dest)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Path == "" {
		return ""
	}
	p := u.Path // url.Parse has decoded %20 and friends
	if strings.HasPrefix(p, "/") {
		p = path.Clean(strings.TrimLeft(p, "/"))
	} else {
		p = path.Join(path.Dir(docPath), p)
	}
	if p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return ""
	}
	return norm.NFC.String(p)
}

// wikiCandidate returns the name a wikilink target matches documents by: LinkName of the target.
func wikiCandidate(target string) string {
	name := path.Base(strings.TrimSpace(target))
	if name == "." || name == "/" {
		return ""
	}
	return LinkName(name)
}
