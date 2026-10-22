package service_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xaroth/speki/gh"
	"github.com/xaroth/speki/gh/ghtest"
)

// TestSignInAndSearch runs speki against a fake GitHub: a client registers, the user signs in through
// the browser flow, and an MCP client uses the token to find, read, write and edit documents.
func TestSignInAndSearch(t *testing.T) {
	t.Parallel()
	g := newGitHub(t, map[string]string{
		"speki.yaml":   "title: KB\ndescription: Test vault.\nguidelines: One page per topic.\n",
		"fleet.md":     "---\ntitle: Fleet doctrine\ntags: [fleet]\n---\n# Fleet doctrine\n\nWe fly zeppelins.\n",
		"ops/roles.md": "# Roles\n\nWho does what.\n",
	}, nil)
	s := startService(t, testConfig(t, g))
	cs := connectMCP(t, s, signIn(t, s).AccessToken)

	text := callTool(t, cs, "guide", nil)
	for _, want := range []string{"### ours/kb", "One page per topic.", "read and write"} {
		if !strings.Contains(text, want) {
			t.Errorf("guide: got\n%s\nwant it to contain %q", text, want)
		}
	}

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "search", Arguments: map[string]any{"query": "zeppelin"}})
	if err != nil || res.IsError {
		t.Fatalf("search: got %v %+v, want results", err, res)
	}
	type result struct{ Vault, Path, Version string }
	var found struct{ Results []result }
	raw, err := json.Marshal(res.StructuredContent)
	if err == nil {
		err = json.Unmarshal(raw, &found)
	}
	if err != nil || len(found.Results) != 1 {
		t.Fatalf("search: got %s, want one result", raw)
	}
	if diff := cmp.Diff(result{Vault: kbRepo, Path: "fleet.md", Version: found.Results[0].Version}, found.Results[0]); diff != "" {
		t.Errorf("search result (-want +got):\n%s", diff)
	}

	text = callTool(t, cs, "read", map[string]any{"vault": kbRepo, "path": "fleet.md"})
	if !strings.Contains(text, "version "+found.Results[0].Version) || !strings.Contains(text, "\">\n---\ntitle: Fleet doctrine\n") {
		t.Errorf("read: got\n%s\nwant the document with its version", text)
	}

	text = callTool(t, cs, "list", map[string]any{"vault": kbRepo})
	if !strings.Contains(text, "ops/\n") || !strings.Contains(text, "fleet.md  Fleet doctrine") {
		t.Errorf("list: got\n%s\nwant ops/ and fleet.md", text)
	}

	text = callTool(t, cs, "history", map[string]any{"vault": kbRepo, "path": "fleet.md"})
	if !strings.Contains(text, "octocat via Claude Code: Add fleet notes") {
		t.Errorf("history: got\n%s\nwant octocat's commit", text)
	}

	// A new document is searchable right after it's written, and an edit right after that.
	text = callTool(t, cs, "write", map[string]any{"vault": kbRepo, "path": "ops/airships.md",
		"content": "---\ntitle: Airships\n---\n# Airships\n\nDirigibles are kept in hangar nine.\n"})
	if !strings.Contains(text, "Saved in commit") || strings.Contains(text, "Search may not") {
		t.Errorf("write: got\n%s\nwant it saved and searchable", text)
	}
	text = callTool(t, cs, "search", map[string]any{"query": "dirigible hangar"})
	if !strings.Contains(text, "ops/airships.md") {
		t.Errorf("search after write: got\n%s\nwant ops/airships.md", text)
	}
	callTool(t, cs, "edit", map[string]any{"vault": kbRepo, "path": "ops/airships.md", "old_text": "hangar nine", "new_text": "hangar ten"})
	text = callTool(t, cs, "read", map[string]any{"vault": kbRepo, "path": "ops/airships.md"})
	if !strings.Contains(text, "hangar ten") || !strings.Contains(text, "created: ") {
		t.Errorf("read after edit: got\n%s\nwant the edit", text)
	}
	text = callTool(t, cs, "query", map[string]any{"sql": "SELECT path FROM documents WHERE path LIKE 'ops/%' ORDER BY path"})
	if want := "path\nops/airships.md\nops/roles.md\n2 rows"; !strings.HasPrefix(text, want) {
		t.Errorf("query: got\n%s\nwant it to start with\n%s", text, want)
	}
	if got, want := g.headMessage(kbRepo), "Update ops/airships.md\n\nSpeki-Client: e2e\nSpeki-Request: "; !strings.HasPrefix(got, want) {
		t.Errorf("got commit message %q, want it to start with %q", got, want)
	}

	if text, isError := tryTool(t, cs, "read", map[string]any{"vault": "ours/other", "path": "fleet.md"}); !isError {
		t.Errorf("read from an unknown vault: got %s, want an error", text)
	}
}

