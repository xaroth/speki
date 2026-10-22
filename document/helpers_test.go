package document_test

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/text/unicode/norm"

	"github.com/xaroth/speki/document"
)

func readFixture(tb testing.TB, name string) []byte {
	tb.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		tb.Fatal(err)
	}
	return raw
}

func mustParse(tb testing.TB, p string, raw []byte) *document.Document {
	tb.Helper()
	d, err := document.Parse(p, raw)
	if err != nil {
		tb.Fatalf("Parse(%s): %v", p, err)
	}
	return d
}

func nfd(s string) string { return norm.NFD.String(s) }
