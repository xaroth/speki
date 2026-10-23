package knowledge_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/xaroth/speki/knowledge"
)

// Every tool answers a vault the user may read but speki doesn't serve as unavailable, and one they
// can't read as not found, without asking GitHub.
func TestUnavailableVaultNamed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	withUnavailable(t, f)
	f.setCopy(ours.RepoID, nil) // in the grant, but not served any more: list shows it as gone
	tools := []struct {
		name string
		call func(t *testing.T, name string) error
	}{
		{name: "list", call: func(t *testing.T, name string) error {
			_, err := f.base.List(t.Context(), f.grant, knowledge.ListRequest{Vault: name})
			return err
		}},
		{name: "search", call: func(t *testing.T, name string) error {
			_, err := f.base.Search(t.Context(), f.grant, knowledge.SearchRequest{Query: "zeppelin", Vaults: []string{"ours/shared", name}})
			return err
		}},
		{name: "query", call: func(t *testing.T, name string) error {
			_, err := f.base.Query(t.Context(), f.grant, knowledge.QueryRequest{SQL: "SELECT 1", Vaults: []string{name}})
			return err
		}},
		{name: "read", call: func(t *testing.T, name string) error {
			_, err := f.base.Read(t.Context(), f.grant, knowledge.ReadRequest{Vault: name, Path: "zeppelin.md"})
			return err
		}},
		{name: "history", call: func(t *testing.T, name string) error {
			_, err := f.base.History(t.Context(), f.grant, knowledge.HistoryRequest{Vault: name})
			return err
		}},
		{name: "write", call: func(t *testing.T, name string) error {
			_, err := f.base.Write(t.Context(), f.grant, knowledge.WriteRequest{Vault: name, Path: "a.md", Content: "# A\n"})
			return err
		}},
	}
	testCases := []struct {
		name    string
		vault   string
		code    knowledge.Code
		message string
	}{
		{name: "building", vault: "OURS/new", code: knowledge.CodeUnavailable, message: "unavailable: ours/new is still building its index. Try again in a minute"},
		{name: "failed", vault: "ours/old", code: knowledge.CodeUnavailable, message: "unavailable: ours/old failed to open"},
		{name: "gone", vault: "OURS/kb", code: knowledge.CodeUnavailable, message: "unavailable: ours/kb was just removed or is reopening. Try again shortly"},
		{name: "unknown", vault: "ours/nothing", code: knowledge.CodeNotFound, message: `not_found: there's no vault named "ours/nothing"`},
	}
	for _, tool := range tools {
		for _, testCase := range testCases {
			t.Run(tool.name+" "+testCase.name, func(t *testing.T) {
				t.Parallel()
				err := tool.call(t, testCase.vault)
				wantCode(t, err, testCase.code)
				if !strings.HasPrefix(err.Error(), testCase.message) {
					t.Errorf("got error %q, want it to start with %q", err, testCase.message)
				}
				if n := len(f.github.sent()); n != 0 {
					t.Errorf("got %d commits, want none", n)
				}
			})
		}
	}
}

// A vault whose index closes under a request answers as gone, the way list shows it.
func TestClosedIndex(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.copy(ours.RepoID).idx.Close()
	testCases := []struct {
		name    string
		call    func(t *testing.T) error
		message string
	}{
		{name: "read", call: func(t *testing.T) error {
			_, err := f.base.Read(t.Context(), f.grant, knowledge.ReadRequest{Vault: "ours/kb", Path: "README.md"})
			return err
		}},
		{name: "list", call: func(t *testing.T) error {
			_, err := f.base.List(t.Context(), f.grant, knowledge.ListRequest{Vault: "ours/kb"})
			return err
		}},
		{name: "search", message: "just removed or is reopening", call: func(t *testing.T) error {
			_, err := f.base.Search(t.Context(), f.grant, knowledge.SearchRequest{Query: "zeppelin", Vaults: []string{"ours/kb"}})
			return err
		}},
		{name: "query", message: "just removed or is reopening", call: func(t *testing.T) error {
			_, err := f.base.Query(t.Context(), f.grant, knowledge.QueryRequest{SQL: "SELECT 1", Vaults: []string{"ours/kb"}})
			return err
		}},
		{name: "delete", call: func(t *testing.T) error {
			_, err := f.base.Delete(t.Context(), f.grant, knowledge.DeleteRequest{Vault: "ours/kb", Path: "README.md", Version: "x"})
			return err
		}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			err := testCase.call(t)
			wantCode(t, err, knowledge.CodeUnavailable)
			if !strings.Contains(err.Error(), testCase.message) {
				t.Errorf("got error %v, want it to contain %q", err, testCase.message)
			}
			if n := len(f.github.sent()); n != 0 {
				t.Errorf("got %d commits, want none", n)
			}
		})
	}
}

