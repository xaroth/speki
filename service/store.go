package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/xaroth/speki/store"
)

// sharedStore is the state instances share, and the channel they tell each other about changes on.
type sharedStore interface {
	SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Get(ctx context.Context, key string) ([]byte, bool, error)
	GetDel(ctx context.Context, key string) ([]byte, bool, error)
	Release(ctx context.Context, key string, value []byte) error
	Publish(ctx context.Context, repoID int64, commit string) error
	Subscribe(ctx context.Context, f func(repoID int64, commit string)) error
	Close() error
}

// memoryStoreWarning is logged at startup and shown on the status page. Instances with a memory store
// each keep their own refresh locks and results, so two of them refreshing one token sign the user out.
const memoryStoreWarning = "no redis.url: sign-in state is kept in memory, so run exactly one instance; several instances need redis.url"

func newStore(cfg Redis, logger *slog.Logger) (sharedStore, error) {
	if cfg.URL == "" {
		logger.Warn(memoryStoreWarning)
		return store.NewMemory(), nil
	}
	r, err := store.NewRedis(context.Background(), cfg.URL, logger)
	if err != nil {
		return nil, fmt.Errorf("configuration: redis.url: %w", err)
	}
	return r, nil
}
