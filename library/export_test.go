package library

import "time"

// SetNow replaces the clock used for backoff and fresh-clone times. Call it before the library has vaults.
func (l *Library) SetNow(now func() time.Time) { l.now = now }

// SetMaxPacks sets how many pack files a copy may collect before it's cloned fresh.
func (l *Library) SetMaxPacks(n int) { l.maxPacks = n }

// SetRename replaces the rename that swaps in a fresh clone. Call it before the library has vaults.
func (l *Library) SetRename(rename func(oldpath, newpath string) error) { l.rename = rename }

// MovedOnRetry is how long after a spoiled fresh clone the next one is tried.
const MovedOnRetry = movedOnRetry
