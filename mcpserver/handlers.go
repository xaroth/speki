package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/eve-online-tools/speki/access"
	"github.com/eve-online-tools/speki/knowledge"
)

// VaultSummary describes a vault the user can read.
type VaultSummary struct {
	Vault       string `json:"vault" jsonschema:"the vault's name, owner/repo"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Guidelines  string `json:"guidelines,omitempty" jsonschema:"how the vault's owners want documents written and organised"`
	CanWrite    bool   `json:"can_write"`
	Documents   int    `json:"documents"`
}

func vaultSummaries(vaults []knowledge.VaultInfo, guidelines bool) []VaultSummary {
	out := make([]VaultSummary, 0, len(vaults))
	for _, v := range vaults {
		s := VaultSummary{Vault: v.Name, Title: v.Title, Description: v.Description, CanWrite: v.CanWrite, Documents: v.Documents}
		if guidelines {
			s.Guidelines = v.Guidelines
		}
		out = append(out, s)
	}
	return out
}

// GuideInput takes nothing.
type GuideInput struct{}

// GuideOutput is how to use speki, and the user's vaults.
type GuideOutput struct {
	Guide  string         `json:"guide"`
	Vaults []VaultSummary `json:"vaults"`
}

func (s *Server) guide(ctx context.Context, g *access.Grant, _ GuideInput) (GuideOutput, string, error) {
	vaults, err := s.kb.Vaults(ctx, g)
	if err != nil {
		return GuideOutput{}, "", err
	}
	out := GuideOutput{Guide: guideText, Vaults: vaultSummaries(vaults, true)}

	var b strings.Builder
	b.WriteString(guideText)
	b.WriteString("\n## Your vaults\n")
	if len(vaults) == 0 {
		b.WriteString("\nNone is ready yet. Try again in a minute.\n")
	}
	for _, v := range out.Vaults {
		fmt.Fprintf(&b, "\n### %s\n\n", oneLine(v.Vault))
		writeVaultDetails(&b, v)
		if v.Guidelines != "" {
			b.WriteString(fence("guidelines", v.Guidelines))
		}
	}
	return out, b.String(), nil
}

func writeVaultDetails(b *strings.Builder, v VaultSummary) {
	if v.Title != "" {
		fmt.Fprintf(b, "Title: %s\n", oneLine(v.Title))
	}
	if v.Description != "" {
		fmt.Fprintf(b, "Description: %s\n", oneLine(v.Description))
	}
	access := "read only"
	if v.CanWrite {
		access = "read and write"
	}
	fmt.Fprintf(b, "Access: %s. Documents: %d\n", access, v.Documents)
}

// ListInput asks for vaults, or a folder.
type ListInput struct {
	Vault     string `json:"vault,omitempty" jsonschema:"the vault to list, owner/repo. Leave out to list your vaults"`
	Path      string `json:"path,omitempty" jsonschema:"the folder to list, relative to the vault root. Leave out for the root"`
	Recursive bool   `json:"recursive,omitempty" jsonschema:"list documents in subfolders too, instead of one entry per subfolder"`
	Cursor    string `json:"cursor,omitempty" jsonschema:"next_cursor from the previous call, to get the next page"`
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
	Guidelines string            `json:"guidelines,omitempty" jsonschema:"the vault's writing guidelines, when listing its root"`
}

func (s *Server) list(ctx context.Context, g *access.Grant, in ListInput) (ListOutput, string, error) {
	if in.Vault == "" {
		vaults, err := s.kb.Vaults(ctx, g)
		if err != nil {
			return ListOutput{}, "", err
		}
		out := ListOutput{Vaults: vaultSummaries(vaults, false)}
		var b strings.Builder
		if len(vaults) == 0 {
			b.WriteString("No vault is ready yet. Try again in a minute.\n")
		}
		for _, v := range out.Vaults {
			fmt.Fprintf(&b, "%s\n", oneLine(v.Vault))
			writeVaultDetails(&b, v)
			b.WriteString("\n")
		}
		return out, b.String(), nil
	}

	l, err := s.kb.List(ctx, g, knowledge.ListRequest{Vault: in.Vault, Path: in.Path, Recursive: in.Recursive, Cursor: in.Cursor})
	if err != nil {
		return ListOutput{}, "", err
	}
	out := ListOutput{Folders: l.Folders, NextCursor: l.NextCursor, Guidelines: l.Guidelines}
	var b strings.Builder
	for _, f := range l.Folders {
		fmt.Fprintf(&b, "%s\n", oneLine(f))
	}
	for _, e := range l.Documents {
		out.Documents = append(out.Documents, DocumentSummary{
			Path: e.Path, Title: e.Title, Description: e.Description, Status: e.Status, Updated: e.Updated, Version: e.Version,
		})
		fmt.Fprintf(&b, "%s  %s  version %s\n", oneLine(e.Path), oneLine(e.Title), e.Version)
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

// SearchInput is a full-text search.
type SearchInput struct {
	Query             string   `json:"query" jsonschema:"words to search for. \"quoted phrases\" must appear as written; -word excludes documents with that word"`
	Vaults            []string `json:"vaults,omitempty" jsonschema:"only these vaults (owner/repo). Leave out to search all your vaults"`
	Path              string   `json:"path,omitempty" jsonschema:"only documents under this folder"`
	Tags              []string `json:"tags,omitempty" jsonschema:"only documents that have all of these tags"`
	ExcludeDeprecated bool     `json:"exclude_deprecated,omitempty" jsonschema:"leave out deprecated documents. By default they're included but ranked lower"`
	Limit             int      `json:"limit,omitempty" jsonschema:"how many results, 1 to 50. Default 10"`
}

// SearchHit is one search result.
type SearchHit struct {
	Vault       string   `json:"vault"`
	Path        string   `json:"path"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Status      string   `json:"status"`
	Updated     string   `json:"updated,omitempty"`
	Version     string   `json:"version"`
	Snippet     string   `json:"snippet" jsonschema:"text around the best match, matched words in **bold**"`
	Score       float64  `json:"score" jsonschema:"relevance; higher is better"`
}

