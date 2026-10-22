// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index

// In-package: these hold a generation across a build and check unusable's errors directly.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/mattn/go-sqlite3"
)

func TestGenerations(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	x, err := Open(t.Context(), dir, Discard())
	if err != nil {
		t.Fatalf("Open: got error %v, want nil", err)
	}
	if got := x.Commit(); got != "" {
		t.Errorf("empty index commit: got %q, want empty", got)
	}

	err = x.Build(t.Context(), "c1", Changes(vault))
	if err != nil {
		t.Fatalf("Build: got error %v, want nil", err)
	}
	// A reader on the first build keeps working while a second build replaces it.
	old, err := x.acquire()
	if err != nil {
		t.Fatalf("acquire: got error %v, want nil", err)
	}
	err = x.Build(t.Context(), "c2", Changes(map[string]string{"only.md": "# Only\n"}))
	if err != nil {
		t.Fatalf("Build: got error %v, want nil", err)
	}
	var n int
	err = old.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM documents").Scan(&n)
	if err != nil || n != len(vault) {
		t.Errorf("old generation during swap: got %d documents, %v; want %d, nil", n, err, len(vault))
	}
	old.release()
	x.Close()

	files, _ := filepath.Glob(filepath.Join(dir, "index-*.db"))
	if len(files) != 1 {
		t.Errorf("generation files: got %v, want one", files)
	}

	// Garbage next to it is cleaned up, and the newest usable generation is reopened.
	err = os.WriteFile(filepath.Join(dir, "index-99.db"), []byte("not sqlite"), 0o644)
	if err != nil {
		t.Fatalf("WriteFile: got error %v, want nil", err)
	}
	x, err = Open(t.Context(), dir, Discard())
	if err != nil {
		t.Fatalf("reopen: got error %v, want nil", err)
	}
	t.Cleanup(x.Close)
	if got := x.Commit(); got != "c2" {
		t.Errorf("reopened commit: got %q, want c2", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "index-99.db")); !os.IsNotExist(err) {
		t.Errorf("unusable generation: got stat error %v, want not exist", err)
	}
	stats, err := x.Stats(t.Context())
	if err != nil || stats.Documents != 1 {
		t.Errorf("Stats: got %+v, %v; want 1 document", stats, err)
	}
}

// Only errors that mean a generation is unusable for good get it discarded; anything else, such as a
// cancelled context or too many open files, keeps the file for the next try.
func TestUnusableErrors(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "no meta row", err: sql.ErrNoRows, want: true},
		{name: "quick check", err: fmt.Errorf("%w: quick_check: row 3 missing", errUnusable), want: true},
		{name: "not a database", err: sqlite3.Error{Code: sqlite3.ErrNotADB}, want: true},
		{name: "corrupt", err: sqlite3.Error{Code: sqlite3.ErrCorrupt}, want: true},
		{name: "cancelled", err: context.Canceled, want: false},
		{name: "can't open", err: sqlite3.Error{Code: sqlite3.ErrCantOpen}, want: false},
		{name: "busy", err: sqlite3.Error{Code: sqlite3.ErrBusy}, want: false},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := errors.Is(unusable(testCase.err), errUnusable); got != testCase.want {
				t.Errorf("unusable(%v): got %v, want %v", testCase.err, got, testCase.want)
			}
		})
	}
}
