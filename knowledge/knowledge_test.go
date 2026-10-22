package knowledge_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/xaroth/speki/auth/access"
	"github.com/xaroth/speki/document"
	"github.com/xaroth/speki/gh"
	"github.com/xaroth/speki/knowledge"
	"github.com/xaroth/speki/library/index"
)

var sharedInfo = knowledge.VaultInfo{Name: "ours/shared", Repositories: []string{"github.com/ours/zeppelin"}, Documents: 1}

func TestVaults(t *testing.T) {
	t.Parallel()
	kb := knowledge.VaultInfo{Name: "ours/kb", Title: "KB", Description: "Our knowledge.", Guidelines: "One page per service.", Frontmatter: []string{"owner"}, Repositories: []string{"github.com/ours/api-*"}, CanWrite: true, Documents: 3}
	testCases := []struct {
		name  string
		setup func(testing.TB, *fixture)
		want  []knowledge.VaultInfo
	}{
		{
			name: "served",
			want: []knowledge.VaultInfo{kb, sharedInfo},
		},
		{
			name: "unavailable, and gone since the grant",
			setup: func(tb testing.TB, f *fixture) {
				withUnavailable(tb, f)
				f.setCopy(shared.RepoID, nil)
			},
			want: []knowledge.VaultInfo{
				kb,
				{Name: "ours/new", Title: "New", Repositories: []string{"github.com/ours/zeppelin"}, State: access.StateBuilding},
				{Name: "ours/old", State: access.StateFailed},
				{Name: "ours/shared", Repositories: sharedInfo.Repositories, State: access.StateGone},
			},
		},
		{
			name:  "index closed",
			setup: func(_ testing.TB, f *fixture) { f.copy(ours.RepoID).idx.Close() },
			want: []knowledge.VaultInfo{
				{Name: "ours/kb", Title: "KB", Description: "Our knowledge.", Guidelines: "One page per service.", Repositories: []string{"github.com/ours/api-*"}, State: access.StateGone},
				sharedInfo,
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			if testCase.setup != nil {
				testCase.setup(t, f)
			}
			got, err := f.base.Vaults(t.Context(), f.grant)
			if err != nil {
				t.Fatalf("Vaults: %v", err)
			}
			if diff := cmp.Diff(testCase.want, got); diff != "" {
				t.Errorf("vaults (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCovering(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	withUnavailable(t, f)
	testCases := []struct {
		name       string
		repository string
		want       knowledge.Covering
		code       knowledge.Code
	}{
		{
			name:       "pattern",
			repository: "git@github.com:Ours/api-gateway.git",
			want: knowledge.Covering{Repository: "github.com/ours/api-gateway", Vaults: []knowledge.VaultInfo{{
				Name: "ours/kb", Title: "KB", Description: "Our knowledge.", Guidelines: "One page per service.", Frontmatter: []string{"owner"},
				Repositories: []string{"github.com/ours/api-*"}, CanWrite: true, Documents: 3,
			}}},
		},
		{
			name:       "served and unavailable",
			repository: "https://github.com/ours/zeppelin",
			want: knowledge.Covering{Repository: "github.com/ours/zeppelin", Vaults: []knowledge.VaultInfo{
				{Name: "ours/new", Title: "New", Repositories: []string{"github.com/ours/zeppelin"}, State: access.StateBuilding},
				sharedInfo,
			}},
		},
		{name: "none", repository: "ours/api", want: knowledge.Covering{Repository: "github.com/ours/api"}},
		{name: "local path", repository: "/home/me/api", code: knowledge.CodeInvalid},
		{name: "pattern", repository: "ours/*", code: knowledge.CodeInvalid},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, err := f.base.Covering(t.Context(), f.grant, testCase.repository)
			if testCase.code != "" {
				wantCode(t, err, testCase.code)
				return
			}
			if err != nil {
				t.Fatalf("Covering: %v", err)
			}
			if diff := cmp.Diff(testCase.want, got); diff != "" {
				t.Errorf("covering (-want +got):\n%s", diff)
			}
		})
	}
}

// Every tool answers a vault the user may read but speki doesn't serve as unavailable, and one they
// can't read as not found, without asking GitHub.
func TestUnavailableVaultNamed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	withUnavailable(t, f)
	f.setCopy(ours.RepoID, nil) // in the grant, but not served any more: list shows it as gone
	tools := []struct {
		name string
		call func(t *testing.T, name string) error
	}{
		{name: "list", call: func(t *testing.T, name string) error {
			_, err := f.base.List(t.Context(), f.grant, knowledge.ListRequest{Vault: name})
			return err
		}},
		{name: "search", call: func(t *testing.T, name string) error {
			_, err := f.base.Search(t.Context(), f.grant, knowledge.SearchRequest{Query: "zeppelin", Vaults: []string{"ours/shared", name}})
			return err
		}},
		{name: "query", call: func(t *testing.T, name string) error {
			_, err := f.base.Query(t.Context(), f.grant, knowledge.QueryRequest{SQL: "SELECT 1", Vaults: []string{name}})
			return err
		}},
		{name: "read", call: func(t *testing.T, name string) error {
			_, err := f.base.Read(t.Context(), f.grant, knowledge.ReadRequest{Vault: name, Path: "zeppelin.md"})
			return err
		}},
		{name: "history", call: func(t *testing.T, name string) error {
			_, err := f.base.History(t.Context(), f.grant, knowledge.HistoryRequest{Vault: name})
			return err
		}},
		{name: "write", call: func(t *testing.T, name string) error {
			_, err := f.base.Write(t.Context(), f.grant, knowledge.WriteRequest{Vault: name, Path: "a.md", Content: "# A\n"})
			return err
		}},
	}
	testCases := []struct {
		name    string
		vault   string
		code    knowledge.Code
		message string
	}{
		{name: "building", vault: "OURS/new", code: knowledge.CodeUnavailable, message: "unavailable: ours/new is still building its index. Try again in a minute"},
		{name: "failed", vault: "ours/old", code: knowledge.CodeUnavailable, message: "unavailable: ours/old failed to open"},
		{name: "gone", vault: "OURS/kb", code: knowledge.CodeUnavailable, message: "unavailable: ours/kb was just removed or is reopening. Try again shortly"},
		{name: "unknown", vault: "ours/nothing", code: knowledge.CodeNotFound, message: `not_found: there's no vault named "ours/nothing"`},
	}
	for _, tool := range tools {
		for _, testCase := range testCases {
			t.Run(tool.name+" "+testCase.name, func(t *testing.T) {
				t.Parallel()
				err := tool.call(t, testCase.vault)
				wantCode(t, err, testCase.code)
				if !strings.HasPrefix(err.Error(), testCase.message) {
					t.Errorf("got error %q, want it to start with %q", err, testCase.message)
				}
				if n := len(f.github.sent()); n != 0 {
					t.Errorf("got %d commits, want none", n)
				}
			})
		}
	}
}

// A vault whose index closes under a request answers as gone, the way list shows it.
func TestClosedIndex(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.copy(ours.RepoID).idx.Close()
	testCases := []struct {
		name    string
		call    func(t *testing.T) error
		message string
	}{
		{name: "read", call: func(t *testing.T) error {
			_, err := f.base.Read(t.Context(), f.grant, knowledge.ReadRequest{Vault: "ours/kb", Path: "README.md"})
			return err
		}},
		{name: "list", call: func(t *testing.T) error {
			_, err := f.base.List(t.Context(), f.grant, knowledge.ListRequest{Vault: "ours/kb"})
			return err
		}},
		{name: "search", message: "just removed or is reopening", call: func(t *testing.T) error {
			_, err := f.base.Search(t.Context(), f.grant, knowledge.SearchRequest{Query: "zeppelin", Vaults: []string{"ours/kb"}})
			return err
		}},
		{name: "query", message: "just removed or is reopening", call: func(t *testing.T) error {
			_, err := f.base.Query(t.Context(), f.grant, knowledge.QueryRequest{SQL: "SELECT 1", Vaults: []string{"ours/kb"}})
			return err
		}},
		{name: "delete", call: func(t *testing.T) error {
			_, err := f.base.Delete(t.Context(), f.grant, knowledge.DeleteRequest{Vault: "ours/kb", Path: "README.md", Version: "x"})
			return err
		}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			err := testCase.call(t)
			wantCode(t, err, knowledge.CodeUnavailable)
			if !strings.Contains(err.Error(), testCase.message) {
				t.Errorf("got error %v, want it to contain %q", err, testCase.message)
			}
			if n := len(f.github.sent()); n != 0 {
				t.Errorf("got %d commits, want none", n)
			}
		})
	}
}

func TestList(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	testCases := []struct {
		name string
		req  knowledge.ListRequest
		want knowledge.Listing
		code knowledge.Code
	}{
		{
			name: "root",
			req:  knowledge.ListRequest{Vault: "ours/kb"},
			want: knowledge.Listing{
				Folders:    []string{"services/"},
				Documents:  []index.Entry{{Path: "README.md", Title: "About", Status: document.StatusActive, Version: blobID(readme)}},
				Guidelines: "One page per service.",
			},
		},
		{
			name: "folder, recursive",
			req:  knowledge.ListRequest{Vault: "ours/kb", Path: "/services", Recursive: true},
			want: knowledge.Listing{Documents: []index.Entry{
				{Path: "services/db/pg.md", Title: "Postgres", Status: document.StatusActive, Version: blobID(postgres)},
				{Path: "services/esi-proxy.md", Title: "ESI proxy", Status: document.StatusActive, Version: blobID(esiProxy)},
			}},
		},
		{name: "no such folder", req: knowledge.ListRequest{Vault: "ours/kb", Path: "nothing"}, code: knowledge.CodeNotFound},
		{name: "cursor of another listing", req: knowledge.ListRequest{Vault: "ours/kb", Path: "services", Cursor: "README.md"}, code: knowledge.CodeInvalid},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, err := f.base.List(t.Context(), f.grant, testCase.req)
			if testCase.code != "" {
				wantCode(t, err, testCase.code)
				return
			}
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if diff := cmp.Diff(testCase.want, got); diff != "" {
				t.Errorf("listing (-want +got):\n%s", diff)
			}
		})
	}
}

