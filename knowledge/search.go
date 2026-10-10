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

// Search runs a full-text search across vaults, best match first. Hits carry the vault name as Source.
func (b *Base) Search(ctx context.Context, g *access.Grant, req SearchRequest) (index.Results, error) {
	if req.Limit < 0 || req.Limit > 50 {
		return index.Results{}, errorf(Invalid, "limit must be between 1 and 50")
	}
	if strings.TrimSpace(req.Query) == "" {
		return index.Results{}, errorf(Invalid, "the query is empty. Give one or more words to search for")
	}

	var sources []index.Source
	if len(req.Vaults) == 0 {
		for _, v := range g.Readable() {
			c, ok := b.vaults.Copy(v.RepoID)
			if ok {
				sources = append(sources, index.Source{Name: v.Name, Index: c.Index()})
			}
		}
	}
	for _, name := range req.Vaults {
		v, c, err := b.open(g, name)
		if err != nil {
			return index.Results{}, err
		}
		sources = append(sources, index.Source{Name: v.Name, Index: c.Index()})
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
		return index.Results{}, errorf(Invalid, "the query has no words to search for, only exclusions. Add a word")
	case errors.Is(err, index.ErrClosed):
		return index.Results{}, errorf(NotFound, "a vault was removed during the search. Search again")
	case err != nil:
		return index.Results{}, err
	}
	return res, nil
}
