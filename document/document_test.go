package document_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"golang.org/x/text/unicode/norm"

	"github.com/xaroth/speki/document"
)

func readFixture(tb testing.TB, name string) []byte {
	tb.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		tb.Fatal(err)
	}
	return raw
}

func mustParse(tb testing.TB, p string, raw []byte) *document.Document {
	tb.Helper()
	d, err := document.Parse(p, raw)
	if err != nil {
		tb.Fatalf("Parse(%s): %v", p, err)
	}
	return d
}

func date(tb testing.TB, s string) time.Time {
	tb.Helper()
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		tb.Fatal(err)
	}
	return t
}

func nfd(s string) string { return norm.NFD.String(s) }

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

func TestParseFrontmatterEdges(t *testing.T) {
	t.Parallel()
	testCases := []struct {
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
		{"long title", "---\ntitle: " + strings.Repeat("t", document.MaxTitle+1) + "\n---\n", strings.Repeat("t", document.MaxTitle+1), "", 1},
		{"long description", "---\ndescription: " + strings.Repeat("d", document.MaxDescription+1) + "\n---\n", "x", "", 1},
		{"nested non-text key", "---\nx: {1: a}\n---\n", "x", "", 1},
		{"opening line with a trailing space", "--- \ntitle: A\n---\nText\n", "x", "--- \ntitle: A\n---\nText\n", 0},
		{"closing line with a trailing space", "---\ntitle: A\n--- \n# Heading\n\nImportant text.\n\n---\n\nMore.\n", "x", "\nMore.\n", 1},
		{"second document of keys", "---\ntitle: A\n--- \nowner: ops\n---\n", "x", "", 1},
		{"only comments", "---\n# just a comment\n---\nText\n", "x", "Text\n", 0},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			d := mustParse(t, "x.md", []byte(testCase.raw))
			if d.Meta.Title != testCase.title {
				t.Errorf("got title %q, want %q", d.Meta.Title, testCase.title)
			}
			if d.Body != testCase.body {
				t.Errorf("got body %q, want %q", d.Body, testCase.body)
			}
			if len(d.Warnings) != testCase.warnings {
				t.Errorf("got warnings %q, want %d", d.Warnings, testCase.warnings)
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

func TestHeadings(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name string
		raw  []byte
		want []document.Heading
	}{
		{
			name: "atx, setext and inline markup",
			raw:  []byte("---\ntitle: T\n---\n\n# One\n\nText\n\n## Two *em* `code`\n\nSetext\n------\n\n```\n# not a heading\n```\n"),
			want: []document.Heading{
				{Level: 1, Text: "One", Line: 5},
				{Level: 2, Text: "Two em code", Line: 9},
				{Level: 2, Text: "Setext", Line: 11},
			},
		},
		{
			name: "empty headings are skipped",
			raw:  []byte("#\n\ntext\n\n##\n\n## Real\n"),
			want: []document.Heading{{Level: 2, Text: "Real", Line: 7}},
		},
		{
			name: "lines counted with CRLF",
			raw:  readFixture(t, "crlf.md"),
			want: []document.Heading{{Level: 1, Text: "Heading", Line: 6}},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			d := mustParse(t, "x.md", testCase.raw)
			if diff := cmp.Diff(testCase.want, d.Headings); diff != "" {
				t.Errorf("headings (-want +got):\n%s", diff)
			}
		})
	}
}

func TestExtra(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name string
		raw  string
		want map[string]any
	}{
		{
			name: "nested values",
			raw:  "---\nowner:\n  team: ops\n  people: [a, b]\n---\n",
			want: map[string]any{"owner": map[string]any{"team": "ops", "people": []any{"a", "b"}}},
		},
		{
			name: "non-finite numbers kept as text",
			raw:  "---\nweight: .nan\nx: -.inf\n---\n",
			want: map[string]any{"weight": "NaN", "x": "-Inf"},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			d := mustParse(t, "x.md", []byte(testCase.raw))
			if diff := cmp.Diff(testCase.want, d.Meta.Extra); diff != "" {
				t.Errorf("extra (-want +got):\n%s", diff)
			}
			if _, err := json.Marshal(d.Meta.Extra); err != nil {
				t.Errorf("got json.Marshal error %v, want none", err)
			}
		})
	}
}

func TestStatusAndDates(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name string
		raw  string
		want document.Meta
	}{
		{
			name: "status",
			raw:  "---\ntitle: A\nstatus: draft\n---\n",
			want: document.Meta{Title: "A", Status: document.StatusDraft},
		},
		{
			name: "created with an offset is stored in UTC",
			raw:  "---\ntitle: A\ncreated: 2026-10-09T16:30:00+02:00\n---\n",
			want: document.Meta{Title: "A", Status: document.StatusActive, Created: time.Date(2026, 10, 9, 14, 30, 0, 0, time.UTC)},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			d := mustParse(t, "a.md", []byte(testCase.raw))
			// cmp compares time.Time with Equal, so check the location separately.
			if diff := cmp.Diff(testCase.want, d.Meta, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("meta (-want +got):\n%s", diff)
			}
			if loc := d.Meta.Created.Location(); loc != time.UTC {
				t.Errorf("got created in %v, want UTC", loc)
			}
			if len(d.Warnings) != 0 {
				t.Errorf("got warnings %q, want none", d.Warnings)
			}
		})
	}
}

