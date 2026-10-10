// Package knowledge is what speki's tools do: list, search, read and history over the vaults a user may
// see. It knows nothing about MCP. Every function that touches a vault takes the user's grant and checks
// it before anything else.
package knowledge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/eve-online-tools/speki/access"
	"github.com/eve-online-tools/speki/gh"
	"github.com/eve-online-tools/speki/index"
	"github.com/eve-online-tools/speki/vault"
)

// Copy is one vault's local copy.
type Copy interface {
	Index() *index.Index
	Blob(sha string) ([]byte, error)
}

// historian looks up commits on GitHub as the user.
type historian interface {
	Commits(ctx context.Context, token, repo, branch, path string, limit int) ([]gh.Commit, error)
}

// Base answers the tools' questions.
type Base struct {
	copies func(repoID int64) (Copy, bool)
	github historian
	logger *slog.Logger
}

// New returns a Base. copies returns the local copy of a vault that's being served.
func New(copies func(repoID int64) (Copy, bool), github historian, logger *slog.Logger) *Base {
	return &Base{copies: copies, github: github, logger: logger}
}

// open returns a vault the user may read, by name, with its local copy.
func (b *Base) open(g *access.Grant, name string) (vault.Vault, Copy, error) {
	v, ok := g.Vault(name)
	if !ok {
		return vault.Vault{}, nil, noVault(name)
	}
	c, ok := b.copies(v.RepoID)
	if !ok {
		return vault.Vault{}, nil, noVault(name)
	}
	return v, c, nil
}

func noVault(name string) *Error {
	return errorf(NotFound, "there's no vault named %q that you can read. Call list without arguments to see your vaults", name)
}

// indexError turns an index that closed under a request (its vault was just removed) into a not_found.
func indexError(name string, err error) error {
	if errors.Is(err, index.ErrClosed) {
		return noVault(name)
	}
	return fmt.Errorf("%s: %w", name, err)
}
