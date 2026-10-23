package mcpserver

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/xaroth/speki/auth/access"
	"github.com/xaroth/speki/document"
	"github.com/xaroth/speki/knowledge"
)

// SearchInput is a full-text search.
type SearchInput struct {
	Query             string   `json:"query" jsonschema:"words; \"quoted phrases\" must appear as written; -word excludes"`
	Vaults            []string `json:"vaults,omitempty" jsonschema:"only these vaults, owner/repo. Default: all"`
	Repositories      []string `json:"repositories,omitempty" jsonschema:"only vaults covering any of these code repositories: git remote URLs or host/owner/repo"`
	Path              string   `json:"path,omitempty" jsonschema:"only documents under this folder"`
	Tags              []string `json:"tags,omitempty" jsonschema:"only documents with all these tags"`
	ExcludeDeprecated bool     `json:"exclude_deprecated,omitempty" jsonschema:"leave out deprecated documents, which otherwise rank lower"`
	Limit             int      `json:"limit,omitempty" jsonschema:"1 to 50. Default 10"`
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
	Score       float64  `json:"score" jsonschema:"relevance from 0 to 1, higher is better; 1 is the best match in its vault"`
}

// SearchOutput is what a search found.
type SearchOutput struct {
	Results   []SearchHit       `json:"results"`
	Corrected map[string]string `json:"corrected,omitempty" jsonschema:"words found in none of your vaults, and what was searched for instead"`
	Relaxed   bool              `json:"relaxed" jsonschema:"true when no document matched every word, and these results match only some"`
	Skipped   []SkippedVault    `json:"skipped_vaults,omitempty" jsonschema:"vaults you can read that weren't searched because speki can't serve them right now. The results say nothing about what they hold"`
	Searched  []string          `json:"repository_vaults,omitempty" jsonschema:"with repositories: the vaults covering them that were searched"`
}

func (s *Server) search(ctx context.Context, g *access.Grant, in SearchInput) (SearchOutput, string, error) {
	res, err := s.kb.Search(ctx, g, knowledge.SearchRequest{
		Query:             in.Query,
		Vaults:            in.Vaults,
		Repositories:      in.Repositories,
		Path:              in.Path,
		Tags:              in.Tags,
		ExcludeDeprecated: in.ExcludeDeprecated,
		Limit:             in.Limit,
	})
	if err != nil {
		return SearchOutput{}, "", err
	}
	out := SearchOutput{
		Results:   make([]SearchHit, 0, len(res.Hits)),
		Corrected: res.Corrected,
		Relaxed:   res.Relaxed,
		Skipped:   skippedVaults(res.Skipped),
		Searched:  res.Searched,
	}
	var b strings.Builder
	writeSkipped(&b, "searched", out.Skipped)
	if len(res.Repositories) > 0 {
		repos := strings.Join(res.Repositories, " or ")
		if len(res.Searched) == 0 {
			if len(res.Skipped) == 0 {
				which := "you can read"
				if len(in.Vaults) > 0 {
					which = "you named"
				}
				fmt.Fprintf(&b, "None of the vaults %s covers %s, so nothing was searched. Leave out repositories to search them anyway.\n", which, repos)
			}
			return out, b.String(), nil
		}
		fmt.Fprintf(&b, "Searched the vaults covering %s: %s.\n", repos, oneLine(strings.Join(res.Searched, ", ")))
	}
	for _, word := range slices.Sorted(maps.Keys(res.Corrected)) {
		fmt.Fprintf(&b, "No document has %q; searched for %q instead.\n", oneLine(word), oneLine(res.Corrected[word]))
	}
	if res.Relaxed {
		b.WriteString("No document matched every word; these match some of them.\n")
	}
	if len(res.Hits) == 0 {
		b.WriteString("No documents found. Try other words, or list a vault to browse it. If you work it out, write it back.\n")
	}
	var hits strings.Builder
	for i, h := range res.Hits {
		out.Results = append(out.Results, SearchHit{
			Vault:       h.Source,
			Path:        h.Path,
			Title:       h.Title,
			Description: h.Description,
			Tags:        h.Tags,
			Status:      string(h.Status),
			Updated:     h.Updated,
			Version:     h.Version,
			Snippet:     h.Snippet,
			Score:       h.Score,
		})
		if i > 0 {
			hits.WriteString("\n")
		}
		fmt.Fprintf(&hits, "%s %s  %s\n", oneLine(h.Source), oneLine(h.Path), docSummary(h.Title, string(h.Status), h.Updated))
		if h.Description != "" && h.Description != h.Snippet { // the snippet falls back to the description
			fmt.Fprintf(&hits, "%s\n", oneLine(h.Description))
		}
		fmt.Fprintf(&hits, "%s\n", oneLine(h.Snippet))
	}
	if hits.Len() > 0 {
		b.WriteString(fence("results", hits.String()))
	}
	return out, b.String(), nil
}

// docSummary is a document's title, its status when not active, and the day it was last updated, which
// help an agent pick what to read.
func docSummary(title, status, updated string) string {
	s := oneLine(title)
	if status != "" && status != string(document.StatusActive) {
		s += " [" + oneLine(status) + "]"
	}
	if len(updated) >= len("2006-01-02") {
		s += "  updated " + oneLine(updated[:len("2006-01-02")])
	}
	return s
}
