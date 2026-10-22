// Package ghcache shares the GitHub reads speki polls with between instances, through the store: one
// instance asks GitHub, and the others use its answer until it's due again. Several instances then ask
// GitHub about as often as one does.
package ghcache

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/xaroth/speki/gh"
)

const (
	// keyPrefix is versioned: instances of different versions mustn't read each other's answers.
	keyPrefix = "gh:v1:"
	// lockTime is how long one instance may fetch an answer before another may too.
	lockTime = 30 * time.Second
	// keepTime is how long the store keeps an answer at least. Old answers still carry the ETag that
	// keeps asking about a branch free.
	keepTime = time.Hour
)

var sharedReads = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "speki_github_shared_reads_total",
	Help: "GitHub reads shared between instances, by call and result: hit (another instance's answer), fetched (asked GitHub and shared), stale (another instance was asking; an older answer was used) or direct (nothing to share; asked GitHub).",
}, []string{"call", "result"})

// github is the GitHub API the pollers read.
type github interface {
	Installations(ctx context.Context) ([]gh.Installation, error)
	Repositories(ctx context.Context, installationID int64) ([]gh.Repository, error)
	BranchHead(ctx context.Context, installationID int64, repo, branch, etag string) (gh.Head, error)
}

// kv is the store the answers are shared through.
type kv interface {
	SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Release(ctx context.Context, key string, value []byte) error // deletes key while it holds value
}

// Options say how long an answer is used before GitHub is asked again. Set each a little below the
// interval of the loop that reads it, so every round of that loop asks GitHub once across instances.
type Options struct {
	Heads     time.Duration // branch heads, read by the poller
	Discovery time.Duration // installations and repositories, read by discovery
}

// Client reads GitHub through answers shared in a store. A store that fails only means asking GitHub
// directly.
type Client struct {
	next github
	kv   kv
	opts Options
	now  func() time.Time
}

// New returns a Client that asks next when no instance has a recent enough answer.
func New(next github, kv kv, opts Options) *Client {
	return &Client{next: next, kv: kv, opts: opts, now: time.Now}
}

// Installations lists every installation of the app, as gh.Client.Installations does.
func (c *Client) Installations(ctx context.Context) ([]gh.Installation, error) {
	out, ok, err := share(ctx, c, "installations", "installations", c.opts.Discovery, func(*[]gh.Installation) ([]gh.Installation, error) {
		return c.next.Installations(ctx)
	})
	if !ok {
		return c.next.Installations(ctx)
	}
	return out, err
}

// Repositories lists the repositories an installation can see, as gh.Client.Repositories does.
func (c *Client) Repositories(ctx context.Context, installationID int64) ([]gh.Repository, error) {
	key := "repositories:" + strconv.FormatInt(installationID, 10)
	out, ok, err := share(ctx, c, "repositories", key, c.opts.Discovery, func(*[]gh.Repository) ([]gh.Repository, error) {
		return c.next.Repositories(ctx, installationID)
	})
	if !ok {
		return c.next.Repositories(ctx, installationID)
	}
	return out, err
}

// BranchHead returns the commit a branch points to, as gh.Client.BranchHead does: an empty SHA when
// etag is still current. The instance that asks GitHub sends the ETag of the shared answer, so asking
// stays free while the branch doesn't move.
func (c *Client) BranchHead(ctx context.Context, installationID int64, repo, branch, etag string) (gh.Head, error) {
	head, ok, err := share(ctx, c, "branch_head", "head:"+repo+":"+branch, c.opts.Heads, func(prev *gh.Head) (gh.Head, error) {
		known := ""
		if prev != nil {
			known = prev.ETag
		}
		h, err := c.next.BranchHead(ctx, installationID, repo, branch, known)
		if err != nil || h.SHA != "" {
			return h, err
		}
		return *prev, nil // unchanged since prev
	})
	if !ok {
		return c.next.BranchHead(ctx, installationID, repo, branch, etag)
	}
	if err != nil {
		return gh.Head{}, err
	}
	if etag != "" && head.ETag == etag {
		return gh.Head{ETag: etag}, nil
	}
	return head, nil
}

// entry is an answer as the store keeps it. A not-found answer is definite, so it's shared too; other
// errors aren't.
type entry[T any] struct {
	At       time.Time `json:"at"`
	Value    T         `json:"value"`
	NotFound string    `json:"not_found,omitempty"` // the error's text
}

// share answers from the store while its answer is younger than maxAge. Otherwise it takes the key's
// lock, calls fetch with the previous answer, if any, and stores the result; while another instance
// holds the lock, the previous answer is used. ok is false when there's nothing to share: the store
// failed, or another instance is fetching an answer the store doesn't have yet. The caller then asks
// GitHub itself.
func share[T any](ctx context.Context, c *Client, call, key string, maxAge time.Duration, fetch func(prev *T) (T, error)) (v T, ok bool, err error) {
	key = keyPrefix + key
	prev, err := load[T](ctx, c.kv, key)
	if err != nil {
		sharedReads.WithLabelValues(call, "direct").Inc()
		return v, false, nil
	}
	if prev != nil && c.now().Sub(prev.At) < maxAge {
		sharedReads.WithLabelValues(call, "hit").Inc()
		v, err = prev.answer()
		return v, true, err
	}

	owner := []byte(rand.Text())
	locked, err := c.kv.SetNX(ctx, key+":lock", owner, lockTime)
	switch {
	case err != nil || (!locked && prev == nil):
		sharedReads.WithLabelValues(call, "direct").Inc()
		return v, false, nil
	case !locked:
		sharedReads.WithLabelValues(call, "stale").Inc()
		v, err = prev.answer()
		return v, true, err
	}
	defer func() { _ = c.kv.Release(context.WithoutCancel(ctx), key+":lock", owner) }()

	// Another instance may have stored an answer between our look and our lock.
	latest, err := load[T](ctx, c.kv, key)
	if err == nil && latest != nil && c.now().Sub(latest.At) < maxAge {
		sharedReads.WithLabelValues(call, "hit").Inc()
		v, err = latest.answer()
		return v, true, err
	}

	var prevValue *T
	if prev != nil && prev.NotFound == "" {
		prevValue = &prev.Value
	}
	e := entry[T]{At: c.now()}
	e.Value, err = fetch(prevValue)
	if errors.Is(err, gh.ErrNotFound) {
		e.NotFound = err.Error()
	} else if err != nil {
		return v, true, err // not shared: the next reader asks again
	}
	raw, merr := json.Marshal(e)
	if merr == nil {
		_ = c.kv.Set(ctx, key, raw, max(keepTime, 2*maxAge)) // unshared, the next reader asks GitHub
	}
	sharedReads.WithLabelValues(call, "fetched").Inc()
	return e.Value, true, err
}

// load returns key's entry, or nil when the store has none.
func load[T any](ctx context.Context, kv kv, key string) (*entry[T], error) {
	raw, found, err := kv.Get(ctx, key)
	if err != nil || !found {
		return nil, err
	}
	var e entry[T]
	err = json.Unmarshal(raw, &e)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", key, err)
	}
	return &e, nil
}

func (e *entry[T]) answer() (T, error) {
	if e.NotFound != "" {
		var zero T
		return zero, notFound(e.NotFound)
	}
	return e.Value, nil
}

// notFound is a shared not-found answer. It reads as the error GitHub gave, and is gh.ErrNotFound.
type notFound string

func (e notFound) Error() string { return string(e) }

func (e notFound) Is(target error) bool { return target == gh.ErrNotFound }
