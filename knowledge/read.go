package knowledge

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/eve-online-tools/speki/access"
	"github.com/eve-online-tools/speki/document"
	"github.com/eve-online-tools/speki/index"
)

// What one read returns at most.
const (
	maxReadLines  = 1000
	maxReadBytes  = 64 << 10 // content and outline together
	maxOutline    = 200      // headings
	maxOutlineLen = 16 << 10 // bytes of the outline, counting each heading's text and a little for the rest
	maxHeading    = 200      // bytes of one heading's text
)

// ReadRequest asks for a document, or part of it.
type ReadRequest struct {
	Vault     string
	Path      string
	FromLine  int // 1-based; 0 means 1
	LineCount int // 0 means as many as fit
}

// Page is a document, or the part of it that was asked for.
type Page struct {
	Vault      string
	Document   index.Document // metadata and version
	Content    string         // raw file text, frontmatter included
	FromLine   int
	LineCount  int
	TotalLines int
	// Outline lists the document's headings when Content isn't the whole document.
	Outline  []document.Heading
	Warnings []string
}

// Read returns a document's raw text, or the lines asked for, up to 1,000 lines and 64 KiB.
func (b *Base) Read(ctx context.Context, g *access.Grant, req ReadRequest) (Page, error) {
	if req.FromLine < 0 || req.LineCount < 0 {
		return Page{}, errorf(Invalid, "from_line and line_count can't be negative")
	}
	v, c, err := b.open(g, req.Vault)
	if err != nil {
		return Page{}, err
	}
	path := strings.TrimPrefix(req.Path, "/")
	doc, raw, err := b.load(ctx, c, path)
	if errors.Is(err, index.ErrNotFound) {
		return Page{}, errorf(NotFound, "%s has no document %s. Use search or list to find it", v.Name, path)
	}
	if err != nil {
		return Page{}, indexError(v.Name, err)
	}

	lines := splitLines(string(raw))
	from := max(req.FromLine, 1)
	if from > max(len(lines), 1) { // an empty document can still be read from line 1
		return Page{}, errorf(Invalid, "%s has %d lines, so from_line %d is past the end", path, len(lines), from)
	}
	want := req.LineCount
	if want == 0 || want > maxReadLines {
		want = maxReadLines
	}

	p := Page{Vault: v.Name, Document: doc, FromLine: from, TotalLines: len(lines), Warnings: slices.Clone(doc.Warnings)}
	// The outline comes with a partial read, and counts against its 64 KiB, so it's worked out first.
	budget := maxReadBytes
	partial := from > 1 || want < len(lines) || len(raw) > maxReadBytes
	if partial {
		parsed, err := document.Parse(path, raw)
		if err == nil {
			p.Outline = outline(parsed.Headings, &p.Warnings)
			budget -= outlineSize(p.Outline)
		}
	}
	var sb strings.Builder
	for _, line := range lines[from-1:] {
		if p.LineCount == want {
			break
		}
		if sb.Len()+len(line) > budget {
			if p.LineCount == 0 { // one line over the cap on its own: show its start
				sb.WriteString(cutUTF8(line, budget))
				p.LineCount = 1
				p.Warnings = append(p.Warnings, fmt.Sprintf("line %d is longer than fits in one read; only its start is shown", from))
			}
			break
		}
		sb.WriteString(line)
		p.LineCount++
	}
	p.Content = sb.String()
	return p, nil
}

// outline returns the headings a partial read shows: at most maxOutline, each cut to maxHeading bytes,
// and maxOutlineLen in all. It adds a warning when it leaves headings out.
func outline(headings []document.Heading, warnings *[]string) []document.Heading {
	var out []document.Heading
	size := 0
	for _, h := range headings {
		h.Text = clip(h.Text, maxHeading)
		if len(out) == maxOutline || size+headingSize(h) > maxOutlineLen {
			*warnings = append(*warnings, fmt.Sprintf("the outline shows the first %d of %d headings only", len(out), len(headings)))
			break
		}
		out = append(out, h)
		size += headingSize(h)
	}
	return out
}

// headingSize is roughly what a heading takes in an answer: its text, its level and line, and the
// JSON around them.
func headingSize(h document.Heading) int { return len(h.Text) + 32 }

func outlineSize(headings []document.Heading) int {
	n := 0
	for _, h := range headings {
		n += headingSize(h)
	}
	return n
}

// clip returns s if it's at most n bytes, or its start with "…" added, n bytes in all.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return cutUTF8(s, n-len("…")) + "…"
}

// load returns a document's metadata and content. A sync between the two lookups can leave the index
// pointing at a version the git copy no longer has, so it tries a second time.
func (b *Base) load(ctx context.Context, c Copy, path string) (index.Document, []byte, error) {
	var err error
	for range 2 {
		var doc index.Document
		doc, err = c.Index().Document(ctx, path)
		if err != nil {
			return index.Document{}, nil, err
		}
		var raw []byte
		raw, err = c.Blob(doc.Version)
		if err == nil {
			return doc, raw, nil
		}
	}
	return index.Document{}, nil, err
}

// splitLines splits text into lines, each keeping its line ending. A final line ending doesn't start
// another line.
func splitLines(s string) []string {
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// cutUTF8 returns the longest prefix of s of at most n bytes that doesn't split a character.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
