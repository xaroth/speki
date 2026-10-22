// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index

// In-package: it holds a generation to add a failing trigger to its writer.

import "testing"

// A failure that isn't the file's (here a failing insert) leaves the commit where it was, so the next
// sync tries again instead of losing the change.
func TestApplyFailureKeepsTheCommit(t *testing.T) {
	t.Parallel()
	x := NewTestIndex(t, vault)
	g, err := x.acquire()
	if err != nil {
		t.Fatalf("acquire: got error %v, want nil", err)
	}
	defer g.release()
	_, err = g.writer.ExecContext(t.Context(), `CREATE TRIGGER fail_headings BEFORE INSERT ON headings
		BEGIN SELECT RAISE(ABORT, 'disk full'); END`)
	if err != nil {
		t.Fatalf("create trigger: got error %v, want nil", err)
	}
	content := "# README\n\nNew text.\n"
	changes := []Change{
		{Path: "bad.md", Version: Version("z"), Content: []byte("not utf-8 \xff")},
		{Path: "README.md", Version: Version(content), Content: []byte(content)},
	}
	err = x.Apply(t.Context(), "c2", changes)
	if err == nil {
		t.Fatal("Apply with failing inserts: got nil error, want one")
	}
	if got := x.Commit(); got != "c1" {
		t.Errorf("commit: got %q, want c1", got)
	}

	_, err = g.writer.ExecContext(t.Context(), "DROP TRIGGER fail_headings")
	if err != nil {
		t.Fatalf("drop trigger: got error %v, want nil", err)
	}
	err = x.Apply(t.Context(), "c2", changes)
	if err != nil || x.Commit() != "c2" {
		t.Fatalf("retry: got %v, commit %q; want nil, c2", err, x.Commit())
	}
	if d, err := x.Document(t.Context(), "bad.md"); err != nil || d.Status != StatusBroken {
		t.Errorf("bad.md after the retry: got %q, %v; want %q", d.Status, err, StatusBroken)
	}
	if d, err := x.Document(t.Context(), "README.md"); err != nil || d.Version != Version(content) {
		t.Errorf("README.md after the retry: got version %q, %v; want %q", d.Version, err, Version(content))
	}
}
