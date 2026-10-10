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

// Why GitHub refused a commit.
var (
	// ErrHeadMoved means the branch no longer points at the expected head.
	ErrHeadMoved = errors.New("the branch moved")
	// ErrBranchRules means branch protection or a ruleset doesn't allow direct commits.
	ErrBranchRules = errors.New("the branch's rules don't allow this commit")
	// ErrForbidden means the user can't push to the repository.
	ErrForbidden = errors.New("no permission to push")
)

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
	var rl *github.RateLimitError
	if errors.As(err, &rl) {
		return &RateLimitError{RetryAfter: max(time.Until(rl.Rate.Reset.Time), time.Second)}
	}
	var abuse *github.AbuseRateLimitError
	if errors.As(err, &abuse) {
		return &RateLimitError{RetryAfter: max(abuse.GetRetryAfter(), time.Second)}
	}
	var gerr *github.ErrorResponse
	if errors.As(err, &gerr) && gerr.Response != nil {
		switch gerr.Response.StatusCode {
		case http.StatusForbidden, http.StatusNotFound:
			return fmt.Errorf("%w: %w", ErrForbidden, err)
		}
	}
	return userError(err)
}

// graphqlErrors maps the errors createCommitOnBranch reports in its response body. GitHub documents
// the types but not the messages, so types are trusted first and messages matched loosely after.
func graphqlErrors(errs []graphqlError) error {
	for _, e := range errs {
		switch e.Type {
		case "STALE_DATA":
			return fmt.Errorf("%w: %s", ErrHeadMoved, e.Message)
		case "RATE_LIMITED":
			return &RateLimitError{RetryAfter: time.Minute}
		case "FORBIDDEN", "NOT_FOUND":
			return fmt.Errorf("%w: %s", ErrForbidden, e.Message)
		}
	}
	for _, e := range errs {
		msg := strings.ToLower(e.Message)
		switch {
		case strings.Contains(msg, "expected branch to point to"):
			return fmt.Errorf("%w: %s", ErrHeadMoved, e.Message)
		case strings.Contains(msg, "push protection") || strings.Contains(msg, "secret scanning") || strings.Contains(msg, "cannot contain secrets"):
			return &SecretError{Detail: e.Message}
		case strings.Contains(msg, "rule violation") || strings.Contains(msg, "protected branch"):
			return fmt.Errorf("%w: %s", ErrBranchRules, e.Message)
		case strings.Contains(msg, "not accessible"):
			return fmt.Errorf("%w: %s", ErrForbidden, e.Message)
		}
	}
	return fmt.Errorf("GitHub: %s (%s)", errs[0].Message, errs[0].Type)
}
