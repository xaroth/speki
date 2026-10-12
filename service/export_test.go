package service

import (
	"context"
	"time"
)

// URLs waits until s listens, and returns the base URLs of its public and internal servers.
func (s *Service) URLs() (public, internal string) {
	<-s.started
	return "http://" + s.publicAddr.String(), "http://" + s.internalAddr.String()
}

// WaitServing waits for the first discovery round and the vault builds it started.
func (s *Service) WaitServing() {
	<-s.discovered
	s.library.WaitIdle()
}

// Ready reports what /readyz says, also once Run returned and nothing answers it any more.
func (s *Service) Ready() bool { return s.ready.Load() }

// SetStopTimeouts shortens how long a stopping s waits for requests and for background work. Call it
// before Run.
func (s *Service) SetStopTimeouts(drain, background time.Duration) {
	s.drainTimeout, s.backgroundTimeout = drain, background
}

// StartBackground runs f as background work that Run waits for when it stops.
func (s *Service) StartBackground(f func()) { s.background.Go(f) }

// WaitHandlers waits for running HTTP handlers until ctx ends, and reports whether they all returned.
func (s *Service) WaitHandlers(ctx context.Context) bool { return waitFor(ctx, &s.handlers) }
