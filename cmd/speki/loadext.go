//go:build !sqlite_omit_load_extension

package main

// Without the tag, mattn/go-sqlite3 links in extension loading. The undefined name is the error message.
var _ = build_with_tags_sqlite_fts5_and_sqlite_omit_load_extension
