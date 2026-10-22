package service

import (
	"context"

	"github.com/xaroth/speki/gh"
	"github.com/xaroth/speki/gh/ghcache"
)

// polledAPI is what discovery and the poller read from GitHub.
type polledAPI interface {
	Installations(ctx context.Context) ([]gh.Installation, error)
	Repositories(ctx context.Context, installationID int64) ([]gh.Repository, error)
	File(ctx context.Context, installationID int64, repo, ref, path string) ([]byte, error)
	BranchHead(ctx context.Context, installationID int64, repo, branch, etag string) (gh.Head, error)
	RepositoryID(ctx context.Context, installationID int64, repo string) (int64, error)
}

// newPolledGitHub shares the reads every round of discovery and polling makes between instances, when
// there is Redis to share them through. A single instance reads GitHub directly.
func newPolledGitHub(cfg Config, github *gh.Client, shared sharedStore) polledAPI {
	if cfg.Redis.URL == "" {
		return github
	}
	// A little under the intervals, so each round asks GitHub once across instances.
	return sharedPolls{
		Client: github,
		shared: ghcache.New(github, shared, ghcache.Options{
			Heads:     cfg.Sync.PollInterval * 9 / 10,
			Discovery: cfg.Sync.DiscoveryInterval * 9 / 10,
		}),
	}
}

// sharedPolls reads installations, repositories and branch heads through the shared answers, and the
// rest from GitHub.
type sharedPolls struct {
	*gh.Client
	shared *ghcache.Client
}

func (p sharedPolls) Installations(ctx context.Context) ([]gh.Installation, error) {
	return p.shared.Installations(ctx)
}

func (p sharedPolls) Repositories(ctx context.Context, installationID int64) ([]gh.Repository, error) {
	return p.shared.Repositories(ctx, installationID)
}

func (p sharedPolls) BranchHead(ctx context.Context, installationID int64, repo, branch, etag string) (gh.Head, error) {
	return p.shared.BranchHead(ctx, installationID, repo, branch, etag)
}
