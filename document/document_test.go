package document

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := readFixtureFS(name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func mustParse(t *testing.T, p string, raw []byte) *Document {
	t.Helper()
	d, err := Parse(p, raw)
	if err != nil {
		t.Fatalf("Parse(%s): %v", p, err)
	}
	return d
}

func date(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestParseFixtures(t *testing.T) {
	tests := []struct {
		file     string
		meta     Meta
		warnings []string
		bodyHas  string
	}{
		{
			file: "obsidian.md",
			meta: Meta{
				Title:       "Fleet doctrine",
				Description: "Which fittings we fly, and why.",
				Tags:        []string{"fleet", "doctrine"},
				Aliases:     []string{"doctrines"},
				Status:      StatusActive,
				Related:     []string{"[[fleet-roles]]", "../ops/comms.md"},
				Created:     date("2026-10-01T00:00:00Z"),
				Updated:     date("2026-10-09T10:30:00Z"),
				Extra:       map[string]any{"cssclasses": []any{"wide"}},
			},
			bodyHas: "# Fleet doctrine",
		},
		{
			file:    "crlf.md",
			meta:    Meta{Title: "Windows note", Tags: []string{"a"}, Status: StatusActive},
			bodyHas: "# Heading\r\n",
		},
		{
			file:    "bom.md",
			meta:    Meta{Title: "With BOM", Status: StatusActive},
			bodyHas: "Body",
		},
		{
			file:     "broken-yaml.md",
			meta:     Meta{Title: "Still searchable", Status: StatusActive},
			warnings: []string{"frontmatter: not valid YAML"},
			bodyHas:  "Body text.",
		},
		{
			file:     "anchors.md",
			meta:     Meta{Title: "Anchored", Status: StatusActive},
			warnings: []string{"frontmatter: YAML anchors and aliases aren't allowed"},
		},
		{
			file: "messy-fields.md",
			meta: Meta{
				Title:  "Messy",
				Tags:   []string{"fleet", "doctrine"},
				Status: StatusActive,
				Extra:  map[string]any{"owner": "ops"},
			},
			warnings: []string{
				"tags: a single value instead of a list",
				`status: want draft, active or deprecated, got "wip"`,
				`created: want a date and time, got "last tuesday"`,
			},
		},
		{
			file:    "no-frontmatter.md",
			meta:    Meta{Title: "Setext title", Status: StatusActive},
			bodyHas: "Just text",
		},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			d := mustParse(t, tt.file, readFixture(t, tt.file))

			if diff := cmp.Diff(tt.meta, d.Meta, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("meta (-want +got):\n%s", diff)
			}
			if len(d.Warnings) != len(tt.warnings) {
				t.Fatalf("warnings = %q, want %d starting with %q", d.Warnings, len(tt.warnings), tt.warnings)
			}
			for i, w := range tt.warnings {
				if !strings.HasPrefix(d.Warnings[i], w) {
					t.Errorf("warning %d = %q, want prefix %q", i, d.Warnings[i], w)
				}
			}
			if !strings.Contains(d.Body, tt.bodyHas) {
				t.Errorf("body %q doesn't contain %q", d.Body, tt.bodyHas)
			}
			if strings.HasPrefix(d.Body, "---") {
				t.Errorf("body still has the frontmatter: %q", d.Body)
			}
		})
	}
}

func TestParseFrontmatterEdges(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		title    string
		body     string
		warnings int
	}{
		{"empty block", "---\n---\nText\n", "x", "Text\n", 0},
		{"closed with dots", "---\ntitle: Dots\n...\nText\n", "Dots", "Text\n", 0},
		{"never closed", "---\ntitle: Open\nText\n", "x", "---\ntitle: Open\nText\n", 0},
		{"not on first line", "\n---\ntitle: Late\n---\n", "x", "\n---\ntitle: Late\n---\n", 0},
		{"closed on last line", "---\ntitle: End\n---", "End", "", 0},
		{"scalar top level", "---\njust text\n---\n", "x", "", 1},
		{"duplicate keys", "---\ntitle: a\ntitle: b\n---\n", "x", "", 1},
		{"null values", "---\ntitle:\ntags:\n---\n# Heading\n", "Heading", "# Heading\n", 0},
		{"title as number", "---\ntitle: 2026\n---\n", "2026", "", 0},
		{"title as list", "---\ntitle: [a]\n---\n", "x", "", 1},
		{"nested tag", "---\ntags: [a, [b], [c]]\n---\n", "x", "", 1},
		{"byte order mark without frontmatter", "\ufeff# Title\n", "Title", "# Title\n", 0},
		{"non-text property name", "---\n? [a]\n: 1\n---\n", "x", "", 1},
		{"nested non-text key", "---\nx: {1: a}\n---\n", "x", "", 1},
		{"closing line with a trailing space", "---\ntitle: A\n--- \n# Heading\n\nImportant text.\n\n---\n\nMore.\n", "x", "\nMore.\n", 1},
		{"second document of keys", "---\ntitle: A\n--- \nowner: ops\n---\n", "x", "", 1},
		{"only comments", "---\n# just a comment\n---\nText\n", "x", "Text\n", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := mustParse(t, "x.md", []byte(tt.raw))
			if d.Meta.Title != tt.title {
				t.Errorf("title = %q, want %q", d.Meta.Title, tt.title)
			}
			if d.Body != tt.body {
				t.Errorf("body = %q, want %q", d.Body, tt.body)
			}
			if len(d.Warnings) != tt.warnings {
				t.Errorf("warnings = %q, want %d", d.Warnings, tt.warnings)
			}
		})
	}
}

