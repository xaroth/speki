package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/xaroth/speki/auth/access"
	"github.com/xaroth/speki/knowledge"
)

// ChangeOutput is what a change did.
type ChangeOutput struct {
	Version      string   `json:"version,omitempty" jsonschema:"the document's new version; absent after delete"`
	Commit       string   `json:"commit"`
	IndexPending bool     `json:"index_pending" jsonschema:"true when the commit landed but search may miss it for a little while"`
	LinkedFrom   []string `json:"linked_from,omitempty" jsonschema:"documents that link to the old path, after a move or delete"`
}

func changeOutput(r knowledge.Result, what string) (ChangeOutput, string) {
	out := ChangeOutput{
		Version:      r.Version,
		Commit:       r.Commit,
		IndexPending: r.IndexPending,
		LinkedFrom:   r.LinkedFrom,
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s in commit %s.\n", what, r.Commit)
	if r.Version != "" {
		fmt.Fprintf(&b, "New version: %s\n", r.Version)
	}
	if r.IndexPending {
		b.WriteString("Search may not show the change for a little while.\n")
	}
	if len(r.LinkedFrom) > 0 {
		b.WriteString("These documents link to the old path:\n")
		for _, p := range r.LinkedFrom {
			fmt.Fprintf(&b, "  %s\n", oneLine(p))
		}
	}
	return out, b.String()
}

// WriteInput creates or replaces a document.
type WriteInput struct {
	Vault   string `json:"vault" jsonschema:"the vault, owner/repo"`
	Path    string `json:"path" jsonschema:"the document's path in the vault, ending in .md, e.g. services/edge-proxy.md"`
	Content string `json:"content" jsonschema:"the whole document: markdown with YAML frontmatter"`
	Version string `json:"version,omitempty" jsonschema:"the version being replaced, from read. Leave out to create a new document"`
	Message string `json:"message,omitempty" jsonschema:"a one-line commit message. Default: Create <path> or Update <path>"`
}

func (s *Server) write(ctx context.Context, g *access.Grant, in WriteInput) (ChangeOutput, string, error) {
	r, err := s.kb.Write(ctx, g, knowledge.WriteRequest{
		Vault:   in.Vault,
		Path:    in.Path,
		Content: in.Content,
		Version: in.Version,
		Message: in.Message,
	})
	if err != nil {
		return ChangeOutput{}, "", err
	}
	out, text := changeOutput(r, "Saved")
	return out, text, nil
}

// EditInput replaces exact text in a document.
type EditInput struct {
	Vault      string `json:"vault" jsonschema:"the vault, owner/repo"`
	Path       string `json:"path" jsonschema:"the document's path in the vault"`
	OldText    string `json:"old_text" jsonschema:"the exact text to replace, as read; may include frontmatter. Whitespace and line endings count"`
	NewText    string `json:"new_text" jsonschema:"the replacement text"`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"replace every occurrence instead of requiring exactly one"`
	Version    string `json:"version,omitempty" jsonschema:"the version you read; the edit fails with conflict if the document changed since"`
	Message    string `json:"message,omitempty" jsonschema:"a one-line commit message. Default: Update <path>"`
}

func (s *Server) edit(ctx context.Context, g *access.Grant, in EditInput) (ChangeOutput, string, error) {
	r, err := s.kb.Edit(ctx, g, knowledge.EditRequest{
		Vault:      in.Vault,
		Path:       in.Path,
		OldText:    in.OldText,
		NewText:    in.NewText,
		ReplaceAll: in.ReplaceAll,
		Version:    in.Version,
		Message:    in.Message,
	})
	if err != nil {
		return ChangeOutput{}, "", err
	}
	out, text := changeOutput(r, "Saved")
	return out, text, nil
}

// MoveInput moves a document.
type MoveInput struct {
	Vault   string `json:"vault" jsonschema:"the vault, owner/repo"`
	From    string `json:"from" jsonschema:"the document's current path"`
	To      string `json:"to" jsonschema:"the new path, ending in .md"`
	Version string `json:"version" jsonschema:"the version of the document being moved, from read"`
	Message string `json:"message,omitempty" jsonschema:"a one-line commit message. Default: Move <from> to <to>"`
}

func (s *Server) move(ctx context.Context, g *access.Grant, in MoveInput) (ChangeOutput, string, error) {
	r, err := s.kb.Move(ctx, g, knowledge.MoveRequest{
		Vault:   in.Vault,
		From:    in.From,
		To:      in.To,
		Version: in.Version,
		Message: in.Message,
	})
	if err != nil {
		return ChangeOutput{}, "", err
	}
	out, text := changeOutput(r, "Moved")
	return out, text, nil
}

// DeleteInput deletes a document.
type DeleteInput struct {
	Vault   string `json:"vault" jsonschema:"the vault, owner/repo"`
	Path    string `json:"path" jsonschema:"the document's path in the vault"`
	Version string `json:"version" jsonschema:"the version being deleted, from read"`
	Message string `json:"message,omitempty" jsonschema:"a one-line commit message. Default: Delete <path>"`
}

func (s *Server) delete(ctx context.Context, g *access.Grant, in DeleteInput) (ChangeOutput, string, error) {
	r, err := s.kb.Delete(ctx, g, knowledge.DeleteRequest{
		Vault:   in.Vault,
		Path:    in.Path,
		Version: in.Version,
		Message: in.Message,
	})
	if err != nil {
		return ChangeOutput{}, "", err
	}
	out, text := changeOutput(r, "Deleted")
	return out, text, nil
}
