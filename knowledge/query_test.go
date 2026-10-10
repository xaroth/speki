package knowledge

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/eve-online-tools/speki/access"
	"github.com/eve-online-tools/speki/vault"
)

func TestQuery(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	rows, err := f.base.Query(ctx, f.grant, QueryRequest{SQL: "SELECT vault, count(*) FROM documents GROUP BY vault ORDER BY vault"})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(rows.Rows) != "[[ours/kb 3] [ours/shared 1]]" {
		t.Errorf("rows = %v", rows.Rows)
	}
	rows, err = f.base.Query(ctx, f.grant, QueryRequest{SQL: "SELECT path FROM documents", Vaults: []string{"ours/shared"}})
	if err != nil || fmt.Sprint(rows.Rows) != "[[zeppelin.md]]" {
		t.Errorf("one vault: %v, %v", rows.Rows, err)
	}

	for name, tt := range map[string]struct {
		req  QueryRequest
		code Code
	}{
		"syntax":            {QueryRequest{SQL: "SELEC 1"}, Invalid},
		"write":             {QueryRequest{SQL: "DELETE FROM documents"}, Invalid},
		"another vault":     {QueryRequest{SQL: "SELECT 1", Vaults: []string{"ours/secret"}}, NotFound},
		"an attached index": {QueryRequest{SQL: "SELECT * FROM v2.documents"}, Invalid},
		"too much":          {QueryRequest{SQL: "SELECT printf('%.*c', 2000000, 'x')"}, TooLarge},
		"endless":           {QueryRequest{SQL: "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n) SELECT count(*) FROM n"}, Timeout},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.base.Query(ctx, f.grant, tt.req)
			wantCode(t, err, tt.code)
		})
	}
}

func TestQueryNeedsVaultsPastTen(t *testing.T) {
	f := newFixture(t)
	var many []vault.Vault
	for i := range 11 {
		id := int64(100 + i)
		f.copies[id] = f.copies[1]
		many = append(many, vault.Vault{RepoID: id, Name: fmt.Sprintf("ours/v%02d", i)})
	}
	g := access.NewGrant(access.User{ID: 7}, many, nil)
	_, err := f.base.Query(t.Context(), g, QueryRequest{SQL: "SELECT 1"})
	wantCode(t, err, Invalid)
	_, err = f.base.Query(t.Context(), g, QueryRequest{SQL: "SELECT count(*) FROM documents", Vaults: []string{"ours/v00", "ours/v01"}})
	if err != nil {
		t.Errorf("naming vaults: %v", err)
	}
}

func TestOneQueryPerUser(t *testing.T) {
	f := newFixture(t)
	done, err := f.base.queryTurn(t.Context(), f.grant.User().ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err = f.base.Query(ctx, f.grant, QueryRequest{SQL: "SELECT 1"})
	wantCode(t, err, Timeout)

	other := access.NewGrant(access.User{ID: 8}, []vault.Vault{ours}, nil)
	if _, err := f.base.Query(t.Context(), other, QueryRequest{SQL: "SELECT 1"}); err != nil {
		t.Errorf("another user waited too: %v", err)
	}
	done()
	if _, err := f.base.Query(t.Context(), f.grant, QueryRequest{SQL: "SELECT 1"}); err != nil {
		t.Errorf("after the first query ended: %v", err)
	}
}