// TestOtherVaultsStayHidden serves a vault the user can't pull on GitHub, and tries every tool on it,
// through sign-in, the resolver, the grant and the tools.
func TestOtherVaultsStayHidden(t *testing.T) {
	t.Parallel()
	g := newGitHub(t, kbFiles, map[string]string{
		"speki.yaml": "title: Secret\ndescription: Not for this user.\n",
		"plans.md":   "# Plans\n\nThe zeppelin launch codes.\n",
	})
	s := startService(t, testConfig(t, g))
	if page := getStatus(t, s); len(page.Vaults) != 2 {
		t.Fatalf("got %d vaults served, want both", len(page.Vaults))
	}
	cs := connectMCP(t, s, signIn(t, s).AccessToken)

	testCases := []struct {
		name     string
		tool     string
		args     map[string]any
		wantDeny bool // a not_found error; otherwise an answer without the hidden vault
	}{
		{name: "guide", tool: "guide"},
		{name: "list vaults", tool: "list"},
		{name: "search everything", tool: "search", args: map[string]any{"query": "zeppelin"}},
		{name: "query everything", tool: "query", args: map[string]any{"sql": "SELECT vault, path, body FROM documents"}},
		{name: "read", tool: "read", args: map[string]any{"vault": hiddenRepo, "path": "plans.md"}, wantDeny: true},
		{name: "list it", tool: "list", args: map[string]any{"vault": hiddenRepo}, wantDeny: true},
		{name: "search it", tool: "search", args: map[string]any{"query": "launch", "vaults": []string{hiddenRepo}}, wantDeny: true},
		{name: "history", tool: "history", args: map[string]any{"vault": hiddenRepo}, wantDeny: true},
		{name: "query it", tool: "query", args: map[string]any{"sql": "SELECT 1", "vaults": []string{hiddenRepo}}, wantDeny: true},
		{name: "write", tool: "write", args: map[string]any{"vault": hiddenRepo, "path": "x.md", "content": "# X\n"}, wantDeny: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			text, isError := tryTool(t, cs, testCase.tool, testCase.args)
			if testCase.wantDeny && (!isError || !strings.HasPrefix(text, "not_found:")) {
				t.Errorf("got error %t: %s, want a not_found error", isError, text)
			}
			if !testCase.wantDeny && isError {
				t.Errorf("got error: %s, want an answer", text)
			}
			// A denial may name what was asked for, but nothing in it.
			leaked := strings.Contains(text, "launch") ||
				!testCase.wantDeny && (strings.Contains(text, hiddenRepo) || strings.Contains(text, "plans.md"))
			if leaked {
				t.Errorf("got\n%s\nwant nothing from the hidden vault", text)
			}
		})
	}
}

// TestRenamedUser renames the signed-in user on GitHub. From their next refresh they're matched in
// writers by their new login, not the old one, which someone else may take.
func TestRenamedUser(t *testing.T) {
	t.Parallel()
	g := newGitHub(t, map[string]string{
		"speki.yaml": "title: KB\ndescription: Test vault.\nwriters: [octocat]\n",
		"fleet.md":   "# Fleet\n\nWe fly zeppelins.\n",
	}, nil)
	s := startService(t, testConfig(t, g))
	tokens := signIn(t, s)
	write := func(path string) map[string]any {
		return map[string]any{"vault": kbRepo, "path": path, "content": "# Airships\n"}
	}
	callTool(t, connectMCP(t, s, tokens.AccessToken), "write", write("before.md"))

	g.Apply(
		ghtest.ExpectUser(userToken, gh.User{ID: 7, Login: "octokitten"}),
		ghtest.ExpectRole(userToken, kbRepo, gh.RoleWrite),
	)
	cs := connectMCP(t, s, refresh(t, s, tokens).AccessToken)
	if text, _ := tryTool(t, cs, "write", write("after.md")); !strings.HasPrefix(text, "no_write_access:") {
		t.Errorf("write after the rename: got %s, want no_write_access", text)
	}
	callTool(t, cs, "read", map[string]any{"vault": kbRepo, "path": "fleet.md"})
}
