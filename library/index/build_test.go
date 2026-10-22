// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/xaroth/speki/library/index"
)

func TestInterruptedBuildIsDiscarded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	x, err := index.Open(t.Context(), dir, index.Discard())
	if err != nil {
		t.Fatalf("Open: got error %v, want nil", err)
	}
	err = x.Build(t.Context(), "good", changes(fleet))
	if err != nil {
		t.Fatalf("Build: got error %v, want nil", err)
	}
	// A build that fails half way, as if speki were killed while the transaction was open.
	failing := func(yield func(index.Change, error) bool) {
		if !yield(index.Change{Path: "a.md", Version: "v", Content: []byte("# A\n")}, nil) {
			return
		}
		yield(index.Change{}, errors.New("killed"))
	}
	err = x.Build(t.Context(), "half", failing)
	if err == nil {
		t.Fatal("failing build: got nil error, want one")
	}
	if got := x.Commit(); got != "good" {
		t.Errorf("commit after failed build: got %q, want good", got)
	}
	x.Close()

	x, err = index.Open(t.Context(), dir, index.Discard())
	if err != nil {
		t.Fatalf("reopen: got error %v, want nil", err)
	}
	t.Cleanup(x.Close)
	if got := x.Commit(); got != "good" {
		t.Errorf("reopened commit: got %q, want good", got)
	}
}

// A build stops reading files once an insert fails: the build fails anyway.
func TestBuildStopsAtAFailedInsert(t *testing.T) {
	t.Parallel()
	x, err := index.Open(t.Context(), t.TempDir(), index.Discard())
	if err != nil {
		t.Fatalf("Open: got error %v, want nil", err)
	}
	t.Cleanup(x.Close)
	const total = 5000
	read := 0
	files := func(yield func(index.Change, error) bool) {
		for i := range total {
			read++
			path := fmt.Sprintf("d%d.md", i)
			if i == 1 {
				path = "d0.md" // the same path twice fails its insert
			}
			content := fmt.Sprintf("# D%d\n", i)
			if !yield(index.Change{Path: path, Version: version(content), Content: []byte(content)}, nil) {
				return
			}
		}
	}
	err = x.Build(t.Context(), "c1", files)
	if err == nil || !strings.Contains(err.Error(), "d0.md: insert document") {
		t.Fatalf("Build: got %v, want a d0.md insert error", err)
	}
	if read > total/2 {
		t.Errorf("files read after the insert failed: got %d, want at most %d", read, total/2)
	}
}
