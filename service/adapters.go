package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/xaroth/speki/auth/access"
	"github.com/xaroth/speki/auth/oauth"
	"github.com/xaroth/speki/knowledge"
	"github.com/xaroth/speki/library"
)

// oauthUsers hands mcpserver the signed-in user as access knows them.
type oauthUsers struct{ *oauth.Server }

func (oauthUsers) User(ctx context.Context) (access.User, bool) {
	id, ok := oauth.IdentityFrom(ctx)
	return access.User{
		ID:     id.UserID,
		Login:  id.Login,
		Token:  id.GitHubToken,
		Client: id.Client,
		Write:  id.Write,
	}, ok
}

// libraryVaults hands knowledge the library's vaults.
type libraryVaults struct{ lib *library.Library }

func (l libraryVaults) Copy(repoID int64) (knowledge.Copy, bool) {
	v, ok := l.lib.Vault(repoID)
	if !ok {
		return nil, false // not a typed nil inside the interface
	}
	return v, true
}

func (l libraryVaults) Sync(ctx context.Context, repoID int64) error {
	v, ok := l.lib.Vault(repoID)
	if !ok {
		return fmt.Errorf("vault %d isn't served", repoID)
	}
	err := l.lib.SyncChanges(ctx, v) // knowledge syncs for writes, which mustn't wait for a rebuild
	if errors.Is(err, library.ErrRebuildNeeded) {
		return knowledge.ErrRebuilding
	}
	return err
}
