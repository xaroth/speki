// Package document turns markdown files with YAML frontmatter into Documents and back, and decides
// which paths speki may write. It knows nothing about vaults, git or the index.
package document

import (
	"bytes"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// Limits on what speki accepts.
const (
	MaxSize            = 1 << 20  // a whole document
	maxFrontmatterSize = 64 << 10 // the frontmatter block
)

// Status says whether a document is current.
type Status string

// The statuses a document can have. A document without a status is active.
const (
	StatusDraft      Status = "draft"
	StatusActive     Status = "active"
	StatusDeprecated Status = "deprecated"
)

// Meta holds the frontmatter fields speki owns, plus everything else in Extra.
type Meta struct {
	Title       string
	Description string
	Tags        []string
	Aliases     []string
	Status      Status
	Related     []string
	Created     time.Time // zero when missing
	Updated     time.Time // zero when missing
	Extra       map[string]any
}

// Heading is a markdown heading. Line counts from 1 at the top of the file, frontmatter included.
type Heading struct {
	Level int
	Text  string
	Line  int
}

// LinkKind says where a link came from.
type LinkKind string

// The kinds of links speki records.
const (
	LinkMarkdown LinkKind = "markdown"
	LinkWiki     LinkKind = "wikilink"
	LinkRelated  LinkKind = "related"
)

// Link is a link found in a document.
type Link struct {
	Target string // as written
	// Candidate is what the link may point to in the same vault: a cleaned path in NFC for markdown links
	// and related entries, LinkName of the target for wikilinks. Empty for links that leave the vault
	// (web addresses, paths above the vault root).
	Candidate string
	Kind      LinkKind
	Text      string
}

// Document is a parsed markdown file.
type Document struct {
	Meta     Meta
	Body     string // everything after the frontmatter
	Headings []Heading
	Links    []Link
	// Warnings lists problems with the frontmatter that were ignored while parsing. Each one starts with
	// the field it's about.
	Warnings []string

	path  string
	raw   []byte
	front *frontmatter // nil when the file has no frontmatter block
	// titleFromPath is set when no title was found and the path stands in for it.
	titleFromPath bool
}

// errUnusable is returned by Parse for files speki can't index at all.
var errUnusable = errors.New("unusable document")

// ErrTooLarge is returned by Check and Touch for a document or frontmatter over its size limit.
var ErrTooLarge = errors.New("too large")

// ErrUnstable is returned by Touch when the frontmatter's comments can't be kept in place.
var ErrUnstable = errors.New("frontmatter comments can't be kept in place")

// Parse reads a document. It fails only when the file can't be used at all: it isn't UTF-8, or its
// frontmatter block is too large. Every other problem ends up in Warnings. The error text reaches agents
// as a broken document's reason and is listed in docs/tools/overview.md, so keep it stable.
func Parse(docPath string, raw []byte) (*Document, error) {
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("%w: not UTF-8", errUnusable)
	}
	front, bodyOff, err := splitFrontmatter(raw)
	if err != nil {
		return nil, err
	}
	if front == nil && bytes.HasPrefix(raw, bom) {
		bodyOff = len(bom) // a byte order mark isn't part of the text
	}

	d := &Document{
		path:  docPath,
		raw:   raw,
		front: front,
		Body:  string(raw[bodyOff:]),
		Meta:  Meta{Status: StatusActive},
	}
	if front != nil {
		d.Warnings = front.readMeta(&d.Meta)
	}

	bodyLine := 1 + bytes.Count(raw[:bodyOff], []byte("\n"))
	d.Headings, d.Links = parseMarkdown(d.Body, bodyLine, docPath)
	for _, r := range d.Meta.Related {
		d.Links = append(d.Links, relatedLink(r, docPath))
	}

	if d.Meta.Title == "" {
		d.Meta.Title = firstH1(d.Headings)
	}
	if d.Meta.Title == "" {
		d.Meta.Title = docPath
		d.titleFromPath = true
	}
	return d, nil
}

func firstH1(hs []Heading) string {
	for _, h := range hs {
		if h.Level == 1 {
			return h.Text
		}
	}
	return ""
}

type frontmatter struct {
	open, close string     // the delimiter lines as written, including line endings
	content     []byte     // between the delimiters
	eol         string     // "\n" or "\r\n"
	doc         *yaml.Node // the YAML document, which holds comments above and below the mapping
	node        *yaml.Node // the mapping inside doc
	// broken explains why the block as a whole couldn't be used. node is nil then.
	broken string
}
