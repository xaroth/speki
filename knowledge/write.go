package knowledge

import (
	"context"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // git blob IDs are SHA-1
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/eve-online-tools/speki/access"
	"github.com/eve-online-tools/speki/document"
	"github.com/eve-online-tools/speki/gh"
	"github.com/eve-online-tools/speki/index"
	"github.com/eve-online-tools/speki/vault"
)

const (
	// commitAttempts is how often a change is tried when the branch keeps moving under it.
	commitAttempts = 3
	// syncWait is how long a change waits for the index to catch up with its commit.
	syncWait = 10 * time.Second
	// maxHeadline is the longest commit message speki writes, in bytes.
	maxHeadline = 200
	// noticeTimeout bounds telling other instances about a commit.
	noticeTimeout = 3 * time.Second
	// lookupRetry is how long speki waits before looking for a lost commit a second time.
	lookupRetry = 2 * time.Second
)

// Result is what a change did.
type Result struct {
	Version      string // the document's new version; empty after a delete
	Commit       string
	IndexPending bool     // the index hadn't caught up with the commit yet
	LinkedFrom   []string // documents linking to the old path, after a move or delete
}

// WriteRequest creates a document, or replaces one entirely.
type WriteRequest struct {
	Vault   string
	Path    string
	Content string
	Version string // the version being replaced; empty creates
	Message string
}

// EditRequest replaces exact text in a document.
type EditRequest struct {
	Vault      string
	Path       string
	OldText    string
	NewText    string
	ReplaceAll bool
	Version    string // optional
	Message    string
}

// MoveRequest moves a document to another path.
type MoveRequest struct {
	Vault   string
	From    string
	To      string
	Version string // of From
	Message string
}

// DeleteRequest deletes a document.
type DeleteRequest struct {
	Vault   string
	Path    string
	Version string
	Message string
}

// Write creates a document (without a version) or replaces one (with the version being replaced).
func (b *Base) Write(ctx context.Context, g *access.Grant, req WriteRequest) (Result, error) {
	path := strings.TrimPrefix(req.Path, "/")
	return b.change(ctx, g, req.Vault, req.Message, func(ctx context.Context, st state) (plan, error) {
		cur, exists, err := st.document(ctx, path)
		if err != nil {
			return plan{}, err
		}
		if req.Version == "" {
			if exists {
				return plan{}, &Error{
					Code:    CodeAlreadyExists,
					Message: fmt.Sprintf("%s already exists, at version %s. Read it and pass that version to replace it, or use edit to change part of it", cur.Path, cur.Version),
					Version: cur.Version,
				}
			}
			newPath, err := st.newPath(path, "")
			if err != nil {
				return plan{}, err
			}
			out, err := prepareDocument(newPath, nil, []byte(req.Content), b.now())
			if err != nil {
				return plan{}, err
			}
			return plan{
				changes: []gh.FileChange{{Path: newPath, Content: out}},
				message: "Create " + newPath,
				path:    newPath,
				version: blobID(out),
			}, nil
		}

		if !exists {
			return plan{}, errorf(CodeNotFound, "%s has no document %s. Leave out version to create it", st.vault.Name, path)
		}
		if cur.Version != req.Version {
			return plan{}, stale(cur)
		}
		raw, err := st.copy.Blob(cur.Version)
		if err != nil {
			return plan{}, err
		}
		out, err := prepareDocument(cur.Path, raw, []byte(req.Content), b.now())
		if err != nil {
			return plan{}, err
		}
		return plan{
			changes: []gh.FileChange{{Path: cur.Path, Content: out}},
			message: "Update " + cur.Path,
			path:    cur.Path,
			version: blobID(out),
		}, nil
	})
}