func TestLinks(t *testing.T) {
	t.Parallel()
	d := mustParse(t, "fleet/doctrine.md", readFixture(t, "obsidian.md"))

	want := []document.Link{
		{Target: "[[ships/ferox|Feroxes]]", Candidate: "ferox", Kind: document.LinkWiki, Text: "Feroxes"},
		{Target: "../ops/comms.md#voice", Candidate: "ops/comms.md", Kind: document.LinkMarkdown, Text: "comms"},
		{Target: "[[logi#Rules]]", Candidate: "logi", Kind: document.LinkWiki, Text: "logi"},
		{Target: "https://eve.example.com/wiki", Kind: document.LinkMarkdown, Text: "https://eve.example.com/wiki"},
		{Target: "[[fleet-roles]]", Candidate: "fleet-roles", Kind: document.LinkWiki},
		{Target: "../ops/comms.md", Candidate: "ops/comms.md", Kind: document.LinkRelated},
	}
	if diff := cmp.Diff(want, d.Links); diff != "" {
		t.Errorf("links (-want +got):\n%s", diff)
	}
}

func TestLinkCandidates(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name string
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
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			d := mustParse(t, "notes/doc.md", []byte("[x]("+testCase.name+")\n"))
			if len(d.Links) != 1 {
				t.Fatalf("got links %+v, want one", d.Links)
			}
			if got := d.Links[0].Candidate; got != testCase.want {
				t.Errorf("got candidate %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestWikiLinkParsing(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name string
		want []document.Link
	}{
		{"[[A Note]]", []document.Link{{Target: "[[A Note]]", Candidate: "a note", Kind: document.LinkWiki, Text: "A Note"}}},
		{"[[a.md|x]]", []document.Link{{Target: "[[a.md|x]]", Candidate: "a", Kind: document.LinkWiki, Text: "x"}}},
		{"`[[in code]]`", nil},
		{"[[]]", nil},
		{"[[open", nil},
		{"| a | [[page\\|label]] |\n|---|---|\n| 1 | 2 |", []document.Link{{Target: "[[page|label]]", Candidate: "page", Kind: document.LinkWiki, Text: "label"}}},
		{"![[diagram.png]]", []document.Link{{Target: "[[diagram.png]]", Candidate: "diagram.png", Kind: document.LinkWiki, Text: "diagram.png"}}},
		{"[see [[inner]]](x.md)", []document.Link{{Target: "x.md", Candidate: "x.md", Kind: document.LinkMarkdown, Text: "see inner"}}},
		{"[x](y.md) and [[z]]", []document.Link{
			{Target: "y.md", Candidate: "y.md", Kind: document.LinkMarkdown, Text: "x"},
			{Target: "[[z]]", Candidate: "z", Kind: document.LinkWiki, Text: "z"},
		}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			d := mustParse(t, "doc.md", []byte(testCase.name))
			if diff := cmp.Diff(testCase.want, d.Links, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("links (-want +got):\n%s", diff)
			}
		})
	}
}

// A file whose name git stores in NFD, as files made on macOS often are.
func TestNFDFileNames(t *testing.T) {
	t.Parallel()
	d := mustParse(t, nfd("menü/brötchen.md"), readFixture(t, "nfd.md"))
	if want := nfd("brötchen"); d.Meta.Title != want {
		t.Errorf("got title %q, want %q from the file name", d.Meta.Title, want)
	}
	if len(d.Links) != 1 || d.Links[0].Candidate != "menü/käse.md" { // candidates are in NFC
		t.Errorf("got links %+v, want one to %q", d.Links, "menü/käse.md")
	}
}

// Link candidates are in NFC, and wikilink names are folded the same way whatever the input's form.
func TestLinkCandidatesAreNormalised(t *testing.T) {
	t.Parallel()
	d := mustParse(t, "index.md", []byte("[a]("+nfd("café/menu.md")+") [[İstanbul]] [["+nfd("İstanbul")+"]]\n"))
	var got []string
	for _, l := range d.Links {
		got = append(got, l.Candidate)
	}
	want := []string{"café/menu.md", document.LinkName("İstanbul.md"), document.LinkName("İstanbul.md")}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("candidates (-want +got):\n%s", diff)
	}
	if got, want := document.LinkName(nfd("İstanbul")), document.LinkName("İstanbul"); got != want {
		t.Errorf("got LinkName of NFD %q, want %q as for NFC", got, want)
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
