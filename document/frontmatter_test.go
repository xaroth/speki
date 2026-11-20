package document_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/xaroth/speki/document"
)

func TestParseFrontmatterEdges(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name     string
		raw      string
		title    string
		body     string
		warnings int
	}{
		{"empty block", "---\n---\nText\n", "x.md", "Text\n", 0},
		{"closed with dots", "---\ntitle: Dots\n...\nText\n", "Dots", "Text\n", 0},
		{"never closed", "---\ntitle: Open\nText\n", "x.md", "---\ntitle: Open\nText\n", 0},
		{"not on first line", "\n---\ntitle: Late\n---\n", "x.md", "\n---\ntitle: Late\n---\n", 0},
		{"closed on last line", "---\ntitle: End\n---", "End", "", 0},
		{"scalar top level", "---\njust text\n---\n", "x.md", "", 1},
		{"duplicate keys", "---\ntitle: a\ntitle: b\n---\n", "x.md", "", 1},
		{"null values", "---\ntitle:\ntags:\n---\n# Heading\n", "Heading", "# Heading\n", 0},
		{"title as number", "---\ntitle: 2026\n---\n", "2026", "", 0},
		{"title as list", "---\ntitle: [a]\n---\n", "x.md", "", 1},
		{"nested tag", "---\ntags: [a, [b], [c]]\n---\n", "x.md", "", 1},
		{"byte order mark without frontmatter", "\ufeff# Title\n", "Title", "# Title\n", 0},
		{"non-text property name", "---\n? [a]\n: 1\n---\n", "x.md", "", 1},
		{"long title", "---\ntitle: " + strings.Repeat("t", document.MaxTitle+1) + "\n---\n", strings.Repeat("t", document.MaxTitle+1), "", 1},
		{"long description", "---\ndescription: " + strings.Repeat("d", document.MaxDescription+1) + "\n---\n", "x.md", "", 1},
		{"nested non-text key", "---\nx: {1: a}\n---\n", "x.md", "", 1},
		{"opening line with a trailing space", "--- \ntitle: A\n---\nText\n", "x.md", "--- \ntitle: A\n---\nText\n", 0},
		{"closing line with a trailing space", "---\ntitle: A\n--- \n# Heading\n\nImportant text.\n\n---\n\nMore.\n", "x.md", "\nMore.\n", 1},
		{"second document of keys", "---\ntitle: A\n--- \nowner: ops\n---\n", "x.md", "", 1},
		{"only comments", "---\n# just a comment\n---\nText\n", "x.md", "Text\n", 0},
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
		{
			name: "OKF stable is active",
			raw:  "---\ntitle: A\nstatus: stable\n---\n",
			want: document.Meta{Title: "A", Status: document.StatusActive},
		},
		{
			name: "OKF generated.at is the last update",
			raw:  "---\ntitle: A\ngenerated: { by: human:a, at: 2026-06-20T22:53:05+02:00 }\ntimestamp: 2026-01-01T00:00:00Z\n---\n",
			want: document.Meta{
				Title:   "A",
				Status:  document.StatusActive,
				Updated: time.Date(2026, 6, 20, 20, 53, 5, 0, time.UTC),
				Extra: map[string]any{
					"generated": map[string]any{"by": "human:a", "at": time.Date(2026, 6, 20, 20, 53, 5, 0, time.UTC)},
					"timestamp": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
				},
			},
		},
		{
			name: "OKF v0.1 timestamp is the last update",
			raw:  "---\ntitle: A\ntimestamp: '2026-05-28T22:53:05+00:00'\n---\n",
			want: document.Meta{
				Title:   "A",
				Status:  document.StatusActive,
				Updated: time.Date(2026, 5, 28, 22, 53, 5, 0, time.UTC),
				Extra:   map[string]any{"timestamp": "2026-05-28T22:53:05+00:00"},
			},
		},
		{
			name: "a later updated wins over OKF dates",
			raw:  "---\ntitle: A\nupdated: 2026-10-09T10:30:00Z\ntimestamp: 2026-01-01T00:00:00Z\n---\n",
			want: document.Meta{
				Title:   "A",
				Status:  document.StatusActive,
				Updated: time.Date(2026, 10, 9, 10, 30, 0, 0, time.UTC),
				Extra:   map[string]any{"timestamp": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
			},
		},
		{
			name: "a later generated.at wins over updated",
			raw:  "---\ntitle: A\nupdated: 2026-01-01T00:00:00Z\ngenerated: {by: x, at: '2026-10-09T10:30:00Z'}\n---\n",
			want: document.Meta{
				Title:   "A",
				Status:  document.StatusActive,
				Updated: time.Date(2026, 10, 9, 10, 30, 0, 0, time.UTC),
				Extra:   map[string]any{"generated": map[string]any{"by": "x", "at": "2026-10-09T10:30:00Z"}},
			},
		},
		{
			name: "a generated.at that isn't a date falls back to timestamp",
			raw:  "---\ntitle: A\ngenerated: {by: x, at: soon}\ntimestamp: '2026-05-28T00:00:00Z'\n---\n",
			want: document.Meta{
				Title:   "A",
				Status:  document.StatusActive,
				Updated: time.Date(2026, 5, 28, 0, 0, 0, 0, time.UTC),
				Extra: map[string]any{
					"generated": map[string]any{"by": "x", "at": "soon"},
					"timestamp": "2026-05-28T00:00:00Z",
				},
			},
		},
		{
			name: "a generated that isn't a mapping is passed over",
			raw:  "---\ntitle: A\ngenerated: yesterday\n---\n",
			want: document.Meta{Title: "A", Status: document.StatusActive, Extra: map[string]any{"generated": "yesterday"}},
		},
		{
			name: "an OKF date that isn't a date is passed over",
			raw:  "---\ntitle: A\ntimestamp: soon\n---\n",
			want: document.Meta{Title: "A", Status: document.StatusActive, Extra: map[string]any{"timestamp": "soon"}},
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
			if loc := d.Meta.Updated.Location(); loc != time.UTC {
				t.Errorf("got updated in %v, want UTC", loc)
			}
			if len(d.Warnings) != 0 {
				t.Errorf("got warnings %q, want none", d.Warnings)
			}
		})
	}
}
