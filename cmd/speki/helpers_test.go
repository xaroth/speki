package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFile writes content to a new file and returns its path.
func writeFile(tb testing.TB, content string) string {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "file")
	err := os.WriteFile(path, []byte(content), 0o600)
	if err != nil {
		tb.Fatal(err)
	}
	return path
}
