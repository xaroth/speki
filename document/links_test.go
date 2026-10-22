package document_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/xaroth/speki/document"
)

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
