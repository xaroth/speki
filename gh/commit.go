package gh

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-github/v88/github"
)

// ErrHeadMoved means the branch no longer points at the expected head.
var ErrHeadMoved = errors.New("the branch moved")

// ErrBranchRules means branch protection or a ruleset doesn't allow direct commits.
var ErrBranchRules = errors.New("the branch's rules don't allow this commit")

// ErrForbidden means the user can't push to the repository.
var ErrForbidden = errors.New("no permission to push")

// ErrRefused means GitHub answered and refused the commit, for a reason none of the others name.
var ErrRefused = errors.New("GitHub refused the commit")

// SecretError means GitHub's push protection found a secret in the content.
type SecretError struct {
	Detail string // GitHub's description of what it found
}

func (e *SecretError) Error() string { return "push protection found a secret: " + e.Detail }

// RateLimitError means GitHub asked to slow down.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("GitHub rate limit: retry in %s", e.RetryAfter.Round(time.Second))
}

// FileChange adds, replaces or deletes one file.
type FileChange struct {
	Path    string
	Content []byte // nil deletes the path
}

// CommitRequest is one commit to make on a branch.
type CommitRequest struct {
	Repo         string // owner/repo
	Branch       string
	ExpectedHead string
	Message      string // first line is the headline
	Changes      []FileChange
}

const createCommitMutation = `mutation($input: CreateCommitOnBranchInput!) {
  createCommitOnBranch(input: $input) { commit { oid } }
}`

type graphqlError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Commit makes one commit as the token's user with GraphQL createCommitOnBranch, which only succeeds
// while the branch points at ExpectedHead, and returns the new commit's SHA.
func (c *Client) Commit(ctx context.Context, token string, req CommitRequest) (string, error) {
	client, err := c.userClient(token)
	if err != nil {
		return "", err
	}
	type addition struct {
		Path     string `json:"path"`
		Contents string `json:"contents"` // always sent: an empty file is "" not missing
	}
	type deletion struct {
		Path string `json:"path"`
	}
	additions, deletions := []addition{}, []deletion{}
	for _, ch := range req.Changes {
		if ch.Content == nil {
			deletions = append(deletions, deletion{Path: ch.Path})
		} else {
			additions = append(additions, addition{Path: ch.Path, Contents: base64.StdEncoding.EncodeToString(ch.Content)})
		}
	}
	headline, body, _ := strings.Cut(req.Message, "\n")
	input := map[string]any{
		"branch":          map[string]string{"repositoryNameWithOwner": req.Repo, "branchName": req.Branch},
		"expectedHeadOid": req.ExpectedHead,
		"message":         map[string]string{"headline": headline, "body": strings.TrimLeft(body, "\n")},
		"fileChanges":     map[string]any{"additions": additions, "deletions": deletions},
	}
	hreq, err := client.NewRequest(ctx, http.MethodPost, "graphql", map[string]any{
		"query":     createCommitMutation,
		"variables": map[string]any{"input": input},
	})
	if err != nil {
		return "", err
	}
	var resp struct {
		Data struct {
			CreateCommitOnBranch *struct {
				Commit struct {
					OID string `json:"oid"`
				} `json:"commit"`
			} `json:"createCommitOnBranch"`
		} `json:"data"`
		Errors []graphqlError `json:"errors"`
	}
	_, err = client.Do(hreq, &resp)
	if err != nil {
		return "", fmt.Errorf("commit to %s: %w", req.Repo, commitError(err))
	}
	if len(resp.Errors) > 0 {
		return "", fmt.Errorf("commit to %s: %w", req.Repo, graphqlErrors(resp.Errors))
	}
	if resp.Data.CreateCommitOnBranch == nil || resp.Data.CreateCommitOnBranch.Commit.OID == "" {
		return "", fmt.Errorf("commit to %s: GitHub returned no commit", req.Repo)
	}
	return resp.Data.CreateCommitOnBranch.Commit.OID, nil
}