// SearchOutput is what a search found.
type SearchOutput struct {
	Results   []SearchHit       `json:"results"`
	Corrected map[string]string `json:"corrected,omitempty" jsonschema:"words found in none of your vaults, and what was searched for instead"`
	Relaxed   bool              `json:"relaxed" jsonschema:"true when no document matched every word, and these results match only some"`
}

func (s *Server) search(ctx context.Context, g *access.Grant, in SearchInput) (SearchOutput, string, error) {
	res, err := s.kb.Search(ctx, g, knowledge.SearchRequest{
		Query:             in.Query,
		Vaults:            in.Vaults,
		Path:              in.Path,
		Tags:              in.Tags,
		ExcludeDeprecated: in.ExcludeDeprecated,
		Limit:             in.Limit,
	})
	if err != nil {
		return SearchOutput{}, "", err
	}
	out := SearchOutput{Results: make([]SearchHit, 0, len(res.Hits)), Corrected: res.Corrected, Relaxed: res.Relaxed}
	var b strings.Builder
	for _, word := range slices.Sorted(maps.Keys(res.Corrected)) {
		fmt.Fprintf(&b, "No document has %q; searched for %q instead.\n", oneLine(word), oneLine(res.Corrected[word]))
	}
	if res.Relaxed {
		b.WriteString("No document matched every word; these match some of them.\n")
	}
	if len(res.Hits) == 0 {
		b.WriteString("No documents found. Try other words, or list a vault to browse it.\n")
	}
	for _, h := range res.Hits {
		out.Results = append(out.Results, SearchHit{
			Vault: h.Source, Path: h.Path, Title: h.Title, Description: h.Description, Tags: h.Tags,
			Status: h.Status, Updated: h.Updated, Version: h.Version, Snippet: h.Snippet, Score: h.Score,
		})
		status := ""
		if h.Status != "" && h.Status != "active" {
			status = " [" + oneLine(h.Status) + "]"
		}
		fmt.Fprintf(&b, "\n%s %s  %s%s  version %s\n%s\n",
			oneLine(h.Source), oneLine(h.Path), oneLine(h.Title), status, h.Version, oneLine(h.Snippet))
	}
	return out, b.String(), nil
}

