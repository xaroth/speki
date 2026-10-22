package knowledge_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/xaroth/speki/auth/access"
	"github.com/xaroth/speki/knowledge"
)

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

// The request's expired deadline stands in for index.SearchTimeout, which is longer.
func TestSearchTimeout(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := f.base.Search(ctx, f.grant, knowledge.SearchRequest{Query: "zeppelin"})
	kerr := wantError(t, err, knowledge.CodeTimeout)
	if !strings.Contains(kerr.Message, "search ran longer than") {
		t.Errorf("got %q, want it to say the search ran too long", kerr.Message)
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
