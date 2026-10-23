package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/xaroth/speki/auth/access"
	"github.com/xaroth/speki/knowledge"
)

// ListInput asks for vaults, or a folder.
type ListInput struct {
	Vault        string   `json:"vault,omitempty" jsonschema:"the vault to list, owner/repo. Leave out to list your vaults"`
	Repositories []string `json:"repositories,omitempty" jsonschema:"without a vault: list only the vaults covering any of these code repositories, as git remote URLs or host/owner/repo"`
	Path         string   `json:"path,omitempty" jsonschema:"the folder to list, relative to the vault root. Leave out for the root"`
	Recursive    bool     `json:"recursive,omitempty" jsonschema:"list documents in subfolders too, instead of one entry per subfolder"`
	Cursor       string   `json:"cursor,omitempty" jsonschema:"the cursor the previous call gave, to get the next page"`
}

// DocumentSummary is a document in a listing.
type DocumentSummary struct {
	Path        string `json:"path"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Status      string `json:"status"`
	Updated     string `json:"updated,omitempty"`
	Version     string `json:"version"`
}

// ListOutput is the user's vaults, or one page of a folder.
type ListOutput struct {
	Vaults     []VaultSummary    `json:"vaults,omitempty"`
	Folders    []string          `json:"folders,omitempty"`
	Documents  []DocumentSummary `json:"documents,omitempty"`
	NextCursor string            `json:"next_cursor,omitempty" jsonschema:"pass as cursor to get the next page; absent on the last page"`
	Uncovered  bool              `json:"uncovered,omitempty" jsonschema:"with repositories: true when no vault covers them, so every vault is listed instead"`
}

func (s *Server) list(ctx context.Context, g *access.Grant, in ListInput) (ListOutput, string, error) {
	if in.Vault != "" && len(in.Repositories) > 0 {
		return ListOutput{}, "", &knowledge.Error{Code: knowledge.CodeInvalid, Message: "repositories filters the list of vaults. Leave out vault"}
	}
	if in.Vault == "" {
		var b strings.Builder
		var vaults []knowledge.VaultInfo
		var uncovered bool
		if len(in.Repositories) > 0 {
			c, err := s.kb.Covering(ctx, g, in.Repositories)
			if err != nil {
				return ListOutput{}, "", err
			}
			vaults = c.Vaults
			if len(vaults) == 0 {
				uncovered = true
				fmt.Fprintf(&b, "None of your vaults covers %s; these are all of them.\n", strings.Join(c.Repositories, " or "))
			}
		}
		if len(in.Repositories) == 0 || uncovered {
			var err error
			if vaults, err = s.kb.Vaults(ctx, g); err != nil {
				return ListOutput{}, "", err
			}
			if served(vaults) == 0 {
				b.WriteString("No vault is ready yet. Try again in a minute.\n")
			}
		}
		out := ListOutput{Vaults: vaultSummaries(vaults, false), Uncovered: uncovered}
		for _, v := range out.Vaults {
			fmt.Fprintf(&b, "%s\n", oneLine(v.Vault))
			writeVaultDetails(&b, v)
			b.WriteString("\n")
		}
		return out, b.String(), nil
	}

	l, err := s.kb.List(ctx, g, knowledge.ListRequest{
		Vault:     in.Vault,
		Path:      in.Path,
		Recursive: in.Recursive,
		Cursor:    in.Cursor,
	})
	if err != nil {
		return ListOutput{}, "", err
	}
	out := ListOutput{Folders: l.Folders, NextCursor: l.NextCursor}
	var b strings.Builder
	for _, f := range l.Folders {
		fmt.Fprintf(&b, "%s\n", oneLine(f))
	}
	var docs strings.Builder
	for _, e := range l.Documents {
		out.Documents = append(out.Documents, DocumentSummary{
			Path:        e.Path,
			Title:       e.Title,
			Description: e.Description,
			Status:      string(e.Status),
			Updated:     e.Updated,
			Version:     e.Version,
		})
		fmt.Fprintf(&docs, "%s  %s\n", oneLine(e.Path), docSummary(e.Title, string(e.Status), e.Updated))
		if e.Description != "" {
			fmt.Fprintf(&docs, "  %s\n", oneLine(e.Description))
		}
	}
	if docs.Len() > 0 {
		b.WriteString(fence("results", docs.String()))
	}
	if len(l.Folders)+len(l.Documents) == 0 {
		b.WriteString("This vault has no documents yet.\n")
	}
	if l.NextCursor != "" {
		fmt.Fprintf(&b, "More entries follow: call list again with cursor %q.\n", l.NextCursor)
	}
	if l.Guidelines != "" {
		b.WriteString(fence("guidelines", l.Guidelines))
	}
	return out, b.String(), nil
}
