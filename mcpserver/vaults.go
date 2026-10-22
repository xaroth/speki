package mcpserver

import (
	"fmt"
	"strings"

	"github.com/xaroth/speki/auth/access"
	"github.com/xaroth/speki/knowledge"
)

// VaultSummary describes a vault the user can read.
type VaultSummary struct {
	Vault        string   `json:"vault" jsonschema:"the vault's name, owner/repo"`
	Title        string   `json:"title,omitempty"`
	Description  string   `json:"description,omitempty"`
	Frontmatter  []string `json:"frontmatter,omitempty" jsonschema:"frontmatter keys you may set on top of title, description, tags, aliases, status and related"`
	Repositories []string `json:"repositories,omitempty" jsonschema:"the code repositories the vault's knowledge is about, as host/owner/repo. A * in the name covers every repository of that owner whose name matches"`
	CanWrite     bool     `json:"can_write"`
	Documents    int      `json:"documents"`
	State        string   `json:"state,omitempty" jsonschema:"set when speki can't serve the vault right now, so it can't be searched or read: building (indexing it for the first time; try again shortly), failed (it couldn't be opened; tell the user) or gone (just removed or reopening; try again shortly)"`
}

func vaultSummaries(vaults []knowledge.VaultInfo, frontmatter bool) []VaultSummary {
	out := make([]VaultSummary, 0, len(vaults))
	for _, v := range vaults {
		s := VaultSummary{
			Vault:        v.Name,
			Title:        v.Title,
			Description:  v.Description,
			Repositories: v.Repositories,
			CanWrite:     v.CanWrite,
			Documents:    v.Documents,
			State:        string(v.State),
		}
		if frontmatter && v.State == "" { // an unserved vault's config may be stale, and it can't be written yet
			s.Frontmatter = v.Frontmatter
		}
		out = append(out, s)
	}
	return out
}

// served counts the vaults speki serves.
func served(vaults []knowledge.VaultInfo) int {
	n := 0
	for _, v := range vaults {
		if v.State == "" {
			n++
		}
	}
	return n
}

// SkippedVault is a vault you can read that a search or query didn't cover.
type SkippedVault struct {
	Vault string `json:"vault"`
	State string `json:"state" jsonschema:"building: speki is indexing it for the first time; try again shortly. failed: speki couldn't open it; tell the user. gone: just removed or reopening; try again shortly"`
}

func skippedVaults(skipped []knowledge.Skipped) []SkippedVault {
	var out []SkippedVault
	for _, s := range skipped {
		out = append(out, SkippedVault{Vault: s.Name, State: string(s.State)})
	}
	return out
}

// stateText says why speki doesn't serve a vault, and what to do about it.
func stateText(state string) string {
	switch access.State(state) {
	case access.StateFailed:
		return "failed to open; tell the user"
	case access.StateGone:
		return "just removed or reopening; try again shortly"
	}
	return "still building its index; try again shortly"
}

// writeSkipped writes which vaults a search or query didn't cover, so their silence isn't read as absence.
func writeSkipped(b *strings.Builder, what string, skipped []SkippedVault) {
	if len(skipped) == 0 {
		return
	}
	parts := make([]string, len(skipped))
	for i, s := range skipped {
		parts[i] = fmt.Sprintf("%s (%s)", oneLine(s.Vault), stateText(s.State))
	}
	fmt.Fprintf(
		b,
		"Not %s, so this result says nothing about them: %s.\n",
		what,
		strings.Join(parts, ", "),
	)
}

func writeVaultDetails(b *strings.Builder, v VaultSummary) {
	if v.Title != "" {
		fmt.Fprintf(b, "Title: %s\n", oneLine(v.Title))
	}
	if v.Description != "" {
		fmt.Fprintf(b, "Description: %s\n", oneLine(v.Description))
	}
	if len(v.Repositories) > 0 {
		fmt.Fprintf(b, "Repositories: %s\n", strings.Join(v.Repositories, ", "))
	}
	if v.State != "" {
		fmt.Fprintf(b, "Not available: %s\n", stateText(v.State))
		return
	}
	access := "read only"
	if v.CanWrite {
		access = "read and write"
	}
	fmt.Fprintf(b, "Access: %s. Documents: %d\n", access, v.Documents)
}
