package knowledge_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/xaroth/speki/document"
	"github.com/xaroth/speki/knowledge"
	"github.com/xaroth/speki/library/index"
)

func TestRead(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 1000) + "\n"
	var outlined strings.Builder
	for range knowledge.MaxOutline {
		outlined.WriteString("# " + strings.Repeat("é", 2500) + "\n" + strings.Repeat("text ", 200) + "\n")
	}
	f := newFixture(t, withKB(map[string]string{
		"services/edge-proxy.md": edgeProxy,
		"lines.md":               strings.Repeat("a\n", 1500),
		"bytes.md":               strings.Repeat(long, 100),
		"huge.md":                "é" + strings.Repeat("y", knowledge.MaxReadBytes+10) + "\ntail\n",
		"crlf.md":                "# A\r\n\r\nb\r\n",
		"empty.md":               "",
		"headings.md":            strings.Repeat("# H\n", knowledge.MaxOutline+5),
		"straddle.md":            strings.Repeat("y", knowledge.MaxReadBytes-1000) + strings.Repeat("é", 1000) + "\n",
		"angles.md":              strings.Repeat(strings.Repeat("<", 99)+"\n", 200),
		"outlined.md":            outlined.String(),
	}))
	testCases := []struct {
		name  string
		req   knowledge.ReadRequest
		code  knowledge.Code
		check func(t *testing.T, p knowledge.Page)
	}{
		{
			name: "whole document",
			req:  knowledge.ReadRequest{Vault: "Ours/KB", Path: "services/edge-proxy.md"},
			check: func(t *testing.T, p knowledge.Page) {
				want := knowledge.Page{
					Vault: "ours/kb",
					Document: index.Document{
						Path:    "services/edge-proxy.md",
						Title:   "Edge proxy",
						Status:  document.StatusActive,
						Tags:    []string{"edge"},
						Bytes:   len(edgeProxy),
						Lines:   12, // the index counts the empty line after the last line ending
						Version: blobID(edgeProxy),
					},
					Content:    edgeProxy,
					FromLine:   1,
					LineCount:  11,
					TotalLines: 11,
				}
				if diff := cmp.Diff(want, p, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("page (-want +got):\n%s", diff)
				}
			},
		},
		{
			name: "one line, with the outline",
			req:  knowledge.ReadRequest{Vault: "ours/kb", Path: "/services/edge-proxy.md", FromLine: 9, LineCount: 1},
			check: func(t *testing.T, p knowledge.Page) {
				if p.Content != "## Deploying\n" || p.LineCount != 1 || p.TotalLines != 11 {
					t.Errorf("got %q, line count %d of %d, want line 9 of 11", p.Content, p.LineCount, p.TotalLines)
				}
				want := []document.Heading{{Level: 1, Text: "Edge proxy", Line: 5}, {Level: 2, Text: "Deploying", Line: 9}}
				if diff := cmp.Diff(want, p.Outline); diff != "" {
					t.Errorf("outline (-want +got):\n%s", diff)
				}
			},
		},
		{
			name: "line cap",
			req:  knowledge.ReadRequest{Vault: "ours/kb", Path: "lines.md"},
			check: func(t *testing.T, p knowledge.Page) {
				if p.LineCount != knowledge.MaxReadLines || p.TotalLines != 1500 {
					t.Errorf("got %d of %d lines, want %d of 1500", p.LineCount, p.TotalLines, knowledge.MaxReadLines)
				}
			},
		},
		{
			name: "byte cap",
			req:  knowledge.ReadRequest{Vault: "ours/kb", Path: "bytes.md"},
			check: func(t *testing.T, p knowledge.Page) {
				room := knowledge.MaxReadBytes - knowledge.MetadataSize(p.Document)
				if want := room / knowledge.TextSize(long); p.LineCount != want || len(p.Content) != want*len(long) {
					t.Errorf("got %d lines, %d bytes, want %d whole lines", p.LineCount, len(p.Content), want)
				}
			},
		},
		{
			name: "line longer than the cap",
			req:  knowledge.ReadRequest{Vault: "ours/kb", Path: "huge.md"},
			check: func(t *testing.T, p knowledge.Page) {
				if p.LineCount != 1 || len(p.Content) > knowledge.MaxReadBytes || !strings.HasPrefix(p.Content, "é") || len(p.Warnings) != 1 {
					t.Errorf("got %d lines, %d bytes, warnings %v, want the start of line 1 and a warning", p.LineCount, len(p.Content), p.Warnings)
				}
			},
		},
		{
			name: "cut before a character the cap would split",
			req:  knowledge.ReadRequest{Vault: "ours/kb", Path: "straddle.md"},
			check: func(t *testing.T, p knowledge.Page) {
				want := knowledge.MaxReadBytes - knowledge.MetadataSize(p.Document) - knowledge.WarningsSize(p.Warnings)
				if len(p.Content) < want-1 || len(p.Content) > want || !strings.HasSuffix(p.Content, "é") || !utf8.ValidString(p.Content) {
					t.Errorf("got %d bytes, valid UTF-8 %t, want %d or one less, ending in é", len(p.Content), utf8.ValidString(p.Content), want)
				}
			},
		},
		{
			// A < counts as the &lt; a fence may escape it to, JSON-encoded.
			name: "markup counts as escaped",
			req:  knowledge.ReadRequest{Vault: "ours/kb", Path: "angles.md", MaxBytes: 20_000},
			check: func(t *testing.T, p knowledge.Page) {
				room := 20_000 - knowledge.MetadataSize(p.Document) - knowledge.OutlineSize(p.Outline)
				if want := room / (99*len(`\u0026lt;`) + 2); p.LineCount != want {
					t.Errorf("got %d lines, want %d", p.LineCount, want)
				}
			},
		},
		{
			name: "CRLF",
			req:  knowledge.ReadRequest{Vault: "ours/kb", Path: "crlf.md", FromLine: 2},
			check: func(t *testing.T, p knowledge.Page) {
				if p.Content != "\r\nb\r\n" || p.TotalLines != 3 {
					t.Errorf("got %q of %d lines, want %q of 3", p.Content, p.TotalLines, "\r\nb\r\n")
				}
			},
		},
		{
			name: "empty document",
			req:  knowledge.ReadRequest{Vault: "ours/kb", Path: "empty.md"},
			check: func(t *testing.T, p knowledge.Page) {
				if p.Content != "" || p.TotalLines != 0 || p.LineCount != 0 {
					t.Errorf("got %q, %d of %d lines, want nothing", p.Content, p.LineCount, p.TotalLines)
				}
			},
		},
		{
			name: "outline cap",
			req:  knowledge.ReadRequest{Vault: "ours/kb", Path: "headings.md", LineCount: 1},
			check: func(t *testing.T, p knowledge.Page) {
				if len(p.Outline) != knowledge.MaxOutline || len(p.Warnings) != 1 {
					t.Errorf("got an outline of %d, warnings %v, want %d and a warning", len(p.Outline), p.Warnings, knowledge.MaxOutline)
				}
			},
		},
		{
			// Long headings can't make a read much bigger than its cap: they're cut, and the outline
			// counts against the cap.
			name: "long headings",
			req:  knowledge.ReadRequest{Vault: "ours/kb", Path: "outlined.md"},
			check: func(t *testing.T, p knowledge.Page) {
				for _, h := range p.Outline {
					if len(h.Text) > knowledge.MaxHeading || !utf8.ValidString(h.Text) || !strings.HasSuffix(h.Text, "…") {
						t.Fatalf("got a heading of %d bytes: %q, want it cut to %d with …", len(h.Text), h.Text, knowledge.MaxHeading)
					}
				}
				if size := knowledge.OutlineSize(p.Outline); size > knowledge.MaxOutlineLen || len(p.Content)+size > knowledge.MaxReadBytes {
					t.Errorf("got %d bytes of content and %d of outline, want at most %d in all", len(p.Content), size, knowledge.MaxReadBytes)
				}
				if len(p.Outline) == 0 || len(p.Outline) == knowledge.MaxOutline || len(p.Warnings) != 1 {
					t.Errorf("got an outline of %d, warnings %v, want a shortened outline and a warning", len(p.Outline), p.Warnings)
				}
			},
		},
		{name: "past the end", req: knowledge.ReadRequest{Vault: "ours/kb", Path: "services/edge-proxy.md", FromLine: 12}, code: knowledge.CodeInvalid},
		{name: "negative line count", req: knowledge.ReadRequest{Vault: "ours/kb", Path: "services/edge-proxy.md", LineCount: -1}, code: knowledge.CodeInvalid},
		{name: "no such document", req: knowledge.ReadRequest{Vault: "ours/kb", Path: "nope.md"}, code: knowledge.CodeNotFound},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			p, err := f.base.Read(t.Context(), f.grant, testCase.req)
			if testCase.code != "" {
				wantCode(t, err, testCase.code)
				return
			}
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			testCase.check(t, p)
		})
	}
}

