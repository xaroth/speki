package document_test

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/xaroth/speki/document"
)

var now = time.Date(2026, 10, 9, 14, 2, 11, 0, time.UTC)

const twoYAMLDocuments = "---\ntitle: A\n--- \n# Heading\n\nImportant text.\n\n---\n\nMore.\n"

func TestCheck(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		current string // empty: a new document
		next    string
		wantErr string // empty: no error
	}{
		{name: "valid new document", next: "---\ntitle: A\n---\nText\n"},
		{name: "new document with heading title", next: "# A\n"},
		{name: "new document without title", next: "Text only\n", wantErr: "title: missing"},
		{name: "new document with bad status", next: "---\ntitle: A\nstatus: wip\n---\n", wantErr: "status"},
		{
			name:    "existing problem is left alone",
			current: "---\ntitle: A\ntags: a, b\n---\nTypo\n",
			next:    "---\ntitle: A\ntags: a, b\n---\nFixed\n",
		},
		{
			name:    "new problem in existing document",
			current: "---\ntitle: A\ntags: a, b\n---\n",
			next:    "---\ntitle: A\ntags: a, b\nstatus: wip\n---\n",
			wantErr: "status",
		},
		{
			name:    "broken frontmatter must be fixed",
			current: "---\ntitle: [a\n---\nText\n",
			next:    "---\ntitle: [a\n---\nNew text\n",
			wantErr: "frontmatter: not valid YAML",
		},
		{
			name:    "block with two YAML documents must be fixed",
			current: twoYAMLDocuments,
			next:    twoYAMLDocuments,
			wantErr: "frontmatter",
		},
		{
			name:    "fixing broken frontmatter",
			current: "---\ntitle: [a\n---\nText\n",
			next:    "---\ntitle: a\n---\nText\n",
		},
		{name: "too many tags", next: "---\ntitle: A\ntags: [" + list("t", document.MaxTags+1) + "]\n---\n", wantErr: "tags: more than 20"},
		{name: "too many aliases", next: "---\ntitle: A\naliases: [" + list("a", document.MaxAliases+1) + "]\n---\n", wantErr: "aliases: more than 10"},
		{
			name:    "too many tags from outside speki",
			current: "---\ntitle: A\ntags: [" + list("t", document.MaxTags+1) + "]\n---\n",
			next:    "---\ntitle: B\ntags: [" + list("t", document.MaxTags+1) + "]\n---\n",
		},
		{name: "too large", next: "# A\n" + strings.Repeat("x", document.MaxSize), wantErr: "larger than"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var current *document.Document
			if testCase.current != "" {
				current = mustParse(t, "a.md", []byte(testCase.current))
			}
			next := mustParse(t, "a.md", []byte(testCase.next))

			err := document.Check(current, next)

			if testCase.wantErr == "" {
				if err != nil {
					t.Errorf("got error %v, want none", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Errorf("got error %v, want one mentioning %q", err, testCase.wantErr)
			}
		})
	}
}

func TestTouch(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		raw     string
		created time.Time
		want    string
	}{
		{
			name: "updates existing field, keeps order and comments",
			raw:  "---\n# about this note\ntitle: A # the title\nupdated: 2020-01-01T00:00:00Z\nowner: ops\n---\nBody\n",
			want: "---\n# about this note\ntitle: A # the title\nupdated: 2026-10-09T14:02:11Z\nowner: ops\n---\nBody\n",
		},
		{
			name:    "adds created and updated",
			raw:     "---\ntitle: A\n---\nBody\n",
			created: now,
			want:    "---\ntitle: A\ncreated: 2026-10-09T14:02:11Z\nupdated: 2026-10-09T14:02:11Z\n---\nBody\n",
		},
		{
			name: "keeps existing created",
			raw:  "---\ncreated: 2020-01-01T00:00:00Z\n---\nBody\n",
			want: "---\ncreated: 2020-01-01T00:00:00Z\nupdated: 2026-10-09T14:02:11Z\n---\nBody\n",
		},
		{
			name: "adds a block when there is none",
			raw:  "# A\n\nBody\n",
			want: "---\nupdated: 2026-10-09T14:02:11Z\n---\n# A\n\nBody\n",
		},
		{
			name: "keeps CRLF",
			raw:  "---\r\ntitle: A\r\n---\r\nBody\r\n",
			want: "---\r\ntitle: A\r\nupdated: 2026-10-09T14:02:11Z\r\n---\r\nBody\r\n",
		},
		{
			name: "keeps the dots closer and lists",
			raw:  "---\ntags:\n  - a\n  - b\n...\nBody\n",
			want: "---\ntags:\n  - a\n  - b\nupdated: 2026-10-09T14:02:11Z\n...\nBody\n",
		},
		{
			name: "keeps a byte order mark first",
			raw:  "\ufeff# A\n",
			want: "\ufeff---\nupdated: 2026-10-09T14:02:11Z\n---\n# A\n",
		},
		{
			name: "keeps comments above and below",
			raw:  "---\n# top comment\n\ntitle: A\n# trailing\n---\nBody\n",
			want: "---\n# top comment\n\ntitle: A\nupdated: 2026-10-09T14:02:11Z\n# trailing\n---\nBody\n",
		},
		{
			name: "keeps a comment-only block",
			raw:  "---\n# just a note\n---\nBody\n",
			want: "---\n# just a note\n\nupdated: 2026-10-09T14:02:11Z\n---\nBody\n",
		},
		{
			name:    "replaces an invalid created",
			raw:     "---\ncreated: wip\n---\nBody\n",
			created: now,
			want:    "---\ncreated: 2026-10-09T14:02:11Z\nupdated: 2026-10-09T14:02:11Z\n---\nBody\n",
		},
		{
			name: "CRLF with dots closer",
			raw:  "---\r\ntitle: A\r\n...\r\nBody\r\n",
			want: "---\r\ntitle: A\r\nupdated: 2026-10-09T14:02:11Z\r\n...\r\nBody\r\n",
		},
		{
			name: "CRLF with comments",
			raw:  "---\r\ntitle: A\r\n# a\r\n# b\r\n---\r\nBody\r\n",
			want: "---\r\ntitle: A\r\nupdated: 2026-10-09T14:02:11Z\r\n# a\r\n# b\r\n---\r\nBody\r\n",
		},
		{
			name: "CRLF comment-only block",
			raw:  "---\r\n# a\r\n# b\r\n---\r\nBody\r\n",
			want: "---\r\n# a\r\n# b\r\n\r\nupdated: 2026-10-09T14:02:11Z\r\n---\r\nBody\r\n",
		},
		{
			name: "CRLF comment in a list",
			raw:  "---\r\ntags:\r\n  - a\r\n  # about b\r\n  - b\r\n---\r\nBody\r\n",
			want: "---\r\ntags:\r\n  - a\r\n  # about b\r\n  - b\r\nupdated: 2026-10-09T14:02:11Z\r\n---\r\nBody\r\n",
		},
		{
			name:    "sets the given created over an existing one",
			raw:     "---\ncreated: 2020-01-01T00:00:00Z\n---\nBody\n",
			created: time.Date(2021, 5, 6, 7, 8, 9, 0, time.FixedZone("x", 3600)),
			want:    "---\ncreated: 2021-05-06T06:08:09Z\nupdated: 2026-10-09T14:02:11Z\n---\nBody\n",
		},
		{
			name: "block on the last line, ending in a lone CR",
			raw:  "---\r\ntitle: A\r\n---\r",
			want: "---\r\ntitle: A\r\nupdated: 2026-10-09T14:02:11Z\r\n---\r\n",
		},
		{
			name: "block on the last line",
			raw:  "---\ntitle: A\n---",
			want: "---\ntitle: A\nupdated: 2026-10-09T14:02:11Z\n---\n",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			raw := []byte(testCase.raw)
			d := mustParse(t, "a.md", raw)

			got, err := d.Touch(now, testCase.created)
			if err != nil {
				t.Fatalf("got error %v, want none", err)
			}
			if string(got) != testCase.want {
				t.Errorf("got\n%q\nwant\n%q", got, testCase.want)
			}
			if string(raw) != testCase.raw {
				t.Errorf("got input changed to %q, want it left alone", raw)
			}
		})
	}
}

