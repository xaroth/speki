package document

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"
)

func readFixtureFS(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join("testdata", name))
}

func TestCheckPath(t *testing.T) {
	existing := map[string]string{} // folded -> actual
	for _, p := range []string{"Runbooks", "Runbooks/deploy.md", "ops", "ops/Fleet.md", "café.md", nfd("menü"), nfd("menü/Brötchen.md")} {
		existing[Fold(p)] = p
	}
	lookup := func(folded string) (string, bool) {
		p, ok := existing[folded]
		return p, ok
	}

	tests := []struct {
		path    string
		want    string // the returned path when valid
		wantErr string
	}{
		{path: "ops/fleet-roles.md", want: "ops/fleet-roles.md"},
		{path: "Runbooks/new.md", want: "Runbooks/new.md"},
		{path: "ops/Fleet.md", want: "ops/Fleet.md"}, // the same path: not a collision
		{path: "café-menu.md", want: "café-menu.md"},
		{path: "", wantErr: "empty"},
		{path: "/abs.md", wantErr: "relative"},
		{path: "notes.txt", wantErr: ".md"},
		{path: "speki.yaml", wantErr: ".md"},
		{path: "a/../b.md", wantErr: "clean"},
		{path: "./a.md", wantErr: "clean"},
		{path: "a//b.md", wantErr: "clean"},
		{path: ".github/workflows/x.md", wantErr: "dot"},
		{path: "a/.hidden.md", wantErr: "dot"},
		{path: "a\\b.md", wantErr: "contain"},
		{path: "what?.md", wantErr: "contain"},
		{path: "[x].md", wantErr: "contain"},
		{path: "tab\t.md", wantErr: "control"},
		{path: "bad\xff.md", wantErr: "UTF-8"},
		{path: strings.Repeat("a/", 11) + "x.md", wantErr: "folders"},
		{path: strings.Repeat("a", 256) + ".md", wantErr: "bytes"},
		{path: "runbooks/new.md", wantErr: `"Runbooks"`},
		{path: "OPS/x.md", wantErr: `"ops"`},
		{path: "ops/fleet.md", wantErr: `"ops/Fleet.md"`},
		{path: "café.md", want: "café.md"}, // same as the existing NFC path
		{path: "CAFÉ.md", wantErr: "café.md"},
		{path: "menü/new.md", want: nfd("menü") + "/new.md"},      // the folder git stores in NFD
		{path: nfd("menü/new.md"), want: nfd("menü") + "/new.md"}, // given in NFD too
		{path: "menü/Brötchen.md", want: nfd("menü/Brötchen.md")}, // an existing NFD file: git's bytes
		{path: "Menü/new.md", wantErr: "letter case"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got, err := CheckPath(tt.path, lookup)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("CheckPath(%q) = %q, %v; want error mentioning %q", tt.path, got, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckPath(%q): %v", tt.path, err)
			}
			if got != tt.want {
				t.Errorf("CheckPath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func nfd(s string) string { return norm.NFD.String(s) }