// commitError maps HTTP-level failures.
func commitError(err error) error {
	if rl, ok := errors.AsType[*github.RateLimitError](err); ok {
		return &RateLimitError{RetryAfter: max(time.Until(rl.Rate.Reset.Time), time.Second)}
	}
	if abuse, ok := errors.AsType[*github.AbuseRateLimitError](err); ok {
		return &RateLimitError{RetryAfter: max(abuse.GetRetryAfter(), time.Second)}
	}
	if gerr, ok := errors.AsType[*github.ErrorResponse](err); ok && gerr.Response != nil {
		switch gerr.Response.StatusCode {
		case http.StatusForbidden, http.StatusNotFound:
			return fmt.Errorf("%w: %w", ErrForbidden, err)
		}
	}
	return userError(err)
}

// graphqlErrors maps the errors createCommitOnBranch reports in its response body. GitHub documents
// the types but not the messages. Messages are matched before FORBIDDEN, because push protection and
// rulesets come back as FORBIDDEN too (recorded in testdata/).
func graphqlErrors(errs []graphqlError) error {
	for _, e := range errs {
		switch e.Type {
		case "STALE_DATA":
			return fmt.Errorf("%w: %s", ErrHeadMoved, tidy(e.Message))
		case "RATE_LIMITED":
			return &RateLimitError{RetryAfter: time.Minute}
		}
	}
	for _, e := range errs {
		msg := strings.ToLower(e.Message)
		if strings.HasPrefix(msg, "repository rule violations found") {
			// The rest can quote names users chose, such as a status check's, so only GitHub's own
			// wording for a secret counts.
			if strings.Contains(msg, "secret detected in content") {
				return &SecretError{Detail: tidy(e.Message)}
			}
			return fmt.Errorf("%w: %s", ErrBranchRules, tidy(e.Message))
		}
		switch {
		case strings.Contains(msg, "expected branch to point to"):
			return fmt.Errorf("%w: %s", ErrHeadMoved, tidy(e.Message))
		case strings.Contains(msg, "push protection") || strings.Contains(msg, "secret scanning") ||
			strings.Contains(msg, "cannot contain secrets"):
			return &SecretError{Detail: tidy(e.Message)}
		case strings.Contains(msg, "rule violation") || strings.Contains(msg, "protected branch"):
			return fmt.Errorf("%w: %s", ErrBranchRules, tidy(e.Message))
		case strings.Contains(msg, "not accessible"):
			return fmt.Errorf("%w: %s", ErrForbidden, tidy(e.Message))
		case e.Type != "FORBIDDEN" && (strings.Contains(msg, "timeout") || strings.Contains(msg, "something went wrong")):
			// GitHub answers some timeouts this way, with HTTP 200; the commit may have landed.
			return fmt.Errorf("GitHub: %s (%s)", tidy(e.Message), e.Type)
		}
	}
	for _, e := range errs {
		if e.Type == "FORBIDDEN" || e.Type == "NOT_FOUND" {
			return fmt.Errorf("%w: %s", ErrForbidden, tidy(e.Message))
		}
	}
	return fmt.Errorf("%w: %s (%s)", ErrRefused, tidy(errs[0].Message), errs[0].Type)
}

// tidy folds GitHub's multi-line messages onto one line.
func tidy(msg string) string {
	return strings.Join(strings.Fields(msg), " ")
}

// Commit is one commit in a repository's history.
type Commit struct {
	SHA     string
	Author  string // GitHub login, or the git author name when the commit isn't linked to an account
	Date    time.Time
	Message string
}

// Commits returns the newest commits on branch, at most limit, as the token's user sees them. With a
// path, only commits that changed it.
func (c *Client) Commits(ctx context.Context, token, repo, branch, path string, limit int) ([]Commit, error) {
	client, err := c.userClient(token)
	if err != nil {
		return nil, err
	}
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	page, _, err := client.Repositories.ListCommits(ctx, owner, name, &github.CommitsListOptions{
		SHA:     branch,
		Path:    path,
		PerPage: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("commits of %s: %w", repo, userError(err))
	}
	out := make([]Commit, 0, len(page))
	for _, rc := range page {
		author := rc.GetAuthor().GetLogin()
		if author == "" {
			author = rc.GetCommit().GetAuthor().GetName()
		}
		out = append(out, Commit{
			SHA:     rc.GetSHA(),
			Author:  author,
			Date:    rc.GetCommit().GetAuthor().GetDate().Time,
			Message: rc.GetCommit().GetMessage(),
		})
	}
	return out, nil
}
