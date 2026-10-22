package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/xaroth/speki/auth/access"
	"github.com/xaroth/speki/knowledge"
)

// ReadInput asks for a document, or part of it.
type ReadInput struct {
	Vault     string `json:"vault" jsonschema:"the vault, owner/repo"`
	Path      string `json:"path" jsonschema:"the document's path in the vault, e.g. services/esi-proxy.md"`
	FromLine  int    `json:"from_line,omitempty" jsonschema:"first line to return, counting from 1 at the top of the file. Default 1"`
	LineCount int    `json:"line_count,omitempty" jsonschema:"how many lines to return. Default: as many as fit in 1,000 lines and one answer"`
}

// Metadata is a document's frontmatter, as speki understands it.
type Metadata struct {
	Title       string         `json:"title"`
	Description string         `json:"description,omitempty"`
	Tags        []string       `json:"tags,omitempty"`
	Aliases     []string       `json:"aliases,omitempty"`
	Status      string         `json:"status"`
	Related     []string       `json:"related,omitempty"`
	Created     string         `json:"created,omitempty"`
	Updated     string         `json:"updated,omitempty"`
	Extra       map[string]any `json:"extra,omitempty" jsonschema:"frontmatter keys speki doesn't own"`
}

// ReadOutput describes a document, or the part of it returned. The content and outline are in the text
// result only, fenced, so they go out once.
type ReadOutput struct {
	Vault      string   `json:"vault"`
	Path       string   `json:"path"`
	Version    string   `json:"version"`
	Metadata   Metadata `json:"metadata"`
	FromLine   int      `json:"from_line"`
	LineCount  int      `json:"line_count"`
	TotalLines int      `json:"total_lines"`
	Warnings   []string `json:"warnings,omitempty" jsonschema:"problems with the frontmatter, from changes made outside speki"`
}

func (s *Server) read(ctx context.Context, g *access.Grant, in ReadInput) (ReadOutput, string, error) {
	p, err := s.kb.Read(ctx, g, knowledge.ReadRequest{
		Vault:     in.Vault,
		Path:      in.Path,
		FromLine:  in.FromLine,
		LineCount: in.LineCount,
		MaxBytes:  readBudget(in.Vault, in.Path),
	})
	if err != nil {
		return ReadOutput{}, "", err
	}
	d := p.Document
	out := ReadOutput{
		Vault:   p.Vault,
		Path:    d.Path,
		Version: d.Version,
		Metadata: Metadata{
			Title:       d.Title,
			Description: d.Description,
			Tags:        d.Tags,
			Aliases:     d.Aliases,
			Status:      string(d.Status),
			Related:     d.Related,
			Created:     d.Created,
			Updated:     d.Updated,
			Extra:       d.Extra,
		},
		FromLine:   p.FromLine,
		LineCount:  p.LineCount,
		TotalLines: p.TotalLines,
		Warnings:   p.Warnings,
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", oneLine(p.Vault), oneLine(d.Path))
	last := p.FromLine + p.LineCount - 1
	if p.LineCount == 0 {
		last = p.FromLine - 1
	}
	fmt.Fprintf(&b, "version %s, lines %d-%d of %d\n", d.Version, p.FromLine, last, p.TotalLines)
	for _, w := range p.Warnings {
		fmt.Fprintf(&b, "warning: %s\n", oneLine(w))
	}
	if len(p.Outline) > 0 {
		var outline strings.Builder
		for _, h := range p.Outline {
			fmt.Fprintf(&outline, "line %d: %s %s\n", h.Line, strings.Repeat("#", h.Level), oneLine(h.Text))
		}
		b.WriteString(fence("outline", outline.String()))
	}
	b.WriteString(fence("document", p.Content))
	return out, b.String(), nil
}

// readBudget is what a page of the document may take in one answer: the vault and path go out beside it.
func readBudget(vault, path string) int {
	return maxAnswer - answerOverhead - encodedSize(vault) - encodedSize(path)
}