// Edit replaces OldText in a document, once unless ReplaceAll is set.
func (b *Base) Edit(ctx context.Context, g *access.Grant, req EditRequest) (Result, error) {
	if _, err := writable(g, req.Vault); err != nil {
		return Result{}, err
	}
	if req.OldText == "" {
		return Result{}, errorf(CodeInvalid, "old_text is empty. Give the exact text to replace")
	}
	path := strings.TrimPrefix(req.Path, "/")
	return b.change(ctx, g, req.Vault, req.Message, func(ctx context.Context, st state) (plan, error) {
		cur, exists, err := st.document(ctx, path)
		if err != nil {
			return plan{}, err
		}
		if !exists {
			return plan{}, errorf(CodeNotFound, "%s has no document %s. Use search or list to find it", st.vault.Name, path)
		}
		if req.Version != "" && cur.Version != req.Version {
			return plan{}, stale(cur)
		}
		raw, err := st.copy.Blob(cur.Version)
		if err != nil {
			return plan{}, err
		}
		text := string(raw)
		n := countOverlapping(text, req.OldText)
		switch {
		case n == 0:
			return plan{}, errorf(CodeInvalid, "old_text doesn't appear in %s. Read it again and copy the text exactly; whitespace and line endings count", cur.Path)
		case n > 1 && !req.ReplaceAll:
			return plan{}, errorf(CodeInvalid, "old_text appears %d times in %s. Include more of the surrounding text so it matches once, or set replace_all", n, cur.Path)
		}
		limit := 1
		if req.ReplaceAll {
			limit = -1
		}
		next := strings.Replace(text, req.OldText, req.NewText, limit)
		out, err := prepareDocument(cur.Path, raw, []byte(next), b.now())
		if err != nil {
			return plan{}, err
		}
		return plan{
			changes: []gh.FileChange{{Path: cur.Path, Content: out}},
			message: "Update " + cur.Path,
			path:    cur.Path,
			version: blobID(out),
		}, nil
	})
}

// Move moves a document in one commit. Links to it elsewhere aren't changed: the result lists the
// documents that link to the old path.
func (b *Base) Move(ctx context.Context, g *access.Grant, req MoveRequest) (Result, error) {
	if _, err := writable(g, req.Vault); err != nil {
		return Result{}, err
	}
	if req.Version == "" {
		return Result{}, errorf(CodeInvalid, "move needs the version of the document being moved. Read it first")
	}
	from := strings.TrimPrefix(req.From, "/")
	to := strings.TrimPrefix(req.To, "/")
	if from == to {
		return Result{}, errorf(CodeInvalid, "from and to are the same path")
	}
	return b.change(ctx, g, req.Vault, req.Message, func(ctx context.Context, st state) (plan, error) {
		cur, exists, err := st.document(ctx, from)
		if err != nil {
			return plan{}, err
		}
		if !exists {
			return plan{}, errorf(CodeNotFound, "%s has no document %s. Use search or list to find it", st.vault.Name, from)
		}
		if cur.Version != req.Version {
			return plan{}, stale(cur)
		}
		target, exists, err := st.document(ctx, to)
		if err != nil {
			return plan{}, err
		}
		if exists {
			return plan{}, &Error{
				Code:    CodeAlreadyExists,
				Message: fmt.Sprintf("%s already exists, at version %s. Pick another path, or delete that document first", target.Path, target.Version),
				Version: target.Version,
			}
		}
		newPath, err := st.newPath(to, cur.Path)
		if err != nil {
			return plan{}, err
		}
		if newPath == cur.Path { // to was the same path in another normalisation
			return plan{}, errorf(CodeInvalid, "%s and %s are the same path", from, to)
		}
		raw, err := st.copy.Blob(cur.Version)
		if err != nil {
			return plan{}, err
		}
		linked, err := st.copy.Index().LinkedFrom(ctx, cur.Path)
		if err != nil {
			return plan{}, err
		}
		return plan{
			changes:    []gh.FileChange{{Path: cur.Path}, {Path: newPath, Content: raw}},
			message:    "Move " + cur.Path + " to " + newPath,
			path:       newPath,
			source:     cur.Path,
			version:    cur.Version,
			linkedFrom: linked,
		}, nil
	})
}

