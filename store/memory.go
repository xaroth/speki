// Package store holds speki's short-lived shared state: used authorization codes, refresh locks and
// refresh results, and passes change notices between instances. One instance keeps it in memory; several
// share it through Redis. Packages that need it declare small interfaces for the methods they use.
package store

import (
	"bytes"
	"context"
	"sync"
	"time"
)

// Memory is a store for a single instance.
type Memory struct {
	mu      sync.Mutex
	values  map[string]entry
	cleaned time.Time
	now     func() time.Time
}

type entry struct {
	value   []byte
	expires time.Time
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{values: make(map[string]entry), now: time.Now}
}

// SetNX stores key with value for ttl unless it's already there, and reports whether it stored it.
func (m *Memory) SetNX(_ context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.cleanLocked()
	if e, ok := m.values[key]; ok && now.Before(e.expires) {
		return false, nil
	}
	m.values[key] = entry{value: value, expires: now.Add(ttl)}
	return true, nil
}

// Set stores key with value for ttl.
func (m *Memory) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.cleanLocked()
	m.values[key] = entry{value: value, expires: now.Add(ttl)}
	return nil
}

// Get returns the value of key, and whether there is one.
func (m *Memory) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.cleanLocked()
	e, ok := m.values[key]
	if !ok || !now.Before(e.expires) {
		return nil, false, nil
	}
	return e.value, true, nil
}

// Delete removes key.
func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.values, key)
	return nil
}

// Release removes key only while it still holds value.
func (m *Memory) Release(_ context.Context, key string, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.values[key]; ok && bytes.Equal(e.value, value) {
		delete(m.values, key)
	}
	return nil
}

// cleanLocked drops expired entries, at most once a minute, and returns the current time.
func (m *Memory) cleanLocked() time.Time {
	now := m.now()
	if now.Sub(m.cleaned) < time.Minute {
		return now
	}
	for k, e := range m.values {
		if !now.Before(e.expires) {
			delete(m.values, k)
		}
	}
	m.cleaned = now
	return now
}

// Publish does nothing: with one instance there's nobody to tell.
func (m *Memory) Publish(context.Context, int64, string) error { return nil }

// Subscribe waits for ctx to end: with one instance no notice ever comes.
func (m *Memory) Subscribe(ctx context.Context, _ func(int64, string)) error {
	<-ctx.Done()
	return nil
}

// Close does nothing.
func (m *Memory) Close() error { return nil }
