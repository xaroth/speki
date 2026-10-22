package access

import (
	"cmp"
	"slices"

	"github.com/xaroth/speki/vault"
)

// User is a signed-in user, from their speki token.
type User struct {
	ID     int64
	Login  string
	Token  string // the user's GitHub token
	Client string // the MCP client they signed in with
	Write  bool   // signed in with write access
}

// State says why a vault the user may read isn't served.
type State string

// The states of vaults the user may read that aren't served.
const (
	StateBuilding State = "building" // being cloned and indexed for the first time
	StateFailed   State = "failed"   // failed to open; speki retries it with backoff
	StateGone     State = "gone"     // served when the grant was made, but not any more: removed, or reopening
)

// Unavailable is a vault the user may read that speki doesn't serve right now.
type Unavailable struct {
	Vault vault.Vault
	State State
}

// Grant is what one user may do.
type Grant struct {
	user        User
	readable    []vault.Vault // sorted by name
	writable    map[int64]bool
	unavailable []Unavailable // sorted by name
}

// NewGrant returns a grant for u to read readable and write the vaults in writable, and that knows u may
// read the vaults in unavailable once they're served. Resolver makes the real ones; tests make their own.
func NewGrant(u User, readable []vault.Vault, writable []int64, unavailable ...Unavailable) *Grant {
	g := &Grant{
		user:        u,
		readable:    slices.Clone(readable),
		writable:    make(map[int64]bool),
		unavailable: slices.Clone(unavailable),
	}
	slices.SortFunc(g.readable, func(a, b vault.Vault) int { return cmp.Compare(a.Name, b.Name) })
	slices.SortFunc(g.unavailable, func(a, b Unavailable) int { return cmp.Compare(a.Vault.Name, b.Vault.Name) })
	if u.Write {
		for _, id := range writable {
			g.writable[id] = true
		}
	}
	return g
}

// User returns who the grant is for, including their GitHub token: the one place speki acts as the user.
func (g *Grant) User() User { return g.user }

// Readable returns the vaults the user may read, sorted by name.
func (g *Grant) Readable() []vault.Vault { return slices.Clone(g.readable) }

// Vault returns a readable vault by name. Names are matched without regard to letter case, as GitHub
// matches repository names.
func (g *Grant) Vault(name string) (vault.Vault, bool) {
	for _, v := range g.readable {
		if asciiEqualFold(v.Name, name) {
			return v, true
		}
	}
	return vault.Vault{}, false
}

// Unavailable returns the vaults the user may read that aren't served, sorted by name. They're never in
// Readable.
func (g *Grant) Unavailable() []Unavailable { return slices.Clone(g.unavailable) }

// UnavailableVault returns a vault the user may read that isn't served, by name, matched like Vault.
func (g *Grant) UnavailableVault(name string) (Unavailable, bool) {
	for _, u := range g.unavailable {
		if asciiEqualFold(u.Vault.Name, name) {
			return u, true
		}
	}
	return Unavailable{}, false
}

// CanWrite reports whether the user may write to a vault.
func (g *Grant) CanWrite(repoID int64) bool { return g.writable[repoID] }

// ReadOnlySession reports whether the user signed in without write access.
func (g *Grant) ReadOnlySession() bool { return !g.user.Write }
