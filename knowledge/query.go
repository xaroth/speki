package knowledge

import (
	"context"
	"errors"
	"strings"

	"github.com/eve-online-tools/speki/access"
	"github.com/eve-online-tools/speki/index"
)

// QueryRequest is one read-only SQL query.
type QueryRequest struct {
	SQL    string
	Vaults []string // empty means every vault the user may read, if that's at most 10
}

// Query runs one SELECT over the user's vaults, one query per user at a time, within index.QueryTimeout
// (waiting for the user's earlier query included).
func (b *Base) Query(ctx context.Context, g *access.Grant, req QueryRequest) (index.Rows, error) {
	var sources []index.Source
	if len(req.Vaults) == 0 {
		for _, v := range g.Readable() {
			c, ok := b.vaults.Copy(v.RepoID)
			if ok {
				sources = append(sources, index.Source{Name: v.Name, Index: c.Index()})
			}
		}
		if len(sources) > index.MaxQuerySources {
			return index.Rows{}, errorf(CodeInvalid, "you can read %d vaults, and a query covers at most %d. Name the ones to query in vaults", len(sources), index.MaxQuerySources)
		}
	}
	seen := map[int64]bool{}
	for _, name := range req.Vaults {
		v, c, err := b.open(g, name)
		if err != nil {
			return index.Rows{}, err
		}
		if seen[v.RepoID] {
			continue
		}
		seen[v.RepoID] = true
		sources = append(sources, index.Source{Name: v.Name, Index: c.Index()})
	}

	if len(sources) > index.MaxQuerySources { // counted after repeats are dropped
		return index.Rows{}, errorf(CodeInvalid, "a query covers at most %d vaults", index.MaxQuerySources)
	}
	ctx, cancel := context.WithTimeout(ctx, index.QueryTimeout)
	defer cancel()
	done, err := b.queryTurn(ctx, g.User().ID)
	if err != nil {
		return index.Rows{}, errorf(CodeTimeout, "your previous query was still running. Wait for it, or make it faster")
	}
	defer done()

	rows, err := b.sandbox.Query(ctx, sources, req.SQL)
	switch {
	case err == nil:
		return rows, nil
	case errors.Is(err, index.ErrBadQuery):
		return index.Rows{}, errorf(CodeInvalid, "%s", strings.TrimPrefix(err.Error(), index.ErrBadQuery.Error()+": "))
	case errors.Is(err, index.ErrQueryTimeout):
		return index.Rows{}, errorf(CodeTimeout, "the query ran longer than %s. Narrow it down, or use search", index.QueryTimeout)
	case errors.Is(err, index.ErrQueryTooLarge):
		return index.Rows{}, errorf(CodeTooLarge, "%s. Select fewer or shorter values", strings.TrimPrefix(err.Error(), index.ErrQueryTooLarge.Error()+": "))
	case errors.Is(err, index.ErrClosed):
		return index.Rows{}, errorf(CodeNotFound, "a vault was removed during the query. Run it again")
	}
	return index.Rows{}, err
}

// queryTurn waits until the user has no other query running, or ctx ends, and returns its release.
func (b *Base) queryTurn(ctx context.Context, user int64) (func(), error) {
	b.mu.Lock()
	turn, ok := b.queries[user]
	if !ok {
		turn = make(chan struct{}, 1)
		b.queries[user] = turn
	}
	b.mu.Unlock()
	select {
	case turn <- struct{}{}:
		return func() { <-turn }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
