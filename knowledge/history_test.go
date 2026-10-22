package knowledge_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/xaroth/speki/gh"
	"github.com/xaroth/speki/knowledge"
)

func TestHistory(t *testing.T) {
	t.Parallel()
	c1, c2 := sha(1), sha(2)
	testCases := []struct {
		name         string
		history      []gh.Commit
		err          error
		tipErr       error
		req          knowledge.HistoryRequest
		want         knowledge.HistoryPage
		wantListings []listing
		code         knowledge.Code
	}{
		{
			name: "document",
			history: []gh.Commit{
				{SHA: c2, Author: "octocat", Date: time.Unix(2, 0), Message: "Update a.md\n\nWhy.\n\nSpeki-Client: Claude Code\n"},
				{SHA: c1, Author: "someone", Date: time.Unix(1, 0), Message: "Create a.md\n\nSpeki-Client: fake\nin the body\n\nSigned-off-by: x\n"},
			},
			req: knowledge.HistoryRequest{Vault: "ours/kb", Path: "/a.md"},
			want: knowledge.HistoryPage{Commits: []knowledge.Commit{
				{SHA: c2, Author: "octocat", Date: time.Unix(2, 0), Message: "Update a.md", Client: "Claude Code"},
				{SHA: c1, Author: "someone", Date: time.Unix(1, 0), Message: "Create a.md"},
			}},
			wantListings: []listing{{Token: "ghu_x", Repo: "ours/kb", From: c2, Path: "a.md", Limit: 11}},
		},
		{
			name:         "vault",
			history:      []gh.Commit{{SHA: c1, Author: "octocat", Message: "Create a.md"}},
			req:          knowledge.HistoryRequest{Vault: "ours/kb", Limit: 1},
			want:         knowledge.HistoryPage{Commits: []knowledge.Commit{{SHA: c1, Author: "octocat", Message: "Create a.md"}}},
			wantListings: []listing{{Token: "ghu_x", Repo: "ours/kb", From: "main", Limit: 2}},
		},
		{
			name: "long fields are cut",
			history: []gh.Commit{{
				SHA:     c1,
				Author:  strings.Repeat("a", 1000),
				Message: strings.Repeat("é", 1000) + "\n\nSpeki-Client: " + strings.Repeat("é", 100),
			}},
			req: knowledge.HistoryRequest{Vault: "ours/kb", Path: "a.md"},
			want: knowledge.HistoryPage{Commits: []knowledge.Commit{{
				SHA:     c1,
				Author:  strings.Repeat("a", knowledge.MaxAuthor-len("…")) + "…",
				Message: strings.Repeat("é", (knowledge.MaxSubject-len("…"))/2) + "…",
				Client:  strings.Repeat("é", knowledge.MaxClient/2),
			}}},
			wantListings: []listing{{Token: "ghu_x", Repo: "ours/kb", From: c1, Path: "a.md", Limit: 11}},
		},
		{name: "repository gone", err: gh.ErrNotFound, req: knowledge.HistoryRequest{Vault: "ours/kb"}, code: knowledge.CodeNotFound},
		{name: "signed out", err: fmt.Errorf("commits: %w", gh.ErrUnauthorized), req: knowledge.HistoryRequest{Vault: "ours/kb"}, code: knowledge.CodeUnavailable},
		{name: "signed out at the head", tipErr: fmt.Errorf("head: %w", gh.ErrUnauthorized), req: knowledge.HistoryRequest{Vault: "ours/kb", Path: "a.md"}, code: knowledge.CodeUnavailable},
		{name: "limit too high", req: knowledge.HistoryRequest{Vault: "ours/kb", Limit: 51}, code: knowledge.CodeInvalid},
		{name: "cursor without a count", req: knowledge.HistoryRequest{Vault: "ours/kb", Cursor: c1}, code: knowledge.CodeInvalid},
		{name: "cursor with a branch", req: knowledge.HistoryRequest{Vault: "ours/kb", Cursor: "main:10"}, code: knowledge.CodeInvalid},
		{name: "cursor with a short SHA", req: knowledge.HistoryRequest{Vault: "ours/kb", Cursor: c1[:7] + ":10"}, code: knowledge.CodeInvalid},
		{name: "cursor with a zero count", req: knowledge.HistoryRequest{Vault: "ours/kb", Cursor: c1 + ":0"}, code: knowledge.CodeInvalid},
		{name: "cursor with a signed count", req: knowledge.HistoryRequest{Vault: "ours/kb", Cursor: c1 + ":+10"}, code: knowledge.CodeInvalid},
		{name: "cursor with an unknown commit", err: gh.ErrNotFound, req: knowledge.HistoryRequest{Vault: "ours/kb", Cursor: c1 + ":10"}, code: knowledge.CodeInvalid},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.github.history, f.github.historyErr, f.github.tipErr = testCase.history, testCase.err, testCase.tipErr
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
			if diff := cmp.Diff(testCase.wantListings, f.github.listings); diff != "" {
				t.Errorf("listings (-want +got):\n%s", diff)
			}
		})
	}
}

// TestHistoryPages follows next cursors to the end, and gets the whole history in order.
func TestHistoryPages(t *testing.T) {
	t.Parallel()
	var history []gh.Commit
	var want []knowledge.Commit
	for i := 5; i > 0; i-- {
		history = append(history, gh.Commit{SHA: sha(i), Author: "octocat", Message: fmt.Sprintf("Change %d", i)})
		want = append(want, knowledge.Commit{SHA: sha(i), Author: "octocat", Message: fmt.Sprintf("Change %d", i)})
	}
	testCases := []struct {
		name     string
		path     string
		wantTips int
	}{
		{name: "vault"},
		{name: "document", path: "a.md", wantTips: 1},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.github.history = history
			var got []knowledge.Commit
			var cursors []string
			req := knowledge.HistoryRequest{Vault: "ours/kb", Path: testCase.path, Limit: 2}
			for range 5 {
				page, err := f.base.History(t.Context(), f.grant, req)
				if err != nil {
					t.Fatalf("History: %v", err)
				}
				got = append(got, page.Commits...)
				if page.NextCursor == "" {
					break
				}
				cursors = append(cursors, page.NextCursor)
				req.Cursor = page.NextCursor
				f.github.history = append([]gh.Commit{{SHA: sha(9), Message: "Newer"}}, f.github.history...) // must not shift the pages
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("history (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff([]string{sha(5) + ":2", sha(5) + ":4"}, cursors); diff != "" {
				t.Errorf("cursors (-want +got):\n%s", diff)
			}
			if f.github.tips != testCase.wantTips {
				t.Errorf("got %d head lookups, want %d", f.github.tips, testCase.wantTips)
			}
		})
	}
}

// sha returns a full commit SHA made from n.
func sha(n int) string {
	return fmt.Sprintf("%040x", n)
}