// Metadata that fills a read's cap on its own leaves out the outline, and every page still shows
// content, so paging reaches the end.
func TestReadLargeMetadata(t *testing.T) {
	t.Parallel()
	var doc strings.Builder
	doc.WriteString("---\ntitle: Fat\nnote: " + strings.Repeat("z", 30_000) + "\n---\n")
	for i := range 300 {
		fmt.Fprintf(&doc, "# Heading %d\nbody\n", i)
	}
	f := newFixture(t, withKB(map[string]string{"fat.md": doc.String()}))
	const maxBytes = 20_000
	for from, pages := 1, 0; ; pages++ {
		if pages == 100 {
			t.Fatalf("still at line %d after %d pages", from, pages)
		}
		p, err := f.base.Read(t.Context(), f.grant, knowledge.ReadRequest{Vault: "ours/kb", Path: "fat.md", FromLine: from, MaxBytes: maxBytes})
		if err != nil {
			t.Fatalf("Read from %d: %v", from, err)
		}
		if knowledge.MetadataSize(p.Document) <= maxBytes {
			t.Fatalf("metadata takes %d bytes, want more than %d", knowledge.MetadataSize(p.Document), maxBytes)
		}
		if p.LineCount < 1 || p.Content == "" || len(p.Outline) != 0 {
			t.Fatalf("from %d: got %d lines, %d bytes, an outline of %d, want content and no outline", from, p.LineCount, len(p.Content), len(p.Outline))
		}
		from += p.LineCount
		if from > p.TotalLines {
			break
		}
	}
}

// A blob the copy lacks is read again once, as a sync may have replaced it between the lookups; when
// it's still missing, that's speki's problem, not the agent's.
func TestReadMissingBlob(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name     string
		failures int
		wantErr  bool
	}{
		{name: "found the second time", failures: 1},
		{name: "missing", failures: 2, wantErr: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.copy(ours.RepoID).failBlobs(testCase.failures)
			p, err := f.base.Read(t.Context(), f.grant, knowledge.ReadRequest{Vault: "ours/kb", Path: "README.md"})
			if !testCase.wantErr {
				if err != nil || p.Content != readme {
					t.Errorf("got %q, %v, want the document", p.Content, err)
				}
				return
			}
			if err == nil {
				t.Fatal("got no error, want one")
			}
			if _, ok := errors.AsType[*knowledge.Error](err); ok {
				t.Errorf("got %v, want a plain error", err)
			}
		})
	}
}
