package knowledge

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/eve-online-tools/speki/access"
	"github.com/eve-online-tools/speki/gh"
)

// clientTrailer is the commit trailer that names the MCP client a change came from.
const clientTrailer = "Speki-Client:"

// requestTrailer is the commit trailer that marks one commit attempt, so speki can find its commit when
// GitHub's answer is lost.
const requestTrailer = "Speki-Request:"

// maxClient is the longest client name history shows, in bytes. Speki writes shorter ones; commits
// made elsewhere may carry anything.
const maxClient = 64

// How much of a commit's author name and subject history shows, in bytes.
const (
	maxAuthor  = 100
	maxSubject = 300
)

// HistoryRequest asks for the latest commits of a vault or one document.
type HistoryRequest struct {
	Vault string
	Path  string // "" for the whole vault
	Limit int    // 1 to 50; 0 means 10
}

// Commit is one change in a vault's history.
type Commit struct {
	SHA     string
	Author  string
	Date    time.Time
	Message string // the first line
	Client  string // the MCP client that made it through speki, from the Speki-Client trailer
}

// History returns the newest commits on the vault's default branch, as GitHub shows them to the user.
func (b *Base) History(ctx context.Context, g *access.Grant, req HistoryRequest) ([]Commit, error) {
	if req.Limit < 0 || req.Limit > 50 {
		return nil, errorf(Invalid, "limit must be between 1 and 50")
	}
	limit := req.Limit
	if limit == 0 {
		limit = 10
	}
	v, _, err := b.open(g, req.Vault)
	if err != nil {
		return nil, err
	}
	path := strings.TrimPrefix(req.Path, "/")
	commits, err := b.github.Commits(ctx, g.User().Token, v.Name, v.DefaultBranch, path, limit)
	if errors.Is(err, gh.ErrNotFound) {
		return nil, noVault(v.Name)
	}
	if errors.Is(err, gh.ErrUnauthorized) {
		return nil, errorf(Unavailable, "GitHub no longer accepts your sign-in. Ask the user to reconnect speki")
	}
	if err != nil {
		return nil, err
	}
	out := make([]Commit, 0, len(commits))
	for _, c := range commits {
		subject, _, _ := strings.Cut(c.Message, "\n")
		out = append(out, Commit{
			SHA:     c.SHA,
			Author:  clip(c.Author, maxAuthor),
			Date:    c.Date,
			Message: clip(strings.TrimSpace(subject), maxSubject),
			Client:  client(c.Message),
		})
	}
	return out, nil
}

// client returns the value of the last Speki-Client trailer in a commit message, cut to maxClient.
func client(message string) string {
	return cutUTF8(trailer(message, clientTrailer), maxClient)
}

// trailer returns the value of the last trailer named key ("Key:") in a commit message.
func trailer(message, key string) string {
	lines := strings.Split(strings.TrimRight(message, "\n"), "\n")
	for i := len(lines) - 1; i > 0; i-- {
		if v, ok := strings.CutPrefix(lines[i], key); ok {
			return strings.TrimSpace(v)
		}
		if strings.TrimSpace(lines[i]) == "" {
			break // trailers are the last paragraph
		}
	}
	return ""
}