func TestListPages(t *testing.T) {
	t.Parallel()
	files := map[string]string{}
	for i := range knowledge.ListLimit + 5 {
		files[fmt.Sprintf("n/%03d.md", i)] = "# Note\n"
	}
	f := newFixture(t, withKB(files))

	var seen []string
	req := knowledge.ListRequest{Vault: "ours/kb", Path: "n"}
	for pages := 1; ; pages++ {
		page, err := f.base.List(t.Context(), f.grant, req)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, d := range page.Documents {
			seen = append(seen, d.Path)
		}
		if page.NextCursor == "" {
			break
		}
		if pages == 2 {
			t.Fatalf("got a cursor on page %d, want 2 pages", pages)
		}
		req.Cursor = page.NextCursor
	}
	want := slices.Sorted(func(yield func(string) bool) {
		for p := range files {
			if !yield(p) {
				return
			}
		}
	})
	if diff := cmp.Diff(want, seen); diff != "" {
		t.Errorf("paged through (-want +got):\n%s", diff)
	}
}

func TestSearch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	withUnavailable(t, f)
	notServed := []knowledge.Skipped{
		{Name: "ours/new", State: access.StateBuilding},
		{Name: "ours/old", State: access.StateFailed},
	}
	testCases := []struct {
		name        string
		req         knowledge.SearchRequest
		wantHits    []string // vault:path, sorted
		wantSkipped []knowledge.Skipped
		wantRepo    string
		wantSearch  []string // vaults searched for a repository
		code        knowledge.Code
	}{
		{
			name:        "every vault",
			req:         knowledge.SearchRequest{Query: "zeppelin"},
			wantHits:    []string{"ours/kb:README.md", "ours/kb:services/db/pg.md", "ours/kb:services/esi-proxy.md", "ours/shared:zeppelin.md"},
			wantSkipped: notServed,
		},
		{
			name:     "one vault, named twice in another letter case",
			req:      knowledge.SearchRequest{Query: "zeppelin", Vaults: []string{"ours/shared", "OURS/SHARED"}},
			wantHits: []string{"ours/shared:zeppelin.md"},
		},
		{
			name:     "path and tags",
			req:      knowledge.SearchRequest{Query: "zeppelin", Path: "services", Tags: []string{"esi"}},
			wantHits: []string{"ours/kb:services/esi-proxy.md"}, wantSkipped: notServed,
		},
		{
			name:        "repository",
			req:         knowledge.SearchRequest{Query: "zeppelin", Repository: "git@github.com:ours/zeppelin.git"},
			wantHits:    []string{"ours/shared:zeppelin.md"},
			wantSkipped: []knowledge.Skipped{{Name: "ours/new", State: access.StateBuilding}},
			wantRepo:    "github.com/ours/zeppelin",
			wantSearch:  []string{"ours/shared"},
		},
		{
			name:       "repository by pattern",
			req:        knowledge.SearchRequest{Query: "zeppelin", Repository: "ours/api-gateway"},
			wantHits:   []string{"ours/kb:README.md", "ours/kb:services/db/pg.md", "ours/kb:services/esi-proxy.md"},
			wantRepo:   "github.com/ours/api-gateway",
			wantSearch: []string{"ours/kb"},
		},
		{
			name:       "repository and a vault named in another letter case",
			req:        knowledge.SearchRequest{Query: "zeppelin", Vaults: []string{"OURS/Shared"}, Repository: "ours/zeppelin"},
			wantHits:   []string{"ours/shared:zeppelin.md"},
			wantRepo:   "github.com/ours/zeppelin",
			wantSearch: []string{"ours/shared"},
		},
		{
			name:     "repository no named vault covers",
			req:      knowledge.SearchRequest{Query: "zeppelin", Vaults: []string{"ours/kb"}, Repository: "ours/zeppelin"},
			wantRepo: "github.com/ours/zeppelin",
		},
		{name: "repository nobody covers", req: knowledge.SearchRequest{Query: "zeppelin", Repository: "ours/other"}, wantRepo: "github.com/ours/other"},
		{name: "repository invalid", req: knowledge.SearchRequest{Query: "zeppelin", Repository: "./api"}, code: knowledge.CodeInvalid},
		{name: "empty", req: knowledge.SearchRequest{Query: "  "}, code: knowledge.CodeInvalid},
		{name: "only exclusions", req: knowledge.SearchRequest{Query: "-zeppelin"}, code: knowledge.CodeInvalid},
		{name: "limit too high", req: knowledge.SearchRequest{Query: "x", Limit: 51}, code: knowledge.CodeInvalid},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, err := f.base.Search(t.Context(), f.grant, testCase.req)
			if testCase.code != "" {
				wantCode(t, err, testCase.code)
				return
			}
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			var hits []string
			for _, h := range got.Hits {
				hits = append(hits, h.Source+":"+h.Path)
			}
			slices.Sort(hits)
			if diff := cmp.Diff(testCase.wantHits, hits); diff != "" {
				t.Errorf("hits (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(testCase.wantSkipped, got.Skipped); diff != "" {
				t.Errorf("skipped (-want +got):\n%s", diff)
			}
			if got.Repository != testCase.wantRepo || !slices.Equal(got.Searched, testCase.wantSearch) {
				t.Errorf("got repository %q searched %v, want %q searched %v", got.Repository, got.Searched, testCase.wantRepo, testCase.wantSearch)
			}
		})
	}
}

