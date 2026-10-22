// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index

import (
	"crypto/sha1"
	"encoding/hex"
	"io"
	"iter"
	"log/slog"
	"maps"
	"slices"
	"testing"
)

// Fixtures shared by the black-box tests and the in-package ones, which can't import index_test.

// Discard is a logger that drops everything.
func Discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Version is the version a file with content gets, as git would give it.
func Version(content string) string {
	sum := sha1.Sum([]byte(content))
	return hex.EncodeToString(sum[:])
}

// Changes yields files, by path, as a build reads them.
func Changes(files map[string]string) iter.Seq2[Change, error] {
	return func(yield func(Change, error) bool) {
		for _, p := range slices.Sorted(maps.Keys(files)) {
			if !yield(Change{Path: p, Version: Version(files[p]), Content: []byte(files[p])}, nil) {
				return
			}
		}
	}
}

// NewTestIndex builds an index of files at commit c1 and waits for its vocabulary, so typo
// corrections are deterministic.
func NewTestIndex(tb testing.TB, files map[string]string) *Index {
	tb.Helper()
	x, err := Open(tb.Context(), tb.TempDir(), Discard())
	if err != nil {
		tb.Fatalf("Open: %v", err)
	}
	tb.Cleanup(x.Close)
	err = x.Build(tb.Context(), "c1", Changes(files))
	if err != nil {
		tb.Fatalf("Build: %v", err)
	}
	x.background.Wait()
	return x
}

// GenerationFile is the file the index currently reads.
func GenerationFile(x *Index) string {
	g := x.current.Load()
	return generationPath(g.dir, g.n)
}

// Limits the black-box tests run into.
const (
	MaxQueryRows    = maxQueryRows
	MaxQueryBytes   = maxQueryBytes
	MaxQueryColumns = maxQueryColumns
	SandboxConns    = sandboxConns
	MaxTerms        = maxTerms
)