// Delete deletes a document. It stays in git history.
func (b *Base) Delete(ctx context.Context, g *access.Grant, req DeleteRequest) (Result, error) {
	if _, err := writable(g, req.Vault); err != nil {
		return Result{}, err
	}
	if req.Version == "" {
		return Result{}, errorf(CodeInvalid, "delete needs the version of the document. Read it first")
	}
	path := strings.TrimPrefix(req.Path, "/")
	return b.change(ctx, g, req.Vault, req.Message, func(ctx context.Context, st state) (plan, error) {
		cur, exists, err := st.document(ctx, path)
		if err != nil {
			return plan{}, err
		}
		if !exists {
			return plan{}, errorf(CodeNotFound, "%s has no document %s. Use search or list to find it", st.vault.Name, path)
		}
		if cur.Version != req.Version {
			return plan{}, stale(cur)
		}
		linked, err := st.copy.Index().LinkedFrom(ctx, cur.Path)
		if err != nil {
			return plan{}, err
		}
		return plan{
			changes:    []gh.FileChange{{Path: cur.Path}},
			message:    "Delete " + cur.Path,
			path:       cur.Path,
			linkedFrom: linked,
		}, nil
	})
}

func rebuilding(name string) *Error {
	return errorf(CodeUnavailable, "%s is rebuilding its index. Try again in a minute", name)
}

// writable returns a vault the user may change, or why not. Every change checks this first, so the
// answer doesn't depend on whether the rest of the request is well formed.
func writable(g *access.Grant, name string) (vault.Vault, error) {
	v, ok := g.Vault(name)
	switch {
	case !ok:
		return vault.Vault{}, noVault(name)
	case g.ReadOnlySession():
		return vault.Vault{}, errorf(CodeReadOnlySession, "the user connected speki read-only. Ask them to reconnect it with write access")
	case !g.CanWrite(v.RepoID):
		return vault.Vault{}, errorf(CodeNoWriteAccess, "the user can read %s but can't push to it on GitHub. An owner of the repository can give them write access", v.Name)
	}
	return v, nil
}

// state is a vault as a change sees it: its copy at the head the commit will be based on.
type state struct {
	vault vault.Vault
	copy  Copy
	head  string
}

// document looks a document up in the index.
func (st state) document(ctx context.Context, path string) (index.Document, bool, error) {
	d, err := st.copy.Index().Document(ctx, path)
	if errors.Is(err, index.ErrNotFound) {
		return index.Document{}, false, nil
	}
	if err != nil {
		return index.Document{}, false, indexError(st.vault.Name, err)
	}
	return d, true, nil
}

// newPath checks a path for a new document against the git tree at head, and returns the path to
// write. moving is the path being moved away from, which doesn't count as a collision.
func (st state) newPath(path, moving string) (string, error) {
	paths, err := st.copy.Paths(st.head)
	if err != nil {
		return "", err
	}
	taken := make(map[string]string, len(paths)) // path -> what it is
	folded := make(map[string]string, len(paths))
	for _, p := range paths {
		if p == moving {
			continue
		}
		taken[p] = "a file"
		folded[document.Fold(p)] = p
		for i := strings.LastIndexByte(p, '/'); i > 0; i = strings.LastIndexByte(p[:i], '/') {
			taken[p[:i]] = "a folder"
			folded[document.Fold(p[:i])] = p[:i]
		}
	}
	checked, err := document.CheckPath(path, func(f string) (string, bool) {
		p, ok := folded[f]
		return p, ok
	})
	if errors.Is(err, document.ErrExists) {
		return "", errorf(CodeAlreadyExists, "%s (stored with a different Unicode encoding of the same name). Pick another path", err)
	}
	if err != nil {
		return "", errorf(CodeInvalid, "%s", err)
	}
	for i := strings.IndexByte(checked, '/'); i > 0; i = next(checked, i) {
		if taken[checked[:i]] == "a file" {
			return "", errorf(CodeAlreadyExists, "%s is a file in the vault, so nothing can go under it. Pick another path", checked[:i])
		}
	}
	switch taken[checked] {
	case "a file":
		return "", errorf(CodeAlreadyExists, "%s exists in the vault but isn't a document speki reads (it may be over 1 MiB). Pick another path", checked)
	case "a folder":
		return "", errorf(CodeAlreadyExists, "%s is a folder in the vault. Pick another path", checked)
	}
	return checked, nil
}