// TestOtherVaultsStayHidden tries every read path on a vault the user can't read, which has a document
// at a path that also exists in a vault they can.
func TestOtherVaultsStayHidden(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	const hidden = "secret"

	vaults, _ := f.base.Vaults(ctx, f.grant)
	for _, v := range vaults {
		if v.Name == secret.Name {
			t.Error("Vaults lists the secret vault")
		}
	}
	res, err := f.base.Search(ctx, f.grant, knowledge.SearchRequest{Query: hidden})
	if err != nil || len(res.Hits) != 0 {
		t.Errorf("search all: got %+v, %v, want no hits", res.Hits, err)
	}
	res, err = f.base.Search(ctx, f.grant, knowledge.SearchRequest{Query: "zeppelin"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, h := range res.Hits {
		if h.Source == secret.Name || strings.Contains(h.Snippet, hidden) {
			t.Errorf("got a search hit from the secret vault: %+v", h)
		}
	}
	// Spelling correction only knows the user's vaults: "secrut" is close to a word only the secret
	// vault has, so it isn't corrected to it.
	res, err = f.base.Search(ctx, f.grant, knowledge.SearchRequest{Query: "secrut"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for word, instead := range res.Corrected {
		if instead == hidden {
			t.Errorf("got %q corrected to %q, a word only the secret vault has", word, instead)
		}
	}
	// A readable vault's document at the same path is that vault's, not the secret one.
	p, err := f.base.Read(ctx, f.grant, knowledge.ReadRequest{Vault: "ours/kb", Path: "services/edge-proxy.md"})
	if err != nil || strings.Contains(p.Content, hidden) {
		t.Errorf("read: got %q, %v, want ours/kb's document", p.Content, err)
	}

	calls := map[string]func() error{
		"search": func() error {
			_, err := f.base.Search(ctx, f.grant, knowledge.SearchRequest{Query: hidden, Vaults: []string{"ours/kb", secret.Name}, Path: "services", Tags: []string{"x"}})
			return err
		},
		"list": func() error {
			_, err := f.base.List(ctx, f.grant, knowledge.ListRequest{Vault: secret.Name, Path: "services"})
			return err
		},
		"read": func() error {
			_, err := f.base.Read(ctx, f.grant, knowledge.ReadRequest{Vault: secret.Name, Path: "services/edge-proxy.md"})
			return err
		},
		"read in another letter case": func() error {
			_, err := f.base.Read(ctx, f.grant, knowledge.ReadRequest{Vault: "OURS/SECRET", Path: "services/edge-proxy.md"})
			return err
		},
		"history": func() error {
			_, err := f.base.History(ctx, f.grant, knowledge.HistoryRequest{Vault: secret.Name})
			return err
		},
		"query": func() error {
			_, err := f.base.Query(ctx, f.grant, knowledge.QueryRequest{SQL: "SELECT 1", Vaults: []string{secret.Name}})
			return err
		},
	}
	for name, call := range calls {
		err := call()
		if kerr, ok := errors.AsType[*knowledge.Error](err); !ok || kerr.Code != knowledge.CodeNotFound {
			t.Errorf("%s: got %v, want not_found", name, err)
		}
	}
	if f.github.lookups+f.github.tips != 0 {
		t.Error("asked GitHub about the secret vault")
	}

	// The answer for a vault the user can't read is the answer for one that doesn't exist.
	_, errSecret := f.base.Read(ctx, f.grant, knowledge.ReadRequest{Vault: secret.Name, Path: "x.md"})
	_, errNone := f.base.Read(ctx, f.grant, knowledge.ReadRequest{Vault: "ours/nothing", Path: "x.md"})
	if got, want := strings.ReplaceAll(errSecret.Error(), secret.Name, "V"), strings.ReplaceAll(errNone.Error(), "ours/nothing", "V"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