func TestParseUnusable(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{"not UTF-8", []byte("title \xff\xfe")},
		{"huge frontmatter", []byte("---\nx: " + strings.Repeat("a", MaxFrontmatterSize) + "\n---\n")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse("x.md", tt.raw)
			if !errors.Is(err, ErrUnusable) {
				t.Errorf("err = %v, want ErrUnusable", err)
			}
		})
	}
}

func TestHeadings(t *testing.T) {
	raw := "---\ntitle: T\n---\n\n# One\n\nText\n\n## Two *em* `code`\n\nSetext\n------\n\n```\n# not a heading\n```\n"
	d := mustParse(t, "x.md", []byte(raw))

	want := []Heading{
		{Level: 1, Text: "One", Line: 5},
		{Level: 2, Text: "Two em code", Line: 9},
		{Level: 2, Text: "Setext", Line: 11},
	}
	if diff := cmp.Diff(want, d.Headings); diff != "" {
		t.Errorf("headings (-want +got):\n%s", diff)
	}
}

func TestEmptyHeadingsAreSkipped(t *testing.T) {
	d := mustParse(t, "x.md", []byte("#\n\ntext\n\n##\n\n## Real\n"))
	want := []Heading{{Level: 2, Text: "Real", Line: 7}}
	if diff := cmp.Diff(want, d.Headings); diff != "" {
		t.Errorf("headings (-want +got):\n%s", diff)
	}
}

func TestExtraIsJSONSafe(t *testing.T) {
	d := mustParse(t, "x.md", []byte("---\nowner:\n  team: ops\n  people: [a, b]\n---\n"))
	_, err := json.Marshal(d.Meta.Extra)
	if err != nil {
		t.Errorf("Extra doesn't marshal: %v", err)
	}
}

func TestHeadingLinesCRLF(t *testing.T) {
	d := mustParse(t, "crlf.md", readFixture(t, "crlf.md"))
	if len(d.Headings) != 1 || d.Headings[0].Line != 6 {
		t.Errorf("headings = %+v, want one on line 6", d.Headings)
	}
}

func TestLinks(t *testing.T) {
	d := mustParse(t, "fleet/doctrine.md", readFixture(t, "obsidian.md"))

	want := []Link{
		{Target: "[[ships/ferox|Feroxes]]", Candidate: "ferox", Kind: LinkWiki, Text: "Feroxes"},
		{Target: "../ops/comms.md#voice", Candidate: "ops/comms.md", Kind: LinkMarkdown, Text: "comms"},
		{Target: "[[logi#Rules]]", Candidate: "logi", Kind: LinkWiki, Text: "logi"},
		{Target: "https://eve.example.com/wiki", Kind: LinkMarkdown, Text: "https://eve.example.com/wiki"},
		{Target: "[[fleet-roles]]", Candidate: "fleet-roles", Kind: LinkWiki},
		{Target: "../ops/comms.md", Candidate: "ops/comms.md", Kind: LinkRelated},
	}
	if diff := cmp.Diff(want, d.Links); diff != "" {
		t.Errorf("links (-want +got):\n%s", diff)
	}
}

func TestLinkCandidates(t *testing.T) {
	tests := []struct {
		dest string
		want string
	}{
		{"other.md", "notes/other.md"},
		{"./other.md", "notes/other.md"},
		{"../top.md", "top.md"},
		{"../../escape.md", ""},
		{"/root.md", "root.md"},
		{"sub/deep.md#part", "notes/sub/deep.md"},
		{"with%20space.md", "notes/with space.md"},
		{"https://example.com/x.md", ""},
		{"mailto:a@example.com", ""},
		{"//host/x.md", ""},
		{"#local", ""},
		{"image.png", "notes/image.png"},
	}
	for _, tt := range tests {
		t.Run(tt.dest, func(t *testing.T) {
			got := pathCandidate(tt.dest, "notes/doc.md")
			if got != tt.want {
				t.Errorf("pathCandidate(%q) = %q, want %q", tt.dest, got, tt.want)
			}
		})
	}
}

func TestWikiLinkParsing(t *testing.T) {
	tests := []struct {
		body string
		want []Link
	}{
		{"[[A Note]]", []Link{{Target: "[[A Note]]", Candidate: "a note", Kind: LinkWiki, Text: "A Note"}}},
		{"[[a.md|x]]", []Link{{Target: "[[a.md|x]]", Candidate: "a", Kind: LinkWiki, Text: "x"}}},
		{"`[[in code]]`", nil},
		{"[[]]", nil},
		{"[[open", nil},
		{"| a | [[page\\|label]] |\n|---|---|\n| 1 | 2 |", []Link{{Target: "[[page|label]]", Candidate: "page", Kind: LinkWiki, Text: "label"}}},
		{"![[diagram.png]]", []Link{{Target: "[[diagram.png]]", Candidate: "diagram.png", Kind: LinkWiki, Text: "diagram.png"}}},
		{"[see [[inner]]](x.md)", []Link{{Target: "x.md", Candidate: "x.md", Kind: LinkMarkdown, Text: "see inner"}}},
		{"[x](y.md) and [[z]]", []Link{
			{Target: "y.md", Candidate: "y.md", Kind: LinkMarkdown, Text: "x"},
			{Target: "[[z]]", Candidate: "z", Kind: LinkWiki, Text: "z"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.body, func(t *testing.T) {
			d := mustParse(t, "doc.md", []byte(tt.body))
			if diff := cmp.Diff(tt.want, d.Links, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("links (-want +got):\n%s", diff)
			}
		})
	}
}
