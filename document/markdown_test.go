package document_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/xaroth/speki/document"
)

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
