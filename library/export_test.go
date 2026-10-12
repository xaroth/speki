package library

import "time"

// SetNow replaces the clock used for backoff and fresh-clone times. Call it before the library has vaults.
func (l *Library) SetNow(now func() time.Time) { l.now = now }

// SetMaxPacks sets how many pack files a copy may collect before it's cloned fresh.
func (l *Library) SetMaxPacks(n int) { l.maxPacks = n }

// MovedOnRetry is how long after a spoiled fresh clone the next one is tried.
const MovedOnRetry = movedOnRetry
