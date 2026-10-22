package document_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/xaroth/speki/document"
)

func date(tb testing.TB, s string) time.Time {
	tb.Helper()
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		tb.Fatal(err)
	}
	return t
}

func TestParseFixtures(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name     string
		meta     document.Meta
		warnings []string
		bodyHas  string
	}{
		{
			name: "obsidian.md",
			meta: document.Meta{
				Title:       "Fleet doctrine",
				Description: "Which fittings we fly, and why.",
				Tags:        []string{"fleet", "doctrine"},
				Aliases:     []string{"doctrines"},
				Status:      document.StatusActive,
				Related:     []string{"[[fleet-roles]]", "../ops/comms.md"},
				Created:     date(t, "2026-10-01T00:00:00Z"),
				Updated:     date(t, "2026-10-09T10:30:00Z"),
				Extra:       map[string]any{"cssclasses": []any{"wide"}},
			},
			bodyHas: "# Fleet doctrine",
		},
		{
			name:    "crlf.md",
			meta:    document.Meta{Title: "Windows note", Tags: []string{"a"}, Status: document.StatusActive},
			bodyHas: "# Heading\r\n",
		},
		{
			name:    "bom.md",
			meta:    document.Meta{Title: "With BOM", Status: document.StatusActive},
			bodyHas: "Body",
		},
		{
			name:     "broken-yaml.md",
			meta:     document.Meta{Title: "Still searchable", Status: document.StatusActive},
			warnings: []string{"frontmatter: not valid YAML"},
			bodyHas:  "Body text.",
		},
		{
			name:     "anchors.md",
			meta:     document.Meta{Title: "Anchored", Status: document.StatusActive},
			warnings: []string{"frontmatter: YAML anchors and aliases aren't allowed"},
		},
		{
			name: "messy-fields.md",
			meta: document.Meta{
				Title:  "Messy",
				Tags:   []string{"fleet", "doctrine"},
				Status: document.StatusActive,
				Extra:  map[string]any{"owner": "ops"},
			},
			warnings: []string{
				"tags: a single value instead of a list",
				`status: want draft, active or deprecated, got "wip"`,
				`created: want a date and time, got "last tuesday"`,
			},
		},
		{
			name:    "no-frontmatter.md",
			meta:    document.Meta{Title: "Setext title", Status: document.StatusActive},
			bodyHas: "Just text",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			d := mustParse(t, testCase.name, readFixture(t, testCase.name))

			if diff := cmp.Diff(testCase.meta, d.Meta, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("meta (-want +got):\n%s", diff)
			}
			if len(d.Warnings) != len(testCase.warnings) {
				t.Fatalf("got warnings %q, want %d starting with %q", d.Warnings, len(testCase.warnings), testCase.warnings)
			}
			for i, w := range testCase.warnings {
				if !strings.HasPrefix(d.Warnings[i], w) {
					t.Errorf("got warning %d %q, want prefix %q", i, d.Warnings[i], w)
				}
			}
			if !strings.Contains(d.Body, testCase.bodyHas) {
				t.Errorf("got body %q, want it to contain %q", d.Body, testCase.bodyHas)
			}
			if strings.HasPrefix(d.Body, "---") {
				t.Errorf("got body %q, want it without the frontmatter", d.Body)
			}
		})
	}
}

func TestParseUnusable(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name string
		raw  []byte
		want string
	}{
		{"not UTF-8", []byte("title \xff\xfe"), "unusable document: not UTF-8"},
		{"huge frontmatter", []byte("---\nx: " + strings.Repeat("a", 64<<10) + "\n---\n"), "unusable document: frontmatter is larger than 65536 bytes"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := document.Parse("x.md", testCase.raw)
			if err == nil {
				t.Fatalf("got no error, want %q", testCase.want)
			}
			// Agents see this text in the broken error and the broken column; docs/functional/tools.md lists it.
			if err.Error() != testCase.want {
				t.Errorf("got error %q, want %q", err, testCase.want)
			}
		})
	}
}

// A file whose name git stores in NFD, as files made on macOS often are.
func TestNFDFileNames(t *testing.T) {
	t.Parallel()
	d := mustParse(t, nfd("menü/brötchen.md"), readFixture(t, "nfd.md"))
	if want := nfd("menü/brötchen.md"); d.Meta.Title != want {
		t.Errorf("got title %q, want %q from the path", d.Meta.Title, want)
	}
	if len(d.Links) != 1 || d.Links[0].Candidate != "menü/käse.md" { // candidates are in NFC
		t.Errorf("got links %+v, want one to %q", d.Links, "menü/käse.md")
	}
}

func BenchmarkParse(b *testing.B) {
	raw := readFixture(b, "obsidian.md")
	for b.Loop() {
		if _, err := document.Parse("fleet/doctrine.md", raw); err != nil {
			b.Fatal(err)
		}
	}
}
