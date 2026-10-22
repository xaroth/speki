package knowledge

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/xaroth/speki/auth/access"
	"github.com/xaroth/speki/library/index"
)

// SearchRequest is a full-text search.
type SearchRequest struct {
	Query             string
	Vaults            []string // empty means every vault the user may read
	Repository        string   // only vaults covering this repository, a git remote or host/owner/repo
	Path              string   // only documents under this folder
	Tags              []string // documents must have all of them
	ExcludeDeprecated bool
	Limit             int // 1 to 50; 0 means 10
}

// SearchResult is what a search found, and the vaults it didn't cover.
type SearchResult struct {
	index.Results
	Skipped []Skipped // only when no vault was named
	// With a Repository: the repository normalised, and the vaults that cover it that were searched.
	Repository string
	Searched   []string
}

// Search runs a full-text search across vaults, best match first, within index.SearchTimeout. Hits carry
// the vault name as Source.
func (b *Base) Search(ctx context.Context, g *access.Grant, req SearchRequest) (SearchResult, error) {
	if req.Limit < 0 || req.Limit > 50 {
		return SearchResult{}, errorf(CodeInvalid, "limit must be between 1 and 50")
	}
	if strings.TrimSpace(req.Query) == "" {
		return SearchResult{}, errorf(CodeInvalid, "the query is empty. Give one or more words to search for")
	}

	var covered map[string]bool
	var repository string
	if req.Repository != "" {
		repo, names, err := covering(g, req.Repository)
		if err != nil {
			return SearchResult{}, err
		}
		covered, repository = names, repo.String()
	}
	sources, notServed, err := b.sources(g, req.Vaults)
	if err != nil {
		return SearchResult{}, err
	}
	var searched []string
	if covered != nil {
		sources = slices.DeleteFunc(sources, func(s index.Source) bool { return !covered[s.Name] })
		notServed = slices.DeleteFunc(notServed, func(s Skipped) bool { return !covered[s.Name] })
		if len(notServed) == 0 {
			notServed = nil
		}
		for _, s := range sources {
			searched = append(searched, s.Name)
		}
		slices.Sort(searched)
		if len(sources) == 0 {
			return SearchResult{Skipped: notServed, Repository: repository}, nil
		}
	}

	prefix := strings.Trim(req.Path, "/")
	if prefix != "" {
		prefix += "/"
	}
	ctx, cancel := context.WithTimeout(ctx, index.SearchTimeout)
	defer cancel()
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
	case err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded): // SQLite reports it as an interrupt
		return SearchResult{}, errorf(CodeTimeout, "the search ran longer than %s. Name fewer vaults, or narrow it with path or tags", index.SearchTimeout)
	case err != nil:
		return SearchResult{}, err
	}
	return SearchResult{Results: res, Skipped: notServed, Repository: repository, Searched: searched}, nil
}
