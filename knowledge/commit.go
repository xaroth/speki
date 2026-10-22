package knowledge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/xaroth/speki/auth/access"
	"github.com/xaroth/speki/gh"
	"github.com/xaroth/speki/library/index"
	"github.com/xaroth/speki/vault"
)

const (
	// commitAttempts is how often a change is tried when the branch keeps moving under it.
	commitAttempts = 3
	// syncWait is how long a change waits for the index to catch up with its commit.
	syncWait = 10 * time.Second
	// writeLockWait is how long a change waits for the one before it on the same vault.
	writeLockWait = 15 * time.Second
	// maxHeadline is the longest commit headline speki writes, in bytes.
	maxHeadline = 200
	// noticeTimeout bounds telling other instances about a commit.
	noticeTimeout = 3 * time.Second
	// lookupRetry is how long speki waits before looking for a lost commit a second time.
	lookupRetry = 2 * time.Second
)

func rebuilding(name string) *Error {
	return errorf(CodeUnavailable, "%s is rebuilding its index. Try again in a minute", name)
}

// plan is a prepared commit.
type plan struct {
	changes    []gh.FileChange
	message    string // used when the agent gave none
	path       string // where the document is after the change
	source     string // for a move, where it was: the document a conflict is about
	version    string // its version there; empty when it's deleted
	linkedFrom []string
	base       index.Document // the existing document the change replaces, moves or deletes, if any
}

// change runs one change: it checks the user may write, takes the vault's write lock (waiting up to
// lockWait), prepares the commit against the index's head, and commits it. When the agent's version is
// stale it syncs once and looks again; when the branch moved, it syncs and starts over, up to
// commitAttempts times. After the commit it waits for the index to catch up, up to syncWait.
func (b *Base) change(ctx context.Context, g *access.Grant, name, message string, prepare func(context.Context, state) (plan, error)) (Result, error) {
	v, err := writable(g, name)
	if err != nil {
		return Result{}, err
	}
	lctx, cancel := context.WithTimeout(ctx, b.lockWait)
	unlock, err := b.writes.lock(lctx, v.RepoID)
	cancel()
	if err != nil {
		if ctx.Err() == nil { // the cap ran out, not the request
			return Result{}, errorf(CodeUnavailable, "another change to %s is still being committed. Try again in a few seconds", v.Name)
		}
		return Result{}, err
	}
	defer unlock()

	synced, moved := false, 0
	for {
		st, p, err := b.prepareWithSync(ctx, v, prepare, &synced)
		if err != nil {
			return Result{}, err
		}
		sha, err := b.commitOnce(ctx, g, v, st, p, message)
		if errors.Is(err, gh.ErrHeadMoved) {
			moved++
			err = b.headMoved(ctx, v, p, moved)
			if err != nil {
				return Result{}, err
			}
			continue
		}
		if err != nil {
			return Result{}, err
		}
		return b.afterCommit(ctx, v, p, sha), nil
	}
}

// prepareWithSync prepares the change against the vault's copy and checks the agent's version. When that
// fails in a way a stale copy explains, it syncs once and prepares again. synced carries across commit
// attempts, so a change syncs for this at most once.
func (b *Base) prepareWithSync(
	ctx context.Context,
	v vault.Vault,
	prepare func(context.Context, state) (plan, error),
	synced *bool,
) (state, plan, error) {
	for {
		c, ok := b.vaults.Copy(v.RepoID)
		if !ok {
			return state{}, plan{}, goneVault(v.Name)
		}
		st := state{vault: c.Info(), copy: c, head: c.Index().Commit()} // the live name and branch
		p, err := prepare(ctx, st)
		if err == nil {
			err = st.unchanged(p.base)
			if se, ok := errors.AsType[*staleError](err); ok {
				b.guardMismatch(ctx, st, p.base, se, *synced)
			}
		}
		if err == nil {
			return st, p, nil
		}
		if _, ok := errors.AsType[*Error](err); ok || ctx.Err() != nil {
			return state{}, plan{}, err
		}
		if *synced {
			return state{}, plan{}, staleConflict(err)
		}
		// A stale version, or a version or tree the copy no longer has: the copy may be behind
		// GitHub, or was just replaced. Sync once and look again.
		*synced = true
		serr := b.syncNow(ctx, v.RepoID)
		if errors.Is(serr, ErrRebuilding) {
			return state{}, plan{}, rebuilding(v.Name)
		}
		if serr != nil {
			b.logger.WarnContext(ctx, "sync before a change failed", "vault", v.Name, "err", serr)
			return state{}, plan{}, staleConflict(err)
		}
	}
}

// staleConflict returns the conflict a staleError carries, or err itself.
func staleConflict(err error) error {
	if se, ok := errors.AsType[*staleError](err); ok {
		return se.conflict
	}
	return err
}

