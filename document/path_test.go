package document_test

import (
	"strings"
	"testing"

	"github.com/eve-online-tools/speki/document"
)

func TestCheckPath(t *testing.T) {
	t.Parallel()
	existing := map[string]string{} // folded -> actual
	for _, p := range []string{"Runbooks", "Runbooks/deploy.md", "ops", "ops/Fleet.md", "café.md", nfd("menü"), nfd("menü/Brötchen.md")} {
		existing[document.Fold(p)] = p
	}
	lookup := func(folded string) (string, bool) {
		p, ok := existing[folded]
		return p, ok
	}

	testCases := []struct {
		name    string // the path
		want    string // the returned path when valid
		wantErr string
	}{
		{name: "ops/fleet-roles.md", want: "ops/fleet-roles.md"},
		{name: "Runbooks/new.md", want: "Runbooks/new.md"},
		{name: "ops/Fleet.md", want: "ops/Fleet.md"}, // the same path: not a collision
		{name: "", wantErr: "empty"},
		{name: "/abs.md", wantErr: "relative"},
		{name: "notes.txt", wantErr: ".md"},
		{name: "a/../b.md", wantErr: "clean"},
		{name: "a/.hidden.md", wantErr: "dot"},
		{name: "a\\b.md", wantErr: "contain"},
		{name: "[x].md", wantErr: "contain"},
		{name: "tab\t.md", wantErr: "control"},
		{name: "bad\xff.md", wantErr: "UTF-8"},
		{name: "a\u202eb.md", wantErr: "invisible"},
		{name: strings.Repeat("a/", 10) + "x.md", want: strings.Repeat("a/", 10) + "x.md"},
		{name: strings.Repeat("a/", 11) + "x.md", wantErr: "folders"},
		{name: strings.Repeat("a", 252) + ".md", want: strings.Repeat("a", 252) + ".md"},
		{name: strings.Repeat("a", 253) + ".md", wantErr: "bytes"},
		{name: "notes/AUX.md", wantErr: "Windows"},
		{name: "com1/x.md", wantErr: "Windows"},
		{name: "console.md", want: "console.md"},
		{name: "folder./x.md", wantErr: "end in"},
		{name: "runbooks/new.md", wantErr: `"Runbooks"`},
		{name: "ops/fleet.md", wantErr: `"ops/Fleet.md"`},
		{name: "café.md", want: "café.md"},                   // same as the existing NFC path
		{name: "caf\u0065\u0301.md", want: "café.md"},        // the same as the existing NFC name, given in NFD
		{name: "CAFÉ.md", wantErr: "café.md"},                // differs in case only
		{name: "nai\u0308ve-x.md", want: "naïve-x.md"},       // a new name given in NFD is stored in NFC
		{name: "menü/new.md", want: nfd("menü") + "/new.md"}, // the folder git stores in NFD
		{name: nfd("menü/new.md"), want: nfd("menü") + "/new.md"},
		{name: "menü/Brötchen.md", wantErr: "already exists"}, // an existing NFD file
		{name: "Menü/new.md", wantErr: "letter case"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, err := document.CheckPath(testCase.name, lookup)
			if testCase.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
					t.Errorf("got %q, %v; want an error mentioning %q", got, err, testCase.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("got error %v, want none", err)
			}
			if got != testCase.want {
				t.Errorf("got %q, want %q", got, testCase.want)
			}
		})
	}
}
