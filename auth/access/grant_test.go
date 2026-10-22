package access_test

import (
	"testing"

	"github.com/xaroth/speki/auth/access"
	"github.com/xaroth/speki/vault"
)

func TestGrantVaultNames(t *testing.T) {
	t.Parallel()
	g := access.NewGrant(access.User{}, []vault.Vault{{RepoID: 1, Name: "ours/kb"}}, nil)
	testCases := []struct {
		name   string
		lookup string
		found  bool
	}{
		{name: "ASCII case folds", lookup: "Ours/KB", found: true},
		{name: "Kelvin sign is not a k", lookup: "ours/Kb"},
		{name: "same length, different name", lookup: "ours/kc"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			v, ok := g.Vault(testCase.lookup)
			if ok != testCase.found {
				t.Fatalf("Vault(%q) found: got %v, want %v", testCase.lookup, ok, testCase.found)
			}
			if ok && v.Name != "ours/kb" {
				t.Errorf("Vault(%q): got %q, want %q", testCase.lookup, v.Name, "ours/kb")
			}
		})
	}
}