// countOverlapping counts the places sub appears in s, overlapping ones too: "ab ab" appears twice in
// "ab ab ab", and an edit of it would be ambiguous.
func countOverlapping(s, sub string) int {
	n := 0
	for i := 0; ; i++ {
		j := strings.Index(s[i:], sub)
		if j < 0 {
			return n
		}
		n++
		i += j
	}
}

// next returns the index of the next "/" in p after i, or -1.
func next(p string, i int) int {
	j := strings.IndexByte(p[i+1:], '/')
	if j < 0 {
		return -1
	}
	return i + 1 + j
}

// prepareDocument checks the new content of a document against its current content (nil for a new
// one) and sets its dates. created is speki's: now for a new document, and kept from the current one
// when the agent's content leaves it out.
func prepareDocument(path string, current, next []byte, now time.Time) ([]byte, error) {
	var cur *document.Document
	created := now
	if current != nil {
		created = time.Time{}
		var err error
		cur, err = document.Parse(path, current)
		if err != nil {
			cur = nil // the current file is unusable; any valid content replaces it
		}
		if cur != nil {
			created = cur.Meta.Created
		}
	}
	d, err := document.Parse(path, next)
	if err != nil {
		return nil, errorf(CodeInvalid, "the content can't be used: %s", err)
	}
	err = document.Check(cur, d)
	if err != nil {
		return nil, documentError(err)
	}
	out, err := d.Touch(now, created)
	if err != nil {
		return nil, documentError(err)
	}
	return out, nil
}

func documentError(err error) *Error {
	msg := strings.ReplaceAll(err.Error(), "\n", "; ")
	if errors.Is(err, document.ErrTooLarge) {
		return errorf(CodeTooLarge, "%s. Split the document into smaller ones", msg)
	}
	return errorf(CodeInvalid, "%s", msg)
}

// staleError means the document changed since the version the agent gave.
type staleError struct{ conflict *Error }

func (e *staleError) Error() string { return e.conflict.Error() }

func stale(cur index.Document) error {
	return &staleError{conflict: &Error{
		Code:    CodeConflict,
		Message: fmt.Sprintf("%s changed since you read it. Current version: %s. Read it again and redo your change", cur.Path, cur.Version),
		Version: cur.Version,
	}}
}

// plan is a prepared commit.
type plan struct {
	changes    []gh.FileChange
	message    string // used when the agent gave none
	path       string // where the document is after the change
	source     string // for a move, where it was: the document a conflict is about
	version    string // its version there; empty when it's deleted
	linkedFrom []string
}