// ReadInput asks for a document, or part of it.
type ReadInput struct {
	Vault     string `json:"vault" jsonschema:"the vault, owner/repo"`
	Path      string `json:"path" jsonschema:"the document's path in the vault, e.g. services/esi-proxy.md"`
	FromLine  int    `json:"from_line,omitempty" jsonschema:"first line to return, counting from 1 at the top of the file. Default 1"`
	LineCount int    `json:"line_count,omitempty" jsonschema:"how many lines to return. Default: as many as fit in 1,000 lines and 64 KiB"`
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

// Heading is an entry in a document's outline.
type Heading struct {
	Level int    `json:"level"`
	Text  string `json:"text"`
	Line  int    `json:"line"`
}

// ReadOutput is a document, or part of it.
type ReadOutput struct {
	Vault      string    `json:"vault"`
	Path       string    `json:"path"`
	Version    string    `json:"version"`
	Metadata   Metadata  `json:"metadata"`
	Content    string    `json:"content" jsonschema:"the raw file text, frontmatter included"`
	FromLine   int       `json:"from_line"`
	LineCount  int       `json:"line_count"`
	TotalLines int       `json:"total_lines"`
	Outline    []Heading `json:"outline,omitempty" jsonschema:"the document's headings, when content isn't the whole document"`
	Warnings   []string  `json:"warnings,omitempty" jsonschema:"problems with the frontmatter, from changes made outside speki"`
}

func (s *Server) read(ctx context.Context, g *access.Grant, in ReadInput) (ReadOutput, string, error) {
	p, err := s.kb.Read(ctx, g, knowledge.ReadRequest{Vault: in.Vault, Path: in.Path, FromLine: in.FromLine, LineCount: in.LineCount})
	if err != nil {
		return ReadOutput{}, "", err
	}
	d := p.Document
	out := ReadOutput{
		Vault:   p.Vault,
		Path:    d.Path,
		Version: d.Version,
		Metadata: Metadata{
			Title: d.Title, Description: d.Description, Tags: d.Tags, Aliases: d.Aliases, Status: d.Status,
			Related: d.Related, Created: d.Created, Updated: d.Updated, Extra: d.Extra,
		},
		Content:    p.Content,
		FromLine:   p.FromLine,
		LineCount:  p.LineCount,
		TotalLines: p.TotalLines,
		Warnings:   p.Warnings,
	}
	for _, h := range p.Outline {
		out.Outline = append(out.Outline, Heading{Level: h.Level, Text: h.Text, Line: h.Line})
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
	if len(out.Outline) > 0 {
		b.WriteString("outline:\n")
		for _, h := range out.Outline {
			fmt.Fprintf(&b, "  line %d: %s %s\n", h.Line, strings.Repeat("#", h.Level), oneLine(h.Text))
		}
	}
	b.WriteString(fence("document", p.Content))
	return out, b.String(), nil
}

// HistoryInput asks for the latest commits.
type HistoryInput struct {
	Vault string `json:"vault" jsonschema:"the vault, owner/repo"`
	Path  string `json:"path,omitempty" jsonschema:"only commits that changed this document. Leave out for the whole vault"`
	Limit int    `json:"limit,omitempty" jsonschema:"how many commits, 1 to 50. Default 10"`
}

// CommitSummary is one commit.
type CommitSummary struct {
	SHA     string `json:"sha"`
	Author  string `json:"author"`
	Date    string `json:"date"`
	Message string `json:"message" jsonschema:"the first line of the commit message"`
	Client  string `json:"client,omitempty" jsonschema:"the MCP client that made the change through speki"`
}

// HistoryOutput is the latest commits, newest first.
type HistoryOutput struct {
	Commits []CommitSummary `json:"commits"`
}

func (s *Server) history(ctx context.Context, g *access.Grant, in HistoryInput) (HistoryOutput, string, error) {
	commits, err := s.kb.History(ctx, g, knowledge.HistoryRequest{Vault: in.Vault, Path: in.Path, Limit: in.Limit})
	if err != nil {
		return HistoryOutput{}, "", err
	}
	out := HistoryOutput{Commits: make([]CommitSummary, 0, len(commits))}
	var b strings.Builder
	if len(commits) == 0 {
		b.WriteString("No commits found.\n")
	}
	for _, c := range commits {
		cs := CommitSummary{SHA: c.SHA, Author: c.Author, Date: c.Date.UTC().Format(time.RFC3339), Message: c.Message, Client: c.Client}
		out.Commits = append(out.Commits, cs)
		via := ""
		if c.Client != "" {
			via = " via " + oneLine(c.Client)
		}
		fmt.Fprintf(&b, "%s %s %s%s: %s\n", cs.SHA, cs.Date, oneLine(cs.Author), via, oneLine(cs.Message))
	}
	return out, b.String(), nil
}

// ChangeOutput is what a change did.
type ChangeOutput struct {
	Version      string   `json:"version,omitempty" jsonschema:"the document's new version; absent after delete"`
	Commit       string   `json:"commit"`
	IndexPending bool     `json:"index_pending" jsonschema:"true when the commit landed but search may miss it for a little while"`
	LinkedFrom   []string `json:"linked_from,omitempty" jsonschema:"documents that link to the old path, after a move or delete"`
}

func changeOutput(r knowledge.Result, what string) (ChangeOutput, string) {
	out := ChangeOutput{Version: r.Version, Commit: r.Commit, IndexPending: r.IndexPending, LinkedFrom: r.LinkedFrom}
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
	Path    string `json:"path" jsonschema:"the document's path in the vault, ending in .md, e.g. services/esi-proxy.md"`
	Content string `json:"content" jsonschema:"the whole document: markdown with YAML frontmatter"`
	Version string `json:"version,omitempty" jsonschema:"the version being replaced, from read. Leave out to create a new document"`
	Message string `json:"message,omitempty" jsonschema:"a one-line commit message. Default: Create <path> or Update <path>"`
}

func (s *Server) write(ctx context.Context, g *access.Grant, in WriteInput) (ChangeOutput, string, error) {
	r, err := s.kb.Write(ctx, g, knowledge.WriteRequest{Vault: in.Vault, Path: in.Path, Content: in.Content, Version: in.Version, Message: in.Message})
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
		Vault: in.Vault, Path: in.Path, OldText: in.OldText, NewText: in.NewText, ReplaceAll: in.ReplaceAll, Version: in.Version, Message: in.Message,
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
	r, err := s.kb.Move(ctx, g, knowledge.MoveRequest{Vault: in.Vault, From: in.From, To: in.To, Version: in.Version, Message: in.Message})
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
	r, err := s.kb.Delete(ctx, g, knowledge.DeleteRequest{Vault: in.Vault, Path: in.Path, Version: in.Version, Message: in.Message})
	if err != nil {
		return ChangeOutput{}, "", err
	}
	out, text := changeOutput(r, "Deleted")
	return out, text, nil
}

// SchemaInput takes nothing.
type SchemaInput struct{}

// SchemaOutput describes what query can read.
type SchemaOutput struct {
	Schema string `json:"schema"`
}

func (s *Server) schema(context.Context, *access.Grant, SchemaInput) (SchemaOutput, string, error) {
	return SchemaOutput{Schema: schemaText}, schemaText, nil
}

// QueryInput is one SQL query.
type QueryInput struct {
	SQL    string   `json:"sql" jsonschema:"one read-only SQLite SELECT statement; see schema"`
	Vaults []string `json:"vaults,omitempty" jsonschema:"the vaults to query (owner/repo), at most 10. Leave out for all your vaults, if that's 10 or fewer"`
}

// QueryOutput is a query's result.
type QueryOutput struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Truncated bool     `json:"truncated" jsonschema:"true when rows were cut at 500 rows or 1 MiB"`
}

func (s *Server) query(ctx context.Context, g *access.Grant, in QueryInput) (QueryOutput, string, error) {
	r, err := s.kb.Query(ctx, g, knowledge.QueryRequest{SQL: in.SQL, Vaults: in.Vaults})
	if err != nil {
		return QueryOutput{}, "", err
	}
	out := QueryOutput{Columns: r.Columns, Rows: r.Rows, Truncated: r.Truncated}
	if out.Columns == nil {
		out.Columns = []string{}
	}
	if out.Rows == nil {
		out.Rows = [][]any{}
	}
	var b strings.Builder
	cells := make([]string, len(out.Columns))
	for i, c := range out.Columns {
		cells[i] = oneLine(c)
	}
	b.WriteString(strings.Join(cells, "\t") + "\n")
	size, shown := 0, 0
	for _, row := range out.Rows {
		for i, v := range row {
			if v == nil {
				cells[i] = "NULL"
			} else {
				cells[i] = oneLine(fmt.Sprint(v))
			}
		}
		line := strings.Join(cells, "\t") + "\n"
		size += jsonSize(line)
		if size > maxQueryText { // the text goes out JSON-encoded too, beside the structured rows
			break
		}
		b.WriteString(line)
		shown++
	}
	fmt.Fprintf(&b, "%d rows", len(out.Rows))
	if shown < len(out.Rows) {
		fmt.Fprintf(&b, ", the first %d shown here; the structured result has them all", shown)
	}
	if out.Truncated {
		b.WriteString(", truncated: add a LIMIT or narrow the query to see the rest")
	}
	b.WriteString("\n")
	return out, b.String(), nil
}

// maxQueryText caps the text rendering of a query result, as it'll be encoded.
const maxQueryText = 1 << 20

// jsonSize is how many bytes s takes encoded as a JSON string.
func jsonSize(s string) int {
	raw, _ := json.Marshal(s) // a string always marshals
	return len(raw)
}
