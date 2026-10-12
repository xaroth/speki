package knowledge_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/eve-online-tools/speki/access"
	"github.com/eve-online-tools/speki/knowledge"
	"github.com/eve-online-tools/speki/vault"
)

func TestQuery(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	withUnavailable(t, f)
	// eleven readable vaults, all served by ours/kb's copy
	var many []vault.Vault
	for i := range 11 {
		v := vault.Vault{RepoID: int64(100 + i), Name: fmt.Sprintf("ours/v%02d", i)}
		f.setCopy(v.RepoID, f.copy(ours.RepoID))
		many = append(many, v)
	}
	manyGrant := access.NewGrant(access.User{ID: 8}, many, nil)
	twelve := make([]string, 12)
	for i := range twelve {
		twelve[i] = "ours/kb"
	}

	testCases := []struct {
		name        string
		grant       *access.Grant // f.grant when nil
		req         knowledge.QueryRequest
		timeout     time.Duration // of the request, when set
		wantRows    string
		wantSkipped []knowledge.Skipped
		code        knowledge.Code
	}{
		{
			name:     "every vault",
			req:      knowledge.QueryRequest{SQL: "SELECT vault, count(*) FROM documents GROUP BY vault ORDER BY vault"},
			wantRows: "[[ours/kb 3] [ours/shared 1]]",
			wantSkipped: []knowledge.Skipped{
				{Name: "ours/new", State: access.StateBuilding},
				{Name: "ours/old", State: access.StateFailed},
			},
		},
		{
			name:     "one vault, named twice",
			req:      knowledge.QueryRequest{SQL: "SELECT path FROM documents", Vaults: []string{"ours/shared", "OURS/shared"}},
			wantRows: "[[zeppelin.md]]",
		},
		{
			name:     "a vault named past the limit, counted once",
			req:      knowledge.QueryRequest{SQL: "SELECT count(*) FROM documents", Vaults: twelve},
			wantRows: "[[3]]",
		},
		{
			name:     "two named of more than ten",
			grant:    manyGrant,
			req:      knowledge.QueryRequest{SQL: "SELECT count(*) FROM documents", Vaults: []string{"ours/v00", "ours/v01"}},
			wantRows: "[[6]]",
		},
		{name: "more than ten, none named", grant: manyGrant, req: knowledge.QueryRequest{SQL: "SELECT 1"}, code: knowledge.CodeInvalid},
		{name: "syntax", req: knowledge.QueryRequest{SQL: "SELEC 1"}, code: knowledge.CodeInvalid},
		{name: "write", req: knowledge.QueryRequest{SQL: "DELETE FROM documents"}, code: knowledge.CodeInvalid},
		{name: "another vault", req: knowledge.QueryRequest{SQL: "SELECT 1", Vaults: []string{"ours/secret"}}, code: knowledge.CodeNotFound},
		{name: "an attached index", req: knowledge.QueryRequest{SQL: "SELECT * FROM v2.documents"}, code: knowledge.CodeInvalid},
		{name: "too much", req: knowledge.QueryRequest{SQL: "SELECT printf('%.*c', 2000000, 'x')"}, code: knowledge.CodeTooLarge},
		{
			// The request's deadline stands in for index.QueryTimeout, which is longer.
			name:    "endless",
			req:     knowledge.QueryRequest{SQL: "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n) SELECT count(*) FROM n"},
			timeout: 50 * time.Millisecond,
			code:    knowledge.CodeTimeout,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			g := f.grant
			if testCase.grant != nil {
				g = testCase.grant
			}
			ctx := t.Context()
			if testCase.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, testCase.timeout)
				defer cancel()
			}
			got, err := f.base.Query(ctx, g, testCase.req)
			if testCase.code != "" {
				wantCode(t, err, testCase.code)
				return
			}
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if rows := fmt.Sprint(got.Rows.Rows); rows != testCase.wantRows {
				t.Errorf("got rows %s, want %s", rows, testCase.wantRows)
			}
			if diff := cmp.Diff(testCase.wantSkipped, got.Skipped); diff != "" {
				t.Errorf("skipped (-want +got):\n%s", diff)
			}
		})
	}
}

// A user's query waits for their previous one, not for other users'.
func TestOneQueryPerUser(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	first := make(chan error, 1)
	go func() {
		_, err := f.base.Query(t.Context(), f.grant, knowledge.QueryRequest{SQL: heldSQL})
		first <- err
	}()
	<-f.sandbox.started

	ended, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := f.base.Query(ended, f.grant, knowledge.QueryRequest{SQL: "SELECT 1"})
	kerr := wantError(t, err, knowledge.CodeTimeout)
	if !strings.Contains(kerr.Message, "previous query was still running") {
		t.Errorf("got %q, want it to name the previous query", kerr.Message)
	}

	other := access.NewGrant(access.User{ID: 8}, []vault.Vault{ours}, nil)
	if _, err := f.base.Query(t.Context(), other, knowledge.QueryRequest{SQL: "SELECT 1"}); err != nil {
		t.Errorf("another user's query: got %v, want no error", err)
	}

	close(f.sandbox.release)
	if err := <-first; err != nil {
		t.Errorf("held query: got %v, want no error", err)
	}
	if _, err := f.base.Query(t.Context(), f.grant, knowledge.QueryRequest{SQL: "SELECT 1"}); err != nil {
		t.Errorf("after the first query ended: got %v, want no error", err)
	}
}
