package gh_test

import (
	"testing"

	"github.com/xaroth/speki/gh"
)

func TestRoleText(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		role    gh.Role
		text    string
		wantErr bool
	}{
		{name: "none", role: gh.RoleNone, text: "none"},
		{name: "read", role: gh.RoleRead, text: "read"},
		{name: "triage", role: gh.RoleTriage, text: "triage"},
		{name: "write", role: gh.RoleWrite, text: "write"},
		{name: "maintain", role: gh.RoleMaintain, text: "maintain"},
		{name: "admin", role: gh.RoleAdmin, text: "admin"},
		{name: "out of range", role: gh.RoleAdmin + 1, text: "Role(6)", wantErr: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := testCase.role.String(); got != testCase.text {
				t.Errorf("String: got %q, want %q", got, testCase.text)
			}
			raw, err := testCase.role.MarshalText()
			if (err != nil) != testCase.wantErr {
				t.Fatalf("MarshalText: got %v, want error %t", err, testCase.wantErr)
			}
			if testCase.wantErr {
				return
			}
			var got gh.Role
			if err := got.UnmarshalText(raw); err != nil || got != testCase.role {
				t.Errorf("UnmarshalText(%q): got %v, %v, want %v", raw, got, err, testCase.role)
			}
		})
	}

	t.Run("unknown name", func(t *testing.T) {
		t.Parallel()
		var r gh.Role
		if err := r.UnmarshalText([]byte("owner")); err == nil {
			t.Error("got no error, want one")
		}
	})
}
