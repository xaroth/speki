// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index

// In-package: it calls fillError directly.

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

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
