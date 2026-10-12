package gh

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/go-github/v88/github"
)

// ErrUnauthorized means GitHub rejected the user's token: it expired or was revoked.
var ErrUnauthorized = errors.New("GitHub rejected the user's token")

// User is a GitHub account.
type User struct {
	ID    int64
	Login string
}

// RepoAccess is what a user may do with one repository.
type RepoAccess struct {
	RepoID int64
	Role   Role
}

func (c *Client) userClient(token string) (*github.Client, error) {
	return newGitHub(c.apiURL, c.metrics.observe("user", authTransport{token: token, next: c.base}))
}

type authTransport struct {
	token string
	next  http.RoundTripper
}

func (a authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+a.token)
	return a.next.RoundTrip(r)
}

func userError(err error) error {
	if gerr, ok := errors.AsType[*github.ErrorResponse](err); ok && gerr.Response != nil && gerr.Response.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%w: %w", ErrUnauthorized, err)
	}
	return notFound(err)
}

// Me returns the user a token belongs to.
func (c *Client) Me(ctx context.Context, token string) (User, error) {
	client, err := c.userClient(token)
	if err != nil {
		return User{}, err
	}
	u, _, err := client.Users.Get(ctx, "")
	if err != nil {
		return User{}, fmt.Errorf("get user: %w", userError(err))
	}
	return User{ID: u.GetID(), Login: u.GetLogin()}, nil
}

// ActiveMember reports whether the token's user is an active member of org. Pending invitations don't
// count.
func (c *Client) ActiveMember(ctx context.Context, token, org string) (bool, error) {
	client, err := c.userClient(token)
	if err != nil {
		return false, err
	}
	m, _, err := client.Organizations.GetOrgMembership(ctx, "", org)
	if errors.Is(notFound(err), ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("membership of %s: %w", org, userError(err))
	}
	return m.GetState() == "active", nil
}

// TeamMember reports whether login is an active member of the team org/slug, directly or through a child
// team, as the token's user sees it. Teams the user can't see count as not a member.
func (c *Client) TeamMember(ctx context.Context, token, org, slug, login string) (bool, error) {
	client, err := c.userClient(token)
	if err != nil {
		return false, err
	}
	m, _, err := client.Teams.GetTeamMembershipBySlug(ctx, org, slug, login)
	if errors.Is(notFound(err), ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("membership of %s/%s: %w", org, slug, userError(err))
	}
	return m.GetState() == "active", nil
}

// UserRepositories returns what the token's user may do in each repository the app's installations can
// see, keyed by installation.
func (c *Client) UserRepositories(ctx context.Context, token string) ([]RepoAccess, error) {
	client, err := c.userClient(token)
	if err != nil {
		return nil, err
	}
	var installations []int64
	opt := &github.ListOptions{PerPage: 100}
	for {
		page, resp, err := client.Apps.ListUserInstallations(ctx, opt)
		if err != nil {
			return nil, fmt.Errorf("user installations: %w", userError(err))
		}
		for _, in := range page {
			installations = append(installations, in.GetID())
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}

	var out []RepoAccess
	for _, id := range installations {
		opt := &github.ListOptions{PerPage: 100}
		for {
			page, resp, err := client.Apps.ListUserRepos(ctx, id, opt)
			if err != nil {
				return nil, fmt.Errorf("repositories of installation %d: %w", id, userError(err))
			}
			for _, r := range page.Repositories {
				out = append(out, RepoAccess{RepoID: r.GetID(), Role: roleOf(r.GetPermissions())})
			}
			if resp.NextPage == 0 {
				break
			}
			opt.Page = resp.NextPage
		}
	}
	return out, nil
}
