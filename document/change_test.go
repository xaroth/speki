package document

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 9, 14, 2, 11, 0, time.UTC)

func TestCheck(t *testing.T) {
	tests := []struct {
		name    string
		current string // empty: a new document
		next    string
		wantErr string // empty: no error
	}{
		{"valid new document", "", "---\ntitle: A\n---\nText\n", ""},
		{"new document with heading title", "", "# A\n", ""},
		{"new document without title", "", "Text only\n", "title: missing"},
		{"new document with bad status", "", "---\ntitle: A\nstatus: wip\n---\n", "status"},
		{"new document with tags as string", "", "---\ntitle: A\ntags: a, b\n---\n", "tags"},
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
			name:    "fixing broken frontmatter",
			current: "---\ntitle: [a\n---\nText\n",
			next:    "---\ntitle: a\n---\nText\n",
		},
		{"too large", "", "# A\n" + strings.Repeat("x", MaxSize), "larger than"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var current *Document
			if tt.current != "" {
				current = mustParse(t, "a.md", []byte(tt.current))
			}
			next := mustParse(t, "a.md", []byte(tt.next))

			err := Check(current, next)

			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("Check: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Check = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestTouch(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		created bool
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
			created: true,
			want:    "---\ntitle: A\ncreated: 2026-10-09T14:02:11Z\nupdated: 2026-10-09T14:02:11Z\n---\nBody\n",
		},
		{
			name:    "keeps existing created",
			raw:     "---\ncreated: 2020-01-01T00:00:00Z\n---\nBody\n",
			created: true,
			want:    "---\ncreated: 2020-01-01T00:00:00Z\nupdated: 2026-10-09T14:02:11Z\n---\nBody\n",
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
			created: true,
			want:    "---\ncreated: 2026-10-09T14:02:11Z\nupdated: 2026-10-09T14:02:11Z\n---\nBody\n",
		},
		{
			name: "CRLF with dots closer",
			raw:  "---\r\ntitle: A\r\n...\r\nBody\r\n",
			want: "---\r\ntitle: A\r\nupdated: 2026-10-09T14:02:11Z\r\n...\r\nBody\r\n",
		},
		{
			name: "block on the last line",
			raw:  "---\ntitle: A\n---",
			want: "---\ntitle: A\nupdated: 2026-10-09T14:02:11Z\n---\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := mustParse(t, "a.md", []byte(tt.raw))

			got, err := d.Touch(now, tt.created)
			if err != nil {
				t.Fatalf("Touch: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("Touch =\n%q\nwant\n%q", got, tt.want)
			}
			if !bytes.Equal(d.raw, []byte(tt.raw)) {
				t.Error("Touch changed the document")
			}
		})
	}
}

func TestTouchBrokenFrontmatter(t *testing.T) {
	d := mustParse(t, "a.md", []byte("---\ntitle: [a\n---\n"))
	_, err := d.Touch(now, false)
	if err == nil {
		t.Fatal("Touch succeeded on broken frontmatter")
	}
}

func TestTouchKeepsMeaning(t *testing.T) {
	d := mustParse(t, "obsidian.md", readFixture(t, "obsidian.md"))
	out, err := d.Touch(now, false)
	if err != nil {
		t.Fatalf("Touch: %v", err)
	}
	again := mustParse(t, "obsidian.md", out)

	if again.Body != d.Body {
		t.Error("body changed")
	}
	want := d.Meta
	want.Updated = now
	if again.Meta.Title != want.Title || !again.Meta.Updated.Equal(now) || !again.Meta.Created.Equal(want.Created) {
		t.Errorf("meta after touch = %+v", again.Meta)
	}
	if len(again.Warnings) != 0 {
		t.Errorf("warnings after touch: %q", again.Warnings)
	}
}

func FuzzParseTouch(f *testing.F) {
	for _, name := range []string{"obsidian.md", "crlf.md", "bom.md", "broken-yaml.md", "messy-fields.md", "no-frontmatter.md"} {
		raw, err := readFixtureFS(name)
		if err == nil {
			f.Add(raw)
		}
	}
	f.Add([]byte("---\n---\n"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		d, err := Parse("fuzz.md", raw)
		if err != nil {
			return
		}
		once, err := d.Touch(now, true)
		if err != nil {
			return
		}
		d2, err := Parse("fuzz.md", once)
		if err != nil {
			t.Fatalf("touched document doesn't parse: %v\n%q", err, once)
		}
		twice, err := d2.Touch(now, true)
		if err != nil {
			t.Fatalf("second Touch: %v", err)
		}
		if !bytes.Equal(once, twice) {
			t.Fatalf("Touch isn't stable:\n%q\n%q", once, twice)
		}
		if d2.Body != d.Body && !strings.HasPrefix(string(raw), "\ufeff") {
			t.Fatalf("body changed:\n%q\n%q", d.Body, d2.Body)
		}
	})
}
