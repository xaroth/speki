package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/xaroth/speki/auth/access"
	"github.com/xaroth/speki/knowledge"
)

// HistoryInput asks for the latest commits.
type HistoryInput struct {
	Vault  string `json:"vault" jsonschema:"the vault, owner/repo"`
	Path   string `json:"path,omitempty" jsonschema:"only commits that changed this document. Leave out for the whole vault"`
	Limit  int    `json:"limit,omitempty" jsonschema:"how many commits, 1 to 50. Default 10"`
	Cursor string `json:"cursor,omitempty" jsonschema:"next_cursor from the previous call with the same vault and path, to get older commits"`
}

// CommitSummary is one commit.
type CommitSummary struct {
	SHA     string `json:"sha"`
	Author  string `json:"author"`
	Date    string `json:"date"`
	Message string `json:"message" jsonschema:"the first line of the commit message"`
	Client  string `json:"client,omitempty" jsonschema:"the MCP client that made the change through speki"`
}

// HistoryOutput is one page of commits, newest first.
type HistoryOutput struct {
	Commits    []CommitSummary `json:"commits"`
	NextCursor string          `json:"next_cursor,omitempty" jsonschema:"pass as cursor to get older commits; absent on the last page"`
}

func (s *Server) history(ctx context.Context, g *access.Grant, in HistoryInput) (HistoryOutput, string, error) {
	h, err := s.kb.History(ctx, g, knowledge.HistoryRequest{
		Vault:  in.Vault,
		Path:   in.Path,
		Limit:  in.Limit,
		Cursor: in.Cursor,
	})
	if err != nil {
		return HistoryOutput{}, "", err
	}
	out := HistoryOutput{Commits: make([]CommitSummary, 0, len(h.Commits)), NextCursor: h.NextCursor}
	var b strings.Builder
	if len(h.Commits) == 0 {
		b.WriteString("No commits found.\n")
	}
	for _, c := range h.Commits {
		cs := CommitSummary{
			SHA:     c.SHA,
			Author:  c.Author,
			Date:    c.Date.UTC().Format(time.RFC3339),
			Message: c.Message,
			Client:  c.Client,
		}
		out.Commits = append(out.Commits, cs)
		via := ""
		if c.Client != "" {
			via = " via " + oneLine(c.Client)
		}
		fmt.Fprintf(&b, "%s %s %s%s: %s\n", cs.SHA, cs.Date, oneLine(cs.Author), via, oneLine(cs.Message))
	}
	if h.NextCursor != "" {
		fmt.Fprintf(&b, "More commits follow: call history again with cursor %q.\n", h.NextCursor)
	}
	return out, b.String(), nil
}
