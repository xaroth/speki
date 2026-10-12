package gh

import (
	"fmt"
	"slices"
	"strings"

	"github.com/google/go-github/v88/github"
)

// Role is a user's role in a repository, lowest first. The names are GitHub's.
type Role int

// Roles. RoleNone is the zero value: no access at all.
const (
	RoleNone Role = iota
	RoleRead
	RoleTriage
	RoleWrite
	RoleMaintain
	RoleAdmin
)

var roleNames = []string{"none", "read", "triage", "write", "maintain", "admin"}

func (r Role) String() string {
	if r < RoleNone || int(r) >= len(roleNames) {
		return fmt.Sprintf("Role(%d)", int(r))
	}
	return roleNames[r]
}

// MarshalText writes a role by its GitHub name.
func (r Role) MarshalText() ([]byte, error) {
	if r < RoleNone || int(r) >= len(roleNames) {
		return nil, fmt.Errorf("unknown role %d", int(r))
	}
	return []byte(roleNames[r]), nil
}

// UnmarshalText reads a role by its GitHub name.
func (r *Role) UnmarshalText(text []byte) error {
	i := slices.Index(roleNames, string(text))
	if i < 0 {
		return fmt.Errorf("unknown role %q, want one of %s", text, strings.Join(roleNames[1:], ", "))
	}
	*r = Role(i)
	return nil
}

func roleOf(p *github.RepositoryPermissions) Role {
	switch {
	case p.GetAdmin():
		return RoleAdmin
	case p.GetMaintain():
		return RoleMaintain
	case p.GetPush():
		return RoleWrite
	case p.GetTriage():
		return RoleTriage
	case p.GetPull():
		return RoleRead
	}
	return RoleNone
}
