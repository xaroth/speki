package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	neturl "net/url"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	keyPrefix     = "speki:"
	noticeChannel = keyPrefix + "vault-head"
	pingTimeout   = 5 * time.Second
	// confirmTimeout is how long Subscribe waits for Redis to confirm the subscription before trying
	// again.
	confirmTimeout = 10 * time.Second
)

// setLogger sets the Redis client's logger, which is global, once.
var setLogger sync.Once

// Redis is a store shared by several instances, in Redis or Valkey.
type Redis struct {
	client *redis.Client
}

// NewRedis connects to the Redis at url (redis:// or rediss://) and checks that it answers. The Redis
// client's own messages go to logger, which becomes the client's logger for the whole process.
func NewRedis(ctx context.Context, url string, logger *slog.Logger) (*Redis, error) {
	setLogger.Do(func() { redis.SetLogger(redisLogger{logger}) })
	opt, err := redis.ParseURL(url)
	if err != nil {
		if uerr, ok := errors.AsType[*neturl.Error](err); ok {
			err = uerr.Err // the url.Error quotes the whole URL, password included
		}
		return nil, fmt.Errorf("redis url: %w", err)
	}
	r := &Redis{client: redis.NewClient(opt)}
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	err = r.client.Ping(ctx).Err()
	if err != nil {
		_ = r.client.Close()
		return nil, fmt.Errorf("redis: %w", err)
	}
	return r, nil
}

// Close closes the connection.
func (r *Redis) Close() error { return r.client.Close() }

// SetNX stores key with value for ttl unless it's already there, and reports whether it stored it.
func (r *Redis) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	ok, err := r.client.SetNX(ctx, keyPrefix+key, value, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("redis setnx: %w", err)
	}
	return ok, nil
}

// Set stores key with value for ttl.
func (r *Redis) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	err := r.client.Set(ctx, keyPrefix+key, value, ttl).Err()
	if err != nil {
		return fmt.Errorf("redis set: %w", err)
	}
	return nil
}

// Get returns the value of key, and whether there is one.
func (r *Redis) Get(ctx context.Context, key string) ([]byte, bool, error) {
	v, err := r.client.Get(ctx, keyPrefix+key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("redis get: %w", err)
	}
	return v, true, nil
}

// releaseScript deletes a key only while it holds the given value.
var releaseScript = redis.NewScript(`if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) end return 0`)

// Release removes key only while it still holds value: a lock that expired and was taken by someone
// else stays theirs.
func (r *Redis) Release(ctx context.Context, key string, value []byte) error {
	err := releaseScript.Run(ctx, r.client, []string{keyPrefix + key}, value).Err()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("redis release: %w", err)
	}
	return nil
}

type notice struct {
	RepoID int64  `json:"repo_id"`
	Commit string `json:"commit"`
}

// Publish tells the other instances that a vault's branch is at commit.
func (r *Redis) Publish(ctx context.Context, repoID int64, commit string) error {
	msg, err := json.Marshal(notice{RepoID: repoID, Commit: commit})
	if err != nil {
		return err
	}
	err = r.client.Publish(ctx, noticeChannel, msg).Err()
	if err != nil {
		return fmt.Errorf("redis publish: %w", err)
	}
	return nil
}

// Subscribe calls f for every notice, its own instance's included, until ctx ends. A notice lost while
// the connection is down only means waiting for the next poll.
func (r *Redis) Subscribe(ctx context.Context, f func(repoID int64, commit string)) error {
	sub := r.client.Subscribe(ctx, noticeChannel)
	defer func() { _ = sub.Close() }()
	// go-redis reads only give up on a context with a deadline. Closing the subscription when ctx ends
	// unblocks every read, so a stalled connection can't hold up a shutdown.
	stop := context.AfterFunc(ctx, func() { _ = sub.Close() })
	defer stop()
	confirmCtx, cancel := context.WithTimeout(ctx, confirmTimeout)
	_, err := sub.Receive(confirmCtx) // the subscription confirmation
	cancel()
	if err != nil {
		return fmt.Errorf("redis subscribe: %w", err)
	}
	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case m, ok := <-ch:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return errors.New("redis subscription closed")
			}
			var n notice
			if json.Unmarshal([]byte(m.Payload), &n) == nil && n.RepoID != 0 {
				f(n.RepoID, n.Commit)
			}
		}
	}
}

// redisLogger sends the Redis client's messages to slog, so every log line stays JSON.
type redisLogger struct{ logger *slog.Logger }

func (l redisLogger) Printf(ctx context.Context, format string, v ...any) {
	l.logger.WarnContext(ctx, "redis: "+fmt.Sprintf(format, v...))
}
