// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index

// In-package: it calls boldMatches directly, to cut snippets where the tests choose.

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A snippet cut inside a match still closes the bold span, and stays within the limit.
func TestBoldMatches(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name string
		in   string
		want string // when short enough to compare
	}{
		{name: "cut inside a match", in: strings.Repeat("a", 490) + "\x01" + strings.Repeat("b", 30) + "\x02 c"},
		{name: "cut inside a character", in: "\x01" + strings.Repeat("é", 400) + "\x02"},
		{name: "short", in: "short \x01match\x02 here", want: "short **match** here"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got := boldMatches(testCase.in)
			if len(got) > maxDescription || strings.Count(got, "**")%2 != 0 || !utf8.ValidString(got) {
				t.Errorf("got %d bytes, %d markers, valid UTF-8 %v; want at most %d, even, valid",
					len(got), strings.Count(got, "**"), utf8.ValidString(got), maxDescription)
			}
			if testCase.want != "" && got != testCase.want {
				t.Errorf("got %q, want %q", got, testCase.want)
			}
		})
	}
}