// change runs one change: it checks the user may write, takes the vault's write lock, prepares the commit
// against the index's head, and commits it. When the agent's version is stale it syncs once and looks
// again; when the branch moved, it syncs and starts over, up to commitAttempts times. After the commit it
// waits for the index to catch up, up to syncWait.
func (b *Base) change(ctx context.Context, g *access.Grant, name, message string, prepare func(context.Context, state) (plan, error)) (Result, error) {
	v, err := writable(g, name)
	if err != nil {
		return Result{}, err
	}
	unlock, err := b.lock(ctx, v.RepoID)
	if err != nil {
		return Result{}, err
	}
	defer unlock()

	synced, moved := false, 0
	for {
		c, ok := b.vaults.Copy(v.RepoID)
		if !ok {
			return Result{}, noVault(name)
		}
		st := state{vault: c.Info(), copy: c, head: c.Index().Commit()} // the live name and branch
		p, err := prepare(ctx, st)
		if err != nil {
			var se *staleError
			var kerr *Error
			if errors.As(err, &kerr) || ctx.Err() != nil {
				return Result{}, err
			}
			if synced {
				if errors.As(err, &se) {
					return Result{}, se.conflict
				}
				return Result{}, err
			}
			// A stale version, or a version or tree the copy no longer has: the copy may be behind
			// GitHub, or was just replaced. Sync once and look again.
			synced = true
			sctx, cancel := context.WithTimeout(ctx, syncWait) // a write waits for a sync this long at most
			serr := b.vaults.Sync(sctx, v.RepoID)
			cancel()
			if errors.Is(serr, ErrRebuilding) {
				return Result{}, rebuilding(v.Name)
			}
			if serr != nil {
				b.logger.WarnContext(ctx, "sync before a change failed", "vault", v.Name, "err", serr)
				if errors.As(err, &se) {
					return Result{}, se.conflict
				}
				return Result{}, err
			}
			continue
		}

		requestID := randomID()
		sha, err := b.github.Commit(ctx, g.User().Token, gh.CommitRequest{
			Repo:         st.vault.Name,
			Branch:       st.vault.DefaultBranch,
			ExpectedHead: st.head,
			Message:      commitMessage(message, p.message, g.User().Client) + "\n" + requestTrailer + " " + requestID,
			Changes:      p.changes,
		})
		if errors.Is(err, gh.ErrHeadMoved) {
			moved++
			if moved == commitAttempts {
				about := p.path
				if p.source != "" {
					about = p.source
				}
				return Result{}, b.keptChanging(ctx, v, about)
			}
			sctx, cancel := context.WithTimeout(ctx, syncWait)
			err = b.vaults.Sync(sctx, v.RepoID)
			cancel()
			if errors.Is(err, ErrRebuilding) {
				return Result{}, rebuilding(v.Name)
			}
			if err != nil {
				b.logger.WarnContext(ctx, "sync after a moved head failed", "vault", v.Name, "err", err)
			}
			continue
		}
		if err != nil {
			cerr := commitError(st.vault.Name, err)
			var kerr *Error
			if errors.As(cerr, &kerr) || errors.Is(err, gh.ErrRefused) {
				var secret *gh.SecretError
				if errors.As(err, &secret) || errors.Is(err, gh.ErrRefused) || errors.Is(err, gh.ErrBranchRules) || errors.Is(err, gh.ErrForbidden) {
					// The agent gets a code; the operator needs GitHub's own words.
					b.logger.InfoContext(ctx, "GitHub refused a commit", "vault", v.Name, "err", err)
				}
				return Result{}, cerr // GitHub answered: nothing landed
			}
			// The answer was lost (a dropped connection, a timeout, a 5xx): GitHub may have made the
			// commit anyway. Look for it, so an agent doesn't retry an edit that would apply twice.
			found, ok := b.findCommit(ctx, g, st.vault, p.path, requestID)
			if !ok {
				return Result{}, cerr
			}
			b.logger.InfoContext(ctx, "a commit whose answer was lost landed", "vault", v.Name, "commit", found)
			sha = found
		}

		res := Result{Version: p.version, Commit: sha, LinkedFrom: p.linkedFrom}
		// The commit landed: finish the job even if the client went away, within syncWait.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), syncWait)
		err = b.vaults.Sync(sctx, v.RepoID)
		cancel()
		if err != nil {
			b.logger.WarnContext(ctx, "sync after a commit failed", "vault", v.Name, "commit", sha, "err", err)
		}
		res.IndexPending = err != nil || !b.indexed(context.WithoutCancel(ctx), v.RepoID, p.path, p.version)
		nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), noticeTimeout) // the commit landed: tell others
		err = b.notices.Publish(nctx, v.RepoID, sha)
		cancel()
		if err != nil {
			b.logger.WarnContext(ctx, "telling other instances about a commit failed", "vault", v.Name, "err", err)
		}
		return res, nil
	}
}