// commitOnce commits p on GitHub and returns the new commit. It returns gh.ErrHeadMoved as is, for the
// caller to sync and start over.
func (b *Base) commitOnce(
	ctx context.Context,
	g *access.Grant,
	v vault.Vault,
	st state,
	p plan,
	message string,
) (string, error) {
	requestID := randomID()
	sha, err := b.github.Commit(ctx, g.User().Token, gh.CommitRequest{
		Repo:         st.vault.Name,
		Branch:       st.vault.DefaultBranch,
		ExpectedHead: st.head,
		Message:      commitMessage(message, p.message, g.User().Client) + "\n" + requestTrailer + " " + requestID,
		Changes:      p.changes,
	})
	if err == nil || errors.Is(err, gh.ErrHeadMoved) {
		return sha, err
	}
	cerr := commitError(st.vault.Name, err)
	if _, ok := errors.AsType[*Error](cerr); ok || errors.Is(err, gh.ErrRefused) {
		if _, ok := errors.AsType[*gh.SecretError](err); ok || errors.Is(err, gh.ErrRefused) || errors.Is(err, gh.ErrBranchRules) || errors.Is(err, gh.ErrForbidden) {
			// The agent gets a code; the operator needs GitHub's own words.
			b.logger.InfoContext(ctx, "GitHub refused a commit", "vault", v.Name, "err", err)
		}
		return "", cerr // GitHub answered: nothing landed
	}
	return b.recoverLostCommit(ctx, g, v, st, p, requestID, cerr)
}

// recoverLostCommit handles a commit whose answer was lost (a dropped connection, a timeout, a 5xx):
// GitHub may have made the commit anyway. It looks for it, so an agent doesn't retry an edit that would
// apply twice, and returns cerr when it isn't there.
func (b *Base) recoverLostCommit(
	ctx context.Context,
	g *access.Grant,
	v vault.Vault,
	st state,
	p plan,
	requestID string,
	cerr error,
) (string, error) {
	found, ok := b.findCommit(ctx, g, st.vault, p.path, requestID)
	if !ok {
		return "", cerr
	}
	b.logger.InfoContext(ctx, "a commit whose answer was lost landed", "vault", v.Name, "commit", found)
	return found, nil
}

// headMoved syncs after the branch moved under a commit for the moved'th time, and gives up at
// commitAttempts.
func (b *Base) headMoved(ctx context.Context, v vault.Vault, p plan, moved int) error {
	if moved == commitAttempts {
		about := p.path
		if p.source != "" {
			about = p.source
		}
		return b.keptChanging(ctx, v, about)
	}
	err := b.syncNow(ctx, v.RepoID)
	if errors.Is(err, ErrRebuilding) {
		return rebuilding(v.Name)
	}
	if err != nil {
		b.logger.WarnContext(ctx, "sync after a moved head failed", "vault", v.Name, "err", err)
	}
	return nil
}

// guardMismatch logs the index and git disagreeing on base at the change's head. Before a sync it can be
// the index applying a newer commit under the change; after one, it's an index bug the operator must see.
func (b *Base) guardMismatch(ctx context.Context, st state, base index.Document, se *staleError, synced bool) {
	args := []any{"vault", st.vault.Name, "path", base.Path, "commit", st.head, "index_version", base.Version, "git_version", se.conflict.Version}
	if !synced {
		b.logger.InfoContext(ctx, "index and git disagree on a document; syncing", args...)
		return
	}
	guardMismatches.Inc()
	b.logger.WarnContext(ctx, "index and git still disagree on a document after a sync", args...)
}

// syncNow syncs a vault for a change, within syncWait.
func (b *Base) syncNow(ctx context.Context, repoID int64) error {
	sctx, cancel := context.WithTimeout(ctx, syncWait)
	defer cancel()
	return b.vaults.Sync(sctx, repoID)
}

// afterCommit waits for the index to catch up with a landed commit and tells other instances about it.
// That work outlives the request, until Stop; the answer stops waiting for it when ctx ends, since the
// commit landed whatever happens next.
func (b *Base) afterCommit(ctx context.Context, v vault.Vault, p plan, sha string) Result {
	res := Result{Version: p.version, Commit: sha, LinkedFrom: p.linkedFrom, IndexPending: true}
	indexed := make(chan bool, 1)
	work, cancel := context.WithCancel(context.WithoutCancel(ctx)) // the request's values, Stop's end
	stop := context.AfterFunc(b.after, cancel)
	started := b.goAfter(func() {
		defer cancel()
		defer stop()
		err := b.syncNow(work, v.RepoID)
		if err != nil {
			b.logger.WarnContext(work, "sync after a commit failed", "vault", v.Name, "commit", sha, "err", err)
		}
		ok := err == nil && b.indexed(work, v.RepoID, p.path, p.version)
		nctx, ncancel := context.WithTimeout(context.WithoutCancel(work), noticeTimeout)
		defer ncancel()
		err = b.notices.Publish(nctx, v.RepoID, sha)
		if err != nil {
			b.logger.WarnContext(work, "telling other instances about a commit failed", "vault", v.Name, "err", err)
		}
		indexed <- ok
	})
	if !started { // speki is stopping: the pollers pick the commit up
		stop()
		cancel()
		return res
	}
	select {
	case ok := <-indexed:
		res.IndexPending = !ok
	case <-ctx.Done():
	}
	return res
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
		commits, err := b.github.Commits(fctx, g.User().Token, v.Name, v.DefaultBranch, path, 0, 5)
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
	secret, isSecret := errors.AsType[*gh.SecretError](err)
	limited, isLimited := errors.AsType[*gh.RateLimitError](err)
	switch {
	case isSecret:
		return errorf(CodeSecretDetected, "GitHub's push protection found a secret in the content (%s). Remove it, and tell the user", oneLine(secret.Detail))
	case isLimited:
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

// oneLine turns line breaks into spaces and drops control and format characters.
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
