package knowledge

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/xaroth/speki/auth/access"
	"github.com/xaroth/speki/gh"
)

// clientTrailer is the commit trailer that names the MCP client a change came from.
const clientTrailer = "Speki-Client:"

// requestTrailer is the commit trailer that marks one commit attempt, so speki can find its commit when
// GitHub's answer is lost.
const requestTrailer = "Speki-Request:"

// How much of a commit history shows, in bytes. Speki writes short client names; commits made elsewhere
// may carry anything.
const (
	maxClient  = 64
	maxAuthor  = 100
	maxSubject = 300
)

// HistoryRequest asks for the latest commits of a vault or one document.
type HistoryRequest struct {
	Vault  string
	Path   string // "" for the whole vault
	Limit  int    // 1 to 50; 0 means 10
	Cursor string // NextCursor of the previous page
}

// HistoryPage is one page of commits, newest first.
type HistoryPage struct {
	Commits    []Commit
	NextCursor string // empty on the last page
}

// Commit is one change in a vault's history.
type Commit struct {
	SHA     string
	Author  string
	Date    time.Time
	Message string // the first line
	Client  string // the MCP client that made it through speki, from the Speki-Client trailer
}

// History returns one page of commits on the vault's default branch, newest first, as GitHub shows them
// to the user.
//
// A cursor names the branch's head when the first page was read and how many commits came before. Later
// pages list from that head, so commits made since don't shift them, and merged branches don't drop
// commits the way listing from the last commit shown would.
func (b *Base) History(ctx context.Context, g *access.Grant, req HistoryRequest) (HistoryPage, error) {
	if req.Limit < 0 || req.Limit > 50 {
		return HistoryPage{}, errorf(CodeInvalid, "limit must be between 1 and 50")
	}
	limit := req.Limit
	if limit == 0 {
		limit = 10
	}
	var head string
	var skip int
	if req.Cursor != "" {
		var ok bool
		head, skip, ok = parseHistoryCursor(req.Cursor)
		if !ok {
			return HistoryPage{}, invalidHistoryCursor()
		}
	}
	v, _, err := b.open(g, req.Vault)
	if err != nil {
		return HistoryPage{}, err
	}
	path := strings.TrimPrefix(req.Path, "/")
	// For a document, the first commit listed needn't be the head, so look the head up to page from it.
	if head == "" && path != "" {
		head, err = b.github.BranchTip(ctx, g.User().Token, v.Name, v.DefaultBranch)
		if err != nil {
			return HistoryPage{}, historyError(v.Name, err)
		}
	}
	from := head
	if from == "" {
		from = v.DefaultBranch
	}
	commits, err := b.github.Commits(ctx, g.User().Token, v.Name, from, path, skip, limit+1)
	if errors.Is(err, gh.ErrNotFound) && req.Cursor != "" {
		return HistoryPage{}, invalidHistoryCursor()
	}
	if err != nil {
		return HistoryPage{}, historyError(v.Name, err)
	}
	var out HistoryPage
	if len(commits) > limit {
		commits = commits[:limit]
		if head == "" {
			head = commits[0].SHA
		}
		out.NextCursor = head + ":" + strconv.Itoa(skip+limit)
	}
	out.Commits = make([]Commit, 0, len(commits))
	for _, c := range commits {
		subject, _, _ := strings.Cut(c.Message, "\n")
		out.Commits = append(out.Commits, Commit{
			SHA:     c.SHA,
			Author:  clip(c.Author, maxAuthor),
			Date:    c.Date,
			Message: clip(strings.TrimSpace(subject), maxSubject),
			Client:  client(c.Message),
		})
	}
	return out, nil
}

// historyError turns what GitHub said about a vault's history into an error for the user.
func historyError(vault string, err error) error {
	if errors.Is(err, gh.ErrNotFound) {
		return noVault(vault)
	}
	if errors.Is(err, gh.ErrUnauthorized) {
		return signedOut()
	}
	return err
}

func invalidHistoryCursor() *Error {
	return errorf(CodeInvalid, "the cursor isn't one history returned. Leave it out to start from the beginning")
}

// parseHistoryCursor splits a cursor, "head:skip", into a full commit SHA and a positive count.
func parseHistoryCursor(cursor string) (head string, skip int, ok bool) {
	head, count, found := strings.Cut(cursor, ":")
	if !found || (len(head) != 40 && len(head) != 64) || strings.Trim(head, "0123456789abcdef") != "" {
		return "", 0, false
	}
	n, err := strconv.ParseUint(count, 10, 31)
	if err != nil || n == 0 {
		return "", 0, false
	}
	return head, int(n), true
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