// findCommit looks on GitHub, as the user, for the commit carrying requestID among the latest ones
// that touched path, and returns its SHA. It asks GitHub rather than the local copy, so a sync holding
// the vault's lock can't delay the answer.
func (b *Base) findCommit(ctx context.Context, g *access.Grant, v vault.Vault, path, requestID string) (string, bool) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), syncWait)
	defer cancel()
	for attempt := range 2 {
		if attempt > 0 { // GitHub may still be finishing the commit, or its listing may lag
			select {
			case <-fctx.Done():
				return "", false
			case <-time.After(lookupRetry):
			}
		}
		commits, err := b.github.Commits(fctx, g.User().Token, v.Name, v.DefaultBranch, path, 5)
		if err != nil {
			continue
		}
		for _, c := range commits {
			if trailer(c.Message, requestTrailer) == requestID {
				return c.SHA, true
			}
		}
	}
	return "", false
}

// randomID returns a value unique to one commit attempt.
func randomID() string {
	var b [12]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b[:])
}

// keptChanging is the conflict for a vault whose branch moved under every attempt. It carries the
// document's current version, as every conflict does, when speki has it.
func (b *Base) keptChanging(ctx context.Context, v vault.Vault, path string) *Error {
	e := errorf(CodeConflict, "%s kept changing while speki tried to commit. Read the document again and redo your change", v.Name)
	if c, ok := b.vaults.Copy(v.RepoID); ok {
		if d, err := c.Index().Document(ctx, path); err == nil {
			e.Version = d.Version
			e.Message = fmt.Sprintf("%s kept changing while speki tried to commit. Current version of %s: %s. Read it again and redo your change", v.Name, d.Path, d.Version)
		}
	}
	return e
}

// indexed reports whether the index has the document at path in version, or not at all when version is
// empty.
func (b *Base) indexed(ctx context.Context, repoID int64, path, version string) bool {
	c, ok := b.vaults.Copy(repoID)
	if !ok {
		return false
	}
	d, err := c.Index().Document(ctx, path)
	if version == "" {
		return errors.Is(err, index.ErrNotFound)
	}
	return err == nil && d.Version == version
}

// commitError turns GitHub's refusal of a commit into an error the agent can act on.
func commitError(name string, err error) error {
	var secret *gh.SecretError
	var limited *gh.RateLimitError
	switch {
	case errors.As(err, &secret):
		return errorf(CodeSecretDetected, "GitHub's push protection found a secret in the content (%s). Remove it, and tell the user", oneLine(secret.Detail))
	case errors.As(err, &limited):
		return errorf(CodeRateLimited, "GitHub asked speki to slow down. Wait %d seconds and try again", int(limited.RetryAfter.Seconds()))
	case errors.Is(err, gh.ErrBranchRules):
		return errorf(CodeVaultReadOnly, "GitHub refused the commit: %s only accepts changes through pull requests or checks. Tell the user", name)
	case errors.Is(err, gh.ErrForbidden):
		return errorf(CodeNoWriteAccess, "GitHub refused the commit: the user can't push to %s", name)
	case errors.Is(err, gh.ErrUnauthorized):
		return signedOut()
	}
	return err
}

// commitMessage returns the commit message: the agent's first line (or fallback), cut to maxHeadline
// bytes, and a trailer naming the client.
func commitMessage(message, fallback, client string) string {
	clean := func(s string) string {
		line := strings.TrimSpace(oneLine(s))
		if strings.HasPrefix(strings.ToLower(line), strings.ToLower(clientTrailer)) {
			return ""
		}
		return cutUTF8(line, maxHeadline)
	}
	first, _, _ := strings.Cut(message, "\n")
	line := clean(first)
	if line == "" {
		line = clean(fallback) // the whole of it: paths come from git, which allows line breaks in names
	}
	if line == "" {
		line = "Update"
	}
	client = cutUTF8(strings.TrimSpace(oneLine(client)), maxClient)
	if client == "" {
		client = "unknown"
	}
	return line + "\n\n" + clientTrailer + " " + client
}

// oneLine drops line breaks and control and format characters.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t' || unicode.In(r, unicode.Zl, unicode.Zp):
			return ' '
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
}

// blobID returns the git blob SHA of content: its version.
func blobID(content []byte) string {
	h := sha1.New() //nolint:gosec // git blob IDs are SHA-1
	h.Write([]byte("blob " + strconv.Itoa(len(content)) + "\x00"))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}
