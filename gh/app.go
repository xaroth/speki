package gh

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/go-github/v88/github"
)

// Installation is one installation of the app on an account.
type Installation struct {
	ID        int64
	AccountID int64
	// Suspended installations can't be used until an owner unsuspends them.
	Suspended bool
}

// Repository is a repository an installation can see.
type Repository struct {
	ID            int64
	FullName      string // owner/repo
	DefaultBranch string
	PushedAt      time.Time
}

// AppOwner returns the ID of the account that owns the app.
func (c *Client) AppOwner(ctx context.Context) (int64, error) {
	app, _, err := c.app.Apps.Get(ctx, "")
	if err != nil {
		return 0, fmt.Errorf("get app: %w", err)
	}
	return app.GetOwner().GetID(), nil
}

// Installations lists every installation of the app.
func (c *Client) Installations(ctx context.Context) ([]Installation, error) {
	var out []Installation
	opt := &github.ListOptions{PerPage: 100}
	for {
		page, resp, err := c.app.Apps.ListInstallations(ctx, opt)
		if err != nil {
			return nil, fmt.Errorf("list installations: %w", err)
		}
		for _, in := range page {
			out = append(out, Installation{
				ID:        in.GetID(),
				AccountID: in.GetAccount().GetID(),
				Suspended: in.SuspendedAt != nil,
			})
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opt.Page = resp.NextPage
	}
}

// Repositories lists the repositories an installation can see. ErrNotFound means the installation is
// gone.
func (c *Client) Repositories(ctx context.Context, installationID int64) ([]Repository, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return nil, err
	}
	var out []Repository
	opt := &github.ListOptions{PerPage: 100}
	for {
		page, resp, err := client.Apps.ListRepos(ctx, opt)
		if err != nil {
			return nil, fmt.Errorf("list repositories of installation %d: %w", installationID, notFound(err))
		}
		for _, r := range page.Repositories {
			out = append(out, Repository{
				ID:            r.GetID(),
				FullName:      r.GetFullName(),
				DefaultBranch: r.GetDefaultBranch(),
				PushedAt:      r.GetPushedAt().Time,
			})
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opt.Page = resp.NextPage
	}
}

// File returns a file's content at ref. ErrNotFound means the file (or the repository) isn't there.
func (c *Client) File(ctx context.Context, installationID int64, repo, ref, path string) ([]byte, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return nil, err
	}
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	file, _, _, err := client.Repositories.GetContents(ctx, owner, name, path, &github.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		return nil, fmt.Errorf("get %s from %s: %w", path, repo, notFound(err))
	}
	if file == nil {
		return nil, fmt.Errorf("get %s from %s: %w: it's a folder", path, repo, ErrNotFound)
	}
	content, err := file.GetContent()
	if err != nil {
		return nil, fmt.Errorf("decode %s from %s: %w", path, repo, err)
	}
	return []byte(content), nil
}

// Head is the result of a conditional branch lookup.
type Head struct {
	SHA  string // empty when unchanged
	ETag string
}

// BranchHead returns the commit a branch points to. When etag matches what GitHub has, it returns a Head
// with an empty SHA: nothing changed, and the request didn't count against the rate limit.
func (c *Client) BranchHead(ctx context.Context, installationID int64, repo, branch, etag string) (Head, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return Head{}, err
	}
	owner, name, err := splitRepo(repo)
	if err != nil {
		return Head{}, err
	}
	u := fmt.Sprintf("repos/%s/%s/git/ref/heads/%s", url.PathEscape(owner), url.PathEscape(name), escapeBranch(branch))
	req, err := client.NewRequest(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Head{}, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	var ref github.Reference
	resp, err := client.Do(req, &ref)
	if resp != nil && resp.StatusCode == http.StatusNotModified {
		return Head{ETag: etag}, nil
	}
	if err != nil {
		return Head{}, fmt.Errorf("head of %s %s: %w", repo, branch, notFound(err))
	}
	return Head{SHA: ref.GetObject().GetSHA(), ETag: resp.Header.Get("ETag")}, nil
}

// escapeBranch escapes each segment of a branch name; branches may contain slashes.
func escapeBranch(branch string) string {
	parts := strings.Split(branch, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func splitRepo(repo string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return "", "", errors.New("repository name must be owner/repo: " + repo)
	}
	return owner, name, nil
}
