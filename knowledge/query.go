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
	sources, err := b.sources(g, req.Vaults)
	if err != nil {
		return index.Rows{}, err
	}
	if len(sources) > index.MaxQuerySources { // counted after repeats are dropped
		if len(req.Vaults) == 0 {
			return index.Rows{}, errorf(CodeInvalid, "you can read %d vaults, and a query covers at most %d. Name the ones to query in vaults", len(sources), index.MaxQuerySources)
		}
		return index.Rows{}, errorf(CodeInvalid, "a query covers at most %d vaults", index.MaxQuerySources)
	}
	ctx, cancel := context.WithTimeout(ctx, index.QueryTimeout)
	defer cancel()
	done, err := b.queries.lock(ctx, g.User().ID)
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
