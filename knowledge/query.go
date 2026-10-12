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

// QueryResult is a query's rows, and the vaults it didn't cover.
type QueryResult struct {
	Rows    index.Rows
	Skipped []Skipped // only when no vault was named
}

// Query runs one SELECT over the user's vaults, one query per user at a time, within index.QueryTimeout
// (waiting for the user's earlier query included).
func (b *Base) Query(ctx context.Context, g *access.Grant, req QueryRequest) (QueryResult, error) {
	sources, notServed, err := b.sources(g, req.Vaults)
	if err != nil {
		return QueryResult{}, err
	}
	if len(sources) > index.MaxQuerySources { // counted after repeats are dropped
		if len(req.Vaults) == 0 {
			return QueryResult{}, errorf(CodeInvalid, "you can read %d vaults, and a query covers at most %d. Name the ones to query in vaults", len(sources), index.MaxQuerySources)
		}
		return QueryResult{}, errorf(CodeInvalid, "a query covers at most %d vaults", index.MaxQuerySources)
	}
	ctx, cancel := context.WithTimeout(ctx, index.QueryTimeout)
	defer cancel()
	done, err := b.queries.lock(ctx, g.User().ID)
	if err != nil {
		return QueryResult{}, errorf(CodeTimeout, "your previous query was still running. Wait for it, or make it faster")
	}
	defer done()

	rows, err := b.sandbox.Query(ctx, sources, req.SQL)
	switch {
	case err == nil:
		return QueryResult{Rows: rows, Skipped: notServed}, nil
	case errors.Is(err, index.ErrBadQuery):
		return QueryResult{}, errorf(CodeInvalid, "%s", strings.TrimPrefix(err.Error(), index.ErrBadQuery.Error()+": "))
	case errors.Is(err, index.ErrQueryTimeout):
		return QueryResult{}, errorf(CodeTimeout, "the query ran longer than %s. Narrow it down, or use search", index.QueryTimeout)
	case errors.Is(err, index.ErrQueryTooLarge):
		return QueryResult{}, errorf(CodeTooLarge, "%s. Select fewer or shorter values", strings.TrimPrefix(err.Error(), index.ErrQueryTooLarge.Error()+": "))
	case errors.Is(err, index.ErrQueryBusy):
		return QueryResult{}, errorf(CodeUnavailable, "%s. Try again in a few seconds", strings.TrimPrefix(err.Error(), index.ErrQueryBusy.Error()+": "))
	case errors.Is(err, index.ErrClosed):
		return QueryResult{}, errorf(CodeUnavailable, "a vault was just removed or is reopening. Run the query again")
	}
	return QueryResult{}, err
}
