package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/xaroth/speki/auth/access"
)

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
	guide := guideText
	if strings.TrimSpace(s.id.Title) != "" || s.id.description() != "" {
		// Which speki this is comes first, for agents connected to more than one.
		head := "# " + oneLine(s.id.title()) + "\n\n"
		if desc := s.id.description(); desc != "" {
			head += desc + "\n\n"
		}
		guide = head + guideText
	}
	out := GuideOutput{Guide: guide, Vaults: vaultSummaries(vaults, true)}

	var b strings.Builder
	b.WriteString(guide)
	b.WriteString("\n## Your vaults\n")
	if served(vaults) == 0 {
		b.WriteString("\nNone is ready yet. Try again in a minute.\n")
	}
	for i, v := range out.Vaults {
		fmt.Fprintf(&b, "\n### %s\n\n", oneLine(v.Vault))
		writeVaultDetails(&b, v)
		if len(v.Frontmatter) > 0 {
			fmt.Fprintf(&b, "Frontmatter keys you may also set: %s\n", oneLine(strings.Join(v.Frontmatter, ", ")))
		}
		if guidelines := vaults[i].Guidelines; guidelines != "" && v.State == "" {
			b.WriteString(fence("guidelines", guidelines))
		}
	}
	return out, b.String(), nil
}
