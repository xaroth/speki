// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index

// In-package: these hold generations, inject SQL failures or read internal tables, none of which the
// exported API reaches.

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

var vault = map[string]string{
	"README.md":    "# About\n\nKnowledge about our fleet.\n",
	"cache.md":     "# Cache\n\nHard problems: cache invalidation and naming things.\n",
	"ops/fleet.md": "# Fleet\n\nSee [the readme](../README.md).\n",
}

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

// Broken documents stay out of the full-text tables, whether built or applied, so their file-name titles
// don't match or feed typo corrections; adding and removing them leaves those tables intact.
func TestBrokenDocumentsStayOutOfFullText(t *testing.T) {
	t.Parallel()
	bad := "# Zanzibar\n\nNot UTF-8 \xff\n"
	testCases := []struct {
		name  string
		index func(tb testing.TB) *Index
	}{
		{name: "built", index: func(tb testing.TB) *Index {
			files := map[string]string{"zanzibar.md": bad}
			for k, v := range vault {
				files[k] = v
			}
			return NewTestIndex(tb, files)
		}},
		{name: "applied", index: func(tb testing.TB) *Index {
			x := NewTestIndex(tb, vault)
			err := x.Apply(tb.Context(), "c2", []Change{{Path: "zanzibar.md", Version: Version(bad), Content: []byte(bad)}})
			if err != nil {
				tb.Fatalf("Apply: got error %v, want nil", err)
			}
			return x
		}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			x := testCase.index(t)
			g, err := x.acquire()
			if err != nil {
				t.Fatalf("acquire: got error %v, want nil", err)
			}
			for _, q := range []string{
				"SELECT count(*) FROM fts WHERE title MATCH 'zanzibar'",
				"SELECT count(*) FROM names WHERE names MATCH 'zanzi'",
				"SELECT count(*) FROM words_vocab WHERE term = 'zanzibar'",
			} {
				var n int
				err := g.reader.QueryRowContext(ctx, q).Scan(&n)
				if err != nil || n != 0 {
					t.Errorf("%s: got %d, %v; want 0, nil", q, n, err)
				}
			}
			v, err := loadVocabulary(ctx, g)
			if err != nil {
				t.Fatalf("loadVocabulary: got error %v, want nil", err)
			}
			if got := v.suggest("zanzibat", 2); len(got) != 0 {
				t.Errorf("suggestions: got %v, want none", got)
			}
			g.release()

			good := "# Zanzibar\n\nFixed.\n"
			err = x.Apply(ctx, "c3", []Change{{Path: "zanzibar.md", Version: Version(good), Content: []byte(good)}})
			if err != nil {
				t.Fatalf("Apply: got error %v, want nil", err)
			}
			res, err := Search(ctx, []Source{{Name: "kb", Index: x}}, "zanzibar", Options{})
			if err != nil || len(res.Hits) != 1 {
				t.Errorf("search for the fixed document: got %+v, %v; want zanzibar.md", res.Hits, err)
			}
			err = x.Apply(ctx, "c4", []Change{{Path: "zanzibar.md", Version: Version(bad), Content: []byte(bad)}})
			if err != nil {
				t.Fatalf("Apply: got error %v, want nil", err)
			}
			err = x.Apply(ctx, "c5", []Change{{Path: "zanzibar.md"}})
			if err != nil {
				t.Fatalf("Apply: got error %v, want nil", err)
			}
			g, err = x.acquire()
			if err != nil {
				t.Fatalf("acquire: got error %v, want nil", err)
			}
			defer g.release()
			for _, table := range []string{"fts", "names", "words"} {
				_, err := g.writer.ExecContext(ctx, "INSERT INTO "+table+" ("+table+") VALUES ('integrity-check')")
				if err != nil {
					t.Errorf("%s integrity check: got error %v, want nil", table, err)
				}
			}
		})
	}
}

// A build whose inserting fails keeps any other error next to it, so a broken git copy is noticed, but
// not the cancellation the failed insert caused.
func TestFillError(t *testing.T) {
	t.Parallel()
	insertErr := errors.New("insert a.md: disk full")
	readErr := errors.New("blob missing")
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	testCases := []struct {
		name      string
		ctx       context.Context
		insertErr error
		readErr   error
		want      []error
		wantText  string // when set, the whole message
	}{
		{name: "with a read error", ctx: t.Context(), insertErr: insertErr, readErr: fmt.Errorf("x: %w", readErr), want: []error{insertErr, readErr}},
		{name: "with the build's own cancellation", ctx: t.Context(), insertErr: insertErr, readErr: context.Canceled, wantText: insertErr.Error()},
		{name: "with the caller's cancellation", ctx: cancelled, insertErr: insertErr, readErr: context.Canceled, want: []error{context.Canceled}},
		{name: "with only a read error", ctx: t.Context(), readErr: readErr, want: []error{readErr}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			err := fillError(testCase.ctx, testCase.insertErr, testCase.readErr)
			for _, want := range testCase.want {
				if !errors.Is(err, want) {
					t.Errorf("got %v, want it to wrap %v", err, want)
				}
			}
			if testCase.wantText != "" && (err == nil || err.Error() != testCase.wantText) {
				t.Errorf("got %v, want %q", err, testCase.wantText)
			}
		})
	}
}
