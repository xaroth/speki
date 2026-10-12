package knowledge

import (
	"context"
	"errors"
	"strings"

	"github.com/eve-online-tools/speki/access"
	"github.com/eve-online-tools/speki/index"
)

// SearchRequest is a full-text search.
type SearchRequest struct {
	Query             string
	Vaults            []string // empty means every vault the user may read
	Path              string   // only documents under this folder
	Tags              []string // documents must have all of them
	ExcludeDeprecated bool
	Limit             int // 1 to 50; 0 means 10
}

// SearchResult is what a search found, and the vaults it didn't cover.
type SearchResult struct {
	index.Results
	Skipped []Skipped // only when no vault was named
}

// Search runs a full-text search across vaults, best match first. Hits carry the vault name as Source.
func (b *Base) Search(ctx context.Context, g *access.Grant, req SearchRequest) (SearchResult, error) {
	if req.Limit < 0 || req.Limit > 50 {
		return SearchResult{}, errorf(CodeInvalid, "limit must be between 1 and 50")
	}
	if strings.TrimSpace(req.Query) == "" {
		return SearchResult{}, errorf(CodeInvalid, "the query is empty. Give one or more words to search for")
	}

	sources, notServed, err := b.sources(g, req.Vaults)
	if err != nil {
		return SearchResult{}, err
	}

	prefix := strings.Trim(req.Path, "/")
	if prefix != "" {
		prefix += "/"
	}
	res, err := index.Search(ctx, sources, req.Query, index.Options{
		Limit:             req.Limit,
		Tags:              req.Tags,
		PathPrefix:        prefix,
		ExcludeDeprecated: req.ExcludeDeprecated,
	})
	switch {
	case errors.Is(err, index.ErrEmptyQuery):
		return SearchResult{}, errorf(CodeInvalid, "the query has no words to search for, only exclusions. Add a word")
	case errors.Is(err, index.ErrTooManyWords):
		return SearchResult{}, errorf(CodeInvalid, "%s. Search for the few words that matter most", err)
	case errors.Is(err, index.ErrClosed):
		return SearchResult{}, errorf(CodeUnavailable, "a vault was just removed or is reopening. Search again")
	case err != nil:
		return SearchResult{}, err
	}
	return SearchResult{Results: res, Skipped: notServed}, nil
}
