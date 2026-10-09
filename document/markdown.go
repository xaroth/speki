package document

import (
	"bytes"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// markdown parses GitHub-flavoured markdown plus wikilinks. It holds no state between parses and is safe
// for concurrent use, so one is shared.
var markdown = goldmark.New(
	goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.Linkify),
	goldmark.WithParserOptions(
		parser.WithInlineParsers(util.Prioritized(wikiLinkParser{}, 199)), // before links (200)
	),
)

// parseMarkdown collects headings and links from body. bodyLine is the file line the body starts on.
func parseMarkdown(body string, bodyLine int, docPath string) ([]Heading, []Link) {
	src := []byte(body)
	root := markdown.Parser().Parse(text.NewReader(src))

	var headings []Heading
	var links []Link
	_ = ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.Heading:
			if n.Lines().Len() == 0 {
				return ast.WalkSkipChildren, nil // an empty heading has nothing to show or find
			}
			headings = append(headings, Heading{
				Level: n.Level,
				Text:  inlineText(n, src),
				Line:  bodyLine + bytes.Count(src[:n.Lines().At(0).Start], []byte("\n")),
			})
		case *ast.Link:
			links = append(links, markdownLink(string(n.Destination), inlineText(n, src), docPath))
			return ast.WalkSkipChildren, nil
		case *ast.Image:
			links = append(links, markdownLink(string(n.Destination), inlineText(n, src), docPath))
			return ast.WalkSkipChildren, nil
		case *ast.AutoLink:
			url := string(n.URL(src))
			links = append(links, Link{Target: url, Kind: LinkMarkdown, Text: url})
		case *wikiLink:
			links = append(links, n.link())
		}
		return ast.WalkContinue, nil
	})
	return headings, links
}

// inlineText returns the plain text inside an inline container, without markup.
func inlineText(n ast.Node, src []byte) string {
	var b strings.Builder
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch c := c.(type) {
		case *ast.Text:
			b.Write(c.Segment.Value(src))
			if c.SoftLineBreak() || c.HardLineBreak() {
				b.WriteByte(' ')
			}
		case *ast.String:
			b.Write(c.Value)
		case *ast.AutoLink:
			b.Write(c.Label(src))
		case *wikiLink:
			b.WriteString(c.label())
		}
		return ast.WalkContinue, nil
	})
	return strings.TrimSpace(b.String())
}

// wikiLink is an Obsidian-style [[target#fragment|label]] link.
type wikiLink struct {
	ast.BaseInline
	target, fragment, text string
}

var kindWikiLink = ast.NewNodeKind("WikiLink")

func (n *wikiLink) Kind() ast.NodeKind { return kindWikiLink }

func (n *wikiLink) Dump(src []byte, level int) {
	ast.DumpHelper(n, src, level, map[string]string{"Target": n.target}, nil)
}

func (n *wikiLink) label() string {
	if n.text != "" {
		return n.text
	}
	return n.target
}

func (n *wikiLink) link() Link {
	raw := n.target
	if n.fragment != "" {
		raw += "#" + n.fragment
	}
	if n.text != "" {
		raw += "|" + n.text
	}
	return Link{
		Target:    "[[" + raw + "]]",
		Candidate: wikiCandidate(n.target),
		Kind:      LinkWiki,
		Text:      n.label(),
	}
}

// wikiLinkParser recognises [[...]] and Obsidian embeds ![[...]] on one line. Anything else is left to
// the regular link parser.
type wikiLinkParser struct{}

func (wikiLinkParser) Trigger() []byte { return []byte{'[', '!'} }

func (wikiLinkParser) Parse(_ ast.Node, block text.Reader, _ parser.Context) ast.Node {
	line, _ := block.PeekLine()
	start := 2
	if bytes.HasPrefix(line, []byte("!")) {
		start = 3
	}
	if !bytes.HasPrefix(line[start-2:], []byte("[[")) {
		return nil
	}
	end := bytes.Index(line, []byte("]]"))
	if end < start {
		return nil
	}
	inner := string(line[start:end])
	if inner == "" || strings.ContainsAny(inner, "[]\n") {
		return nil
	}
	block.Advance(end + 2)

	// Inside tables Obsidian escapes the separator as \|.
	inner = strings.ReplaceAll(inner, `\|`, "|")
	n := &wikiLink{}
	inner, n.text, _ = strings.Cut(inner, "|")
	n.target, n.fragment, _ = strings.Cut(inner, "#")
	n.target = strings.TrimSpace(n.target)
	n.text = strings.TrimSpace(n.text)
	return n
}
