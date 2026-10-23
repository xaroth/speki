package knowledge_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/xaroth/speki/auth/access"
	"github.com/xaroth/speki/document"
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
				{Path: "services/edge-proxy.md", Title: "Edge proxy", Status: document.StatusActive, Version: blobID(edgeProxy)},
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
