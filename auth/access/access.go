// Package access decides what a signed-in user may do: whether they get in at all, and which vaults they
// may read and write. GitHub is the source of truth, narrowed by each vault's readers and writers; answers
// are cached for five minutes, in a cache every instance shares.
package access

import "errors"

// ErrUnauthorized means GitHub rejected the user's token: it expired or was revoked.
var ErrUnauthorized = errors.New("GitHub no longer accepts this sign-in")

// ErrNotMember means the user isn't an active member of any required organisation.
var ErrNotMember = errors.New("you aren't a member of an organisation this speki requires")

// ErrNoVaults means the user can't read any vault.
var ErrNoVaults = errors.New("you can't read any vault on this speki")

// ErrNotReady means the user can't read any vault being served yet, but may read one speki is still
// cloning and indexing.
var ErrNotReady = errors.New("speki is still loading your vaults; try again shortly")

// ErrVaultsFailed means the only vaults the user may read failed to open. Speki retries them with backoff,
// so asking again in a few minutes can succeed.
var ErrVaultsFailed = errors.New("speki couldn't open your vaults and keeps retrying; try again in a few minutes")

// errLoading means a resolve found only vaults being built that the user may read: worth waiting for.
var errLoading = errors.New("readable vaults are still being built")

// asciiEqualFold reports whether a and b are equal, ignoring the case of ASCII letters only: GitHub names
// are ASCII, and a Unicode fold would let a Kelvin sign stand for a k.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
