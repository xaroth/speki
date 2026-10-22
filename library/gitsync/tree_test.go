package gitsync_test

import (
	"testing"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/google/go-cmp/cmp"
	"golang.org/x/text/unicode/norm"

	"github.com/xaroth/speki/library/gitsync"
)

// BlobAt finds names go-git's own lookup rejects, and tells an NFC name from its NFD twin: git compares bytes.
func TestBlobAt(t *testing.T) {
	t.Parallel()
	nfc, nfd := norm.NFC.String("café.md"), norm.NFD.String("café.md")
	src := newSource(t)
	head := src.rawCommit(map[string]string{
		"README.md":    "# Vault\n",
		"ops/fleet.md": "# Fleet\n",
		"a\tb.md":      "# Tab\n",
		"d\tx/a.md":    "# In a tab folder\n",
		nfc:            "# NFC\n",
		nfd:            "# NFD\n",
	})
	m := openCopy(t, src)
	testCases := []struct {
		name string
		path string
		want string
	}{
		{name: "top-level file", path: "README.md", want: blobOf("# Vault\n")},
		{name: "file in a folder", path: "ops/fleet.md", want: blobOf("# Fleet\n")},
		{name: "tab in the name", path: "a\tb.md", want: blobOf("# Tab\n")},
		{name: "tab in the folder name", path: "d\tx/a.md", want: blobOf("# In a tab folder\n")},
		{name: "NFC name", path: nfc, want: blobOf("# NFC\n")},
		{name: "NFD name", path: nfd, want: blobOf("# NFD\n")},
		{name: "missing file", path: "missing.md"},
		{name: "a folder", path: "ops"},
		{name: "missing file in a folder", path: "ops/missing.md"},
		{name: "file as a folder", path: "README.md/x.md"},
		{name: "missing folder", path: "nope/fleet.md"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, err := m.BlobAt(head, testCase.path)
			if err != nil || got != testCase.want {
				t.Errorf("got BlobAt(%q) %q, %v; want %q", testCase.path, got, err, testCase.want)
			}
		})
	}
}

// A control character in a file or folder name skips that file, not the vault, in a diff too.
func TestControlCharacterInAName(t *testing.T) {
	t.Parallel()
	src := newSource(t)
	first := src.rawCommit(map[string]string{"a\tb.md": "# Tab\n", "d\tx/a.md": "# A\n", "ok.md": "# OK\n"})
	m := openCopy(t, src)
	files, err := m.Files(first)
	if diff := cmp.Diff([]gitsync.File{{Path: "ok.md", Blob: blobOf("# OK\n")}}, files); err != nil || diff != "" {
		t.Errorf("Files, error %v (-want +got):\n%s", err, diff)
	}

	second := src.rawCommit(map[string]string{"a\tb.md": "# Tab2\n", "d\tx/a.md": "# A2\n", "d\tx/b.md": "# B\n", "ok.md": "# OK2\n"})
	head, err := m.Fetch(t.Context(), nil)
	if err != nil || head != second {
		t.Fatalf("got Fetch %s, %v; want %s", head, err, second)
	}
	changes, err := m.Diff(first, second)
	if diff := cmp.Diff([]gitsync.File{{Path: "ok.md", Blob: blobOf("# OK2\n")}}, changes); err != nil || diff != "" {
		t.Errorf("Diff, error %v (-want +got):\n%s", err, diff)
	}
}

// A diff needs neither the old content of a deleted file nor the folders speki doesn't read, and skips a
// change of the executable bit alone.
func TestDiffNeedsOnlyWhatItReads(t *testing.T) {
	t.Parallel()
	src := newSource(t)
	first := src.commit(map[string]*string{"gone.md": new("# Gone\n"), ".obsidian/x.md": new("x"), "run.md": new("# Run\n")})
	src.makeExecutable("run.md")
	second := src.commit(map[string]*string{"gone.md": nil, ".obsidian/x.md": new("y")})
	if mode := src.entryOf(second, "run.md").Mode; mode != filemode.Executable {
		t.Fatalf("got run.md mode %v in the second commit, want %v", mode, filemode.Executable)
	}
	m, gitDir := openWorkingCopy(t, src)
	removeObject(t, gitDir, src.entryOf(first, "gone.md").Hash.String())
	removeObject(t, gitDir, src.entryOf(first, ".obsidian").Hash.String())
	removeObject(t, gitDir, src.entryOf(second, ".obsidian").Hash.String())

	changes, err := m.Diff(first, second)
	if diff := cmp.Diff([]gitsync.File{{Path: "gone.md"}}, changes); err != nil || diff != "" {
		t.Errorf("Diff, error %v (-want +got):\n%s", err, diff)
	}
}