func TestTouchErrors(t *testing.T) {
	t.Parallel()
	const head = "---\ntitle: A\n---\n"
	testCases := []struct {
		name     string
		raw      string
		wantIs   error  // nil: only the text is checked
		wantText string // a part of the error text
	}{
		{name: "broken frontmatter", raw: "---\ntitle: [a\n---\n", wantText: "frontmatter: not valid YAML"},
		{name: "block with two YAML documents", raw: twoYAMLDocuments, wantText: "frontmatter"},
		{name: "past MaxSize once dated", raw: head + strings.Repeat("x", document.MaxSize-len(head)), wantIs: document.ErrTooLarge},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			d := mustParse(t, "a.md", []byte(testCase.raw))
			out, err := d.Touch(now, now)
			if err == nil {
				t.Fatalf("got %q and no error, want an error", out)
			}
			if testCase.wantIs != nil && !errors.Is(err, testCase.wantIs) {
				t.Errorf("got error %v, want %v", err, testCase.wantIs)
			}
			if !strings.Contains(err.Error(), testCase.wantText) {
				t.Errorf("got error %q, want it to mention %q", err, testCase.wantText)
			}
		})
	}
}

func TestTouchKeepsMeaning(t *testing.T) {
	t.Parallel()
	d := mustParse(t, "obsidian.md", readFixture(t, "obsidian.md"))
	out, err := d.Touch(now, time.Time{})
	if err != nil {
		t.Fatalf("got error %v, want none", err)
	}
	again := mustParse(t, "obsidian.md", out)

	if again.Body != d.Body {
		t.Errorf("got body %q, want %q", again.Body, d.Body)
	}
	want := d.Meta
	want.Updated = now
	if diff := cmp.Diff(want, again.Meta); diff != "" {
		t.Errorf("meta after touch (-want +got):\n%s", diff)
	}
	if len(again.Warnings) != 0 {
		t.Errorf("got warnings %q, want none", again.Warnings)
	}
}

func FuzzParseTouch(f *testing.F) {
	for _, name := range []string{"obsidian.md", "crlf.md", "bom.md", "broken-yaml.md", "messy-fields.md", "no-frontmatter.md", "anchors.md", "nfd.md"} {
		f.Add(readFixture(f, name))
	}
	f.Add([]byte("---\n---\n"))
	f.Add([]byte(twoYAMLDocuments))
	f.Fuzz(func(t *testing.T, raw []byte) {
		d, err := document.Parse("fuzz.md", raw)
		if err != nil {
			return
		}
		once, err := d.Touch(now, now)
		if err != nil {
			return
		}
		d2, err := document.Parse("fuzz.md", once)
		if err != nil {
			t.Fatalf("touched document doesn't parse: %v\n%q", err, once)
		}
		twice, err := d2.Touch(now, now)
		if err != nil {
			t.Fatalf("second Touch: %v", err)
		}
		if !bytes.Equal(once, twice) {
			t.Fatalf("Touch isn't stable:\n%q\n%q", once, twice)
		}
		if d2.Body != d.Body {
			t.Fatalf("got body %q, want %q", d2.Body, d.Body)
		}
	})
}

// list returns n distinct items for a YAML flow list.
func list(prefix string, n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	return strings.Join(items, ", ")
}