// A served vault that goes away after the grant was made is skipped, not silently left out.
func TestSearchSkipsAVaultGoneSinceTheGrant(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setCopy(shared.RepoID, nil)
	got, err := f.base.Search(t.Context(), f.grant, knowledge.SearchRequest{Query: "zeppelin"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, h := range got.Hits {
		if h.Source != "ours/kb" {
			t.Errorf("got a hit from %s, want only ours/kb", h.Source)
		}
	}
	want := []knowledge.Skipped{{Name: "ours/shared", State: access.StateGone}}
	if diff := cmp.Diff(want, got.Skipped); diff != "" {
		t.Errorf("skipped (-want +got):\n%s", diff)
	}
}

func TestRead(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 1000) + "\n"
	var outlined strings.Builder
	for range knowledge.MaxOutline {
		outlined.WriteString("# " + strings.Repeat("é", 2500) + "\n" + strings.Repeat("text ", 200) + "\n")
	}
	f := newFixture(t, withKB(map[string]string{
		"services/esi-proxy.md": esiProxy,
		"lines.md":              strings.Repeat("a\n", 1500),
		"bytes.md":              strings.Repeat(long, 100),
		"huge.md":               "é" + strings.Repeat("y", knowledge.MaxReadBytes+10) + "\ntail\n",
		"crlf.md":               "# A\r\n\r\nb\r\n",
		"empty.md":              "",
		"headings.md":           strings.Repeat("# H\n", knowledge.MaxOutline+5),
		"straddle.md":           strings.Repeat("y", knowledge.MaxReadBytes-1) + "é\n",
		"outlined.md":           outlined.String(),
	}))
	testCases := []struct {
		name  string
		req   knowledge.ReadRequest
		code  knowledge.Code
		check func(t *testing.T, p knowledge.Page)
	}{
		{
			name: "whole document",
			req:  knowledge.ReadRequest{Vault: "Ours/KB", Path: "services/esi-proxy.md"},
			check: func(t *testing.T, p knowledge.Page) {
				want := knowledge.Page{
					Vault: "ours/kb",
					Document: index.Document{
						Path:    "services/esi-proxy.md",
						Title:   "ESI proxy",
						Status:  document.StatusActive,
						Tags:    []string{"esi"},
						Bytes:   len(esiProxy),
						Lines:   12, // the index counts the empty line after the last line ending
						Version: blobID(esiProxy),
					},
					Content:    esiProxy,
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
			req:  knowledge.ReadRequest{Vault: "ours/kb", Path: "/services/esi-proxy.md", FromLine: 9, LineCount: 1},
			check: func(t *testing.T, p knowledge.Page) {
				if p.Content != "## Deploying\n" || p.LineCount != 1 || p.TotalLines != 11 {
					t.Errorf("got %q, line count %d of %d, want line 9 of 11", p.Content, p.LineCount, p.TotalLines)
				}
				want := []document.Heading{{Level: 1, Text: "ESI proxy", Line: 5}, {Level: 2, Text: "Deploying", Line: 9}}
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
				if want := knowledge.MaxReadBytes / len(long); p.LineCount != want || len(p.Content) != want*len(long) {
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
				if len(p.Content) != knowledge.MaxReadBytes-1 || !utf8.ValidString(p.Content) {
					t.Errorf("got %d bytes, valid UTF-8 %t, want %d valid bytes", len(p.Content), utf8.ValidString(p.Content), knowledge.MaxReadBytes-1)
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
			// Long headings can't make a read much bigger than 64 KiB: they're cut, and the outline
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
		{name: "past the end", req: knowledge.ReadRequest{Vault: "ours/kb", Path: "services/esi-proxy.md", FromLine: 12}, code: knowledge.CodeInvalid},
		{name: "negative line count", req: knowledge.ReadRequest{Vault: "ours/kb", Path: "services/esi-proxy.md", LineCount: -1}, code: knowledge.CodeInvalid},
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

func TestHistory(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		history []gh.Commit
		err     error
		req     knowledge.HistoryRequest
		want    []knowledge.Commit
		code    knowledge.Code
	}{
		{
			name: "document",
			history: []gh.Commit{
				{SHA: "c2", Author: "octocat", Date: time.Unix(2, 0), Message: "Update a.md\n\nWhy.\n\nSpeki-Client: Claude Code\n"},
				{SHA: "c1", Author: "someone", Date: time.Unix(1, 0), Message: "Create a.md\n\nSpeki-Client: fake\nin the body\n\nSigned-off-by: x\n"},
			},
			req: knowledge.HistoryRequest{Vault: "ours/kb", Path: "/a.md"},
			want: []knowledge.Commit{
				{SHA: "c2", Author: "octocat", Date: time.Unix(2, 0), Message: "Update a.md", Client: "Claude Code"},
				{SHA: "c1", Author: "someone", Date: time.Unix(1, 0), Message: "Create a.md"},
			},
		},
		{
			name: "long fields are cut",
			history: []gh.Commit{{
				SHA:     "c1",
				Author:  strings.Repeat("a", 1000),
				Message: strings.Repeat("é", 1000) + "\n\nSpeki-Client: " + strings.Repeat("é", 100),
			}},
			req: knowledge.HistoryRequest{Vault: "ours/kb", Path: "a.md"},
			want: []knowledge.Commit{{
				SHA:     "c1",
				Author:  strings.Repeat("a", knowledge.MaxAuthor-len("…")) + "…",
				Message: strings.Repeat("é", (knowledge.MaxSubject-len("…"))/2) + "…",
				Client:  strings.Repeat("é", knowledge.MaxClient/2),
			}},
		},
		{name: "repository gone", err: gh.ErrNotFound, req: knowledge.HistoryRequest{Vault: "ours/kb"}, code: knowledge.CodeNotFound},
		{name: "signed out", err: fmt.Errorf("commits: %w", gh.ErrUnauthorized), req: knowledge.HistoryRequest{Vault: "ours/kb"}, code: knowledge.CodeUnavailable},
		{name: "limit too high", req: knowledge.HistoryRequest{Vault: "ours/kb", Limit: 51}, code: knowledge.CodeInvalid},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.github.history, f.github.historyErr = testCase.history, testCase.err
			got, err := f.base.History(t.Context(), f.grant, testCase.req)
			if testCase.code != "" {
				wantCode(t, err, testCase.code)
				return
			}
			if err != nil {
				t.Fatalf("History: %v", err)
			}
			if diff := cmp.Diff(testCase.want, got); diff != "" {
				t.Errorf("history (-want +got):\n%s", diff)
			}
			h := f.github
			if h.token != "ghu_x" || h.repo != "ours/kb" || h.branch != "main" || h.path != "a.md" || h.limit != 10 {
				t.Errorf("got a request for token %q, %s@%s, %q, limit %d, want ghu_x, ours/kb@main, a.md, 10", h.token, h.repo, h.branch, h.path, h.limit)
			}
		})
	}
}

// TestOtherVaultsStayHidden tries every read path on a vault the user can't read, which has a document
// at a path that also exists in a vault they can.
func TestOtherVaultsStayHidden(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	const hidden = "secret"

	vaults, _ := f.base.Vaults(ctx, f.grant)
	for _, v := range vaults {
		if v.Name == secret.Name {
			t.Error("Vaults lists the secret vault")
		}
	}
	res, err := f.base.Search(ctx, f.grant, knowledge.SearchRequest{Query: hidden})
	if err != nil || len(res.Hits) != 0 {
		t.Errorf("search all: got %+v, %v, want no hits", res.Hits, err)
	}
	res, err = f.base.Search(ctx, f.grant, knowledge.SearchRequest{Query: "zeppelin"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, h := range res.Hits {
		if h.Source == secret.Name || strings.Contains(h.Snippet, hidden) {
			t.Errorf("got a search hit from the secret vault: %+v", h)
		}
	}
	// Spelling correction only knows the user's vaults: "secrut" is close to a word only the secret
	// vault has, so it isn't corrected to it.
	res, err = f.base.Search(ctx, f.grant, knowledge.SearchRequest{Query: "secrut"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for word, instead := range res.Corrected {
		if instead == hidden {
			t.Errorf("got %q corrected to %q, a word only the secret vault has", word, instead)
		}
	}
	// A readable vault's document at the same path is that vault's, not the secret one.
	p, err := f.base.Read(ctx, f.grant, knowledge.ReadRequest{Vault: "ours/kb", Path: "services/esi-proxy.md"})
	if err != nil || strings.Contains(p.Content, hidden) {
		t.Errorf("read: got %q, %v, want ours/kb's document", p.Content, err)
	}

	calls := map[string]func() error{
		"search": func() error {
			_, err := f.base.Search(ctx, f.grant, knowledge.SearchRequest{Query: hidden, Vaults: []string{"ours/kb", secret.Name}, Path: "services", Tags: []string{"x"}})
			return err
		},
		"list": func() error {
			_, err := f.base.List(ctx, f.grant, knowledge.ListRequest{Vault: secret.Name, Path: "services"})
			return err
		},
		"read": func() error {
			_, err := f.base.Read(ctx, f.grant, knowledge.ReadRequest{Vault: secret.Name, Path: "services/esi-proxy.md"})
			return err
		},
		"read in another letter case": func() error {
			_, err := f.base.Read(ctx, f.grant, knowledge.ReadRequest{Vault: "OURS/SECRET", Path: "services/esi-proxy.md"})
			return err
		},
		"history": func() error {
			_, err := f.base.History(ctx, f.grant, knowledge.HistoryRequest{Vault: secret.Name})
			return err
		},
		"query": func() error {
			_, err := f.base.Query(ctx, f.grant, knowledge.QueryRequest{SQL: "SELECT 1", Vaults: []string{secret.Name}})
			return err
		},
	}
	for name, call := range calls {
		err := call()
		if kerr, ok := errors.AsType[*knowledge.Error](err); !ok || kerr.Code != knowledge.CodeNotFound {
			t.Errorf("%s: got %v, want not_found", name, err)
		}
	}
	if f.github.repo != "" {
		t.Error("asked GitHub about the secret vault")
	}

	// The answer for a vault the user can't read is the answer for one that doesn't exist.
	_, errSecret := f.base.Read(ctx, f.grant, knowledge.ReadRequest{Vault: secret.Name, Path: "x.md"})
	_, errNone := f.base.Read(ctx, f.grant, knowledge.ReadRequest{Vault: "ours/nothing", Path: "x.md"})
	if got, want := strings.ReplaceAll(errSecret.Error(), secret.Name, "V"), strings.ReplaceAll(errNone.Error(), "ours/nothing", "V"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
