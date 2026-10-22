// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index

// In-package: these call scrubPaths and fill a sandbox's query slots directly.

import (
	"errors"
	"testing"
	"time"
)

func TestScrubPaths(t *testing.T) {
	t.Parallel()
	relative := []Source{{Name: "acme/kb", Index: &Index{dir: "data/100/index"}}}
	testCases := []struct {
		name string
		msg  string
		want string
	}{
		{name: "absolute", msg: "unable to open database: /data/vaults/x/index/3.db", want: "unable to open database: <file>"},
		{name: "uri", msg: "cannot open file:/data/a.db?mode=ro", want: "cannot open <file>"},
		{name: "uri with host part", msg: "cannot open file:///data/a/b.db?mode=ro", want: "cannot open <file>"},
		{name: "in brackets", msg: "bad [/x/y/z.db]", want: "bad [<file>"}, // the bracket goes too
		{name: "relative index folder", msg: "cannot open data/100/index/index-1.db", want: "cannot open <file>"},
		{name: "relative index folder as uri", msg: "cannot open file:data/100/index/index-1.db?mode=ro", want: "cannot open <file>"},
		{name: "a table name", msg: "no such table: a/b/c", want: "no such table: a/b/c"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := scrubPaths(testCase.msg, relative); got != testCase.want {
				t.Errorf("got %q, want %q", got, testCase.want)
			}
		})
	}
}

// Past sandboxConns running queries, the next one waits briefly, then gives up as busy.
func TestQueryBusy(t *testing.T) {
	t.Parallel()
	sb, err := NewSandbox()
	if err != nil {
		t.Fatalf("NewSandbox: got error %v, want nil", err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	sb.wait = 10 * time.Millisecond
	for range sandboxConns { // as if that many queries were running
		sb.slots <- struct{}{}
	}
	_, err = sb.Query(t.Context(), nil, "SELECT 1")
	if !errors.Is(err, ErrQueryBusy) {
		t.Errorf("all slots taken: got %v, want %v", err, ErrQueryBusy)
	}
	<-sb.slots
	_, err = sb.Query(t.Context(), nil, "SELECT 1")
	if err != nil {
		t.Errorf("a slot free: got %v, want nil", err)
	}
}
