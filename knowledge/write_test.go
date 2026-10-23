package knowledge_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/text/unicode/norm"

	"github.com/xaroth/speki/auth/access"
	"github.com/xaroth/speki/document"
	"github.com/xaroth/speki/gh"
	"github.com/xaroth/speki/knowledge"
	"github.com/xaroth/speki/vault"
)

func TestChanges(t *testing.T) {
	t.Parallel()
	const dated = "created: " + nowStamp + "\nupdated: " + nowStamp + "\n"
	longTitle := strings.Repeat("t", document.MaxTitle+1)
	nfd := norm.NFD.String
	moving := map[string]string{
		"a.md":     "# A\n",
		"b.md":     "# B\n\nSee [A](a.md).\n",
		"c.md":     "# C\n\nSee [[a]].\n",
		"taken.md": "# Taken\n",
		"empty.md": "",
	}
	created := map[string]string{"a.md": "---\ntitle: A\ncreated: 2020-01-02T03:04:05Z\n---\nBody.\n"}
	testCases := []struct {
		name        string
		files       map[string]string // of ours/kb; the defaults when nil
		req         any
		wantChanges []gh.FileChange
		wantMessage string
		wantResult  knowledge.Result
	}{
		{
			name:        "create",
			req:         knowledge.WriteRequest{Vault: "ours/kb", Path: "/runbooks/deploy.md", Content: "---\ntitle: Deploying\n---\n# Deploying\n\nRun the zeppelin job.\n"},
			wantChanges: []gh.FileChange{{Path: "runbooks/deploy.md", Content: []byte("---\ntitle: Deploying\n" + dated + "---\n# Deploying\n\nRun the zeppelin job.\n")}},
			wantMessage: "Create runbooks/deploy.md",
			wantResult:  knowledge.Result{Version: blobID("---\ntitle: Deploying\n" + dated + "---\n# Deploying\n\nRun the zeppelin job.\n")},
		},
		{
			name:        "create ignores the agent's created",
			req:         knowledge.WriteRequest{Vault: "ours/kb", Path: "new.md", Content: "---\ntitle: New\ncreated: 1999-01-01T00:00:00Z\n---\n"},
			wantChanges: []gh.FileChange{{Path: "new.md", Content: []byte("---\ntitle: New\n" + dated + "---\n")}},
			wantMessage: "Create new.md",
			wantResult:  knowledge.Result{Version: blobID("---\ntitle: New\n" + dated + "---\n")},
		},
		{
			name:        "create in a folder git stores in NFD",
			files:       map[string]string{nfd("menü/brötchen.md"): "# Brötchen\n"},
			req:         knowledge.WriteRequest{Vault: "ours/kb", Path: "menü/käse.md", Content: "# Käse\n"},
			wantChanges: []gh.FileChange{{Path: nfd("menü") + "/käse.md", Content: []byte("---\n" + dated + "---\n# Käse\n")}},
			wantMessage: "Create " + nfd("menü") + "/käse.md",
			wantResult:  knowledge.Result{Version: blobID("---\n" + dated + "---\n# Käse\n")},
		},
		{
			name:        "create with a title of the longest length",
			req:         knowledge.WriteRequest{Vault: "ours/kb", Path: "ok.md", Content: "---\ntitle: " + longTitle[1:] + "\n---\n"},
			wantChanges: []gh.FileChange{{Path: "ok.md", Content: []byte("---\ntitle: " + longTitle[1:] + "\n" + dated + "---\n")}},
			wantMessage: "Create ok.md",
			wantResult:  knowledge.Result{Version: blobID("---\ntitle: " + longTitle[1:] + "\n" + dated + "---\n")},
		},
		{
			name:        "replace",
			req:         knowledge.WriteRequest{Vault: "ours/kb", Path: "README.md", Version: blobID(readme), Content: "---\ntitle: About\n---\nNew text.\n", Message: "Rewrite the intro"},
			wantChanges: []gh.FileChange{{Path: "README.md", Content: []byte("---\ntitle: About\nupdated: " + nowStamp + "\n---\nNew text.\n")}},
			wantMessage: "Rewrite the intro",
			wantResult:  knowledge.Result{Version: blobID("---\ntitle: About\nupdated: " + nowStamp + "\n---\nNew text.\n")},
		},
		{
			name:        "replace keeps created",
			files:       created,
			req:         knowledge.WriteRequest{Vault: "ours/kb", Path: "a.md", Version: blobID(created["a.md"]), Content: "---\ntitle: A\n---\nNew body.\n"},
			wantChanges: []gh.FileChange{{Path: "a.md", Content: []byte("---\ntitle: A\ncreated: 2020-01-02T03:04:05Z\nupdated: " + nowStamp + "\n---\nNew body.\n")}},
			wantMessage: "Update a.md",
			wantResult:  knowledge.Result{Version: blobID("---\ntitle: A\ncreated: 2020-01-02T03:04:05Z\nupdated: " + nowStamp + "\n---\nNew body.\n")},
		},
		{
			name:        "edit",
			req:         knowledge.EditRequest{Vault: "ours/kb", Path: "services/edge-proxy.md", OldText: "Run the job.", NewText: "Run the deploy job."},
			wantChanges: []gh.FileChange{{Path: "services/edge-proxy.md", Content: []byte(strings.Replace(strings.Replace(edgeProxy, "Run the job.", "Run the deploy job.", 1), "tags: [edge]\n", "tags: [edge]\nupdated: "+nowStamp+"\n", 1))}},
			wantMessage: "Update services/edge-proxy.md",
			wantResult:  knowledge.Result{Version: blobID(strings.Replace(strings.Replace(edgeProxy, "Run the job.", "Run the deploy job.", 1), "tags: [edge]\n", "tags: [edge]\nupdated: "+nowStamp+"\n", 1))},
		},
		{
			name:        "edit every match",
			req:         knowledge.EditRequest{Vault: "ours/kb", Path: "services/edge-proxy.md", OldText: "Edge proxy", NewText: "Edge gateway", ReplaceAll: true},
			wantChanges: []gh.FileChange{{Path: "services/edge-proxy.md", Content: []byte(strings.Replace(strings.ReplaceAll(edgeProxy, "Edge proxy", "Edge gateway"), "tags: [edge]\n", "tags: [edge]\nupdated: "+nowStamp+"\n", 1))}},
			wantMessage: "Update services/edge-proxy.md",
			wantResult:  knowledge.Result{Version: blobID(strings.Replace(strings.ReplaceAll(edgeProxy, "Edge proxy", "Edge gateway"), "tags: [edge]\n", "tags: [edge]\nupdated: "+nowStamp+"\n", 1))},
		},
		{
			name:        "create with a key the vault allows",
			req:         knowledge.WriteRequest{Vault: "ours/kb", Path: "new.md", Content: "---\ntitle: New\nowner: me\n---\n"},
			wantChanges: []gh.FileChange{{Path: "new.md", Content: []byte("---\ntitle: New\nowner: me\n" + dated + "---\n")}},
			wantMessage: "Create new.md",
			wantResult:  knowledge.Result{Version: blobID("---\ntitle: New\nowner: me\n" + dated + "---\n")},
		},
		{
			name:        "create with related documents",
			req:         knowledge.WriteRequest{Vault: "ours/kb", Path: "services/new.md", Content: "---\ntitle: New\nrelated: [edge-proxy.md, /README.md]\n---\n"},
			wantChanges: []gh.FileChange{{Path: "services/new.md", Content: []byte("---\ntitle: New\nrelated: [edge-proxy.md, /README.md]\n" + dated + "---\n")}},
			wantMessage: "Create services/new.md",
			wantResult:  knowledge.Result{Version: blobID("---\ntitle: New\nrelated: [edge-proxy.md, /README.md]\n" + dated + "---\n")},
		},
		{
			name:        "edit keeping a related document that's gone",
			files:       map[string]string{"a.md": "---\ntitle: A\nrelated: [gone.md]\n---\nBody.\n"},
			req:         knowledge.EditRequest{Vault: "ours/kb", Path: "a.md", OldText: "Body.", NewText: "Text."},
			wantChanges: []gh.FileChange{{Path: "a.md", Content: []byte("---\ntitle: A\nrelated: [gone.md]\nupdated: " + nowStamp + "\n---\nText.\n")}},
			wantMessage: "Update a.md",
			wantResult:  knowledge.Result{Version: blobID("---\ntitle: A\nrelated: [gone.md]\nupdated: " + nowStamp + "\n---\nText.\n")},
		},
		{
			name:        "edit removing created keeps it",
			files:       created,
			req:         knowledge.EditRequest{Vault: "ours/kb", Path: "a.md", OldText: "created: 2020-01-02T03:04:05Z\n", NewText: ""},
			wantChanges: []gh.FileChange{{Path: "a.md", Content: []byte("---\ntitle: A\ncreated: 2020-01-02T03:04:05Z\nupdated: " + nowStamp + "\n---\nBody.\n")}},
			wantMessage: "Update a.md",
			wantResult:  knowledge.Result{Version: blobID("---\ntitle: A\ncreated: 2020-01-02T03:04:05Z\nupdated: " + nowStamp + "\n---\nBody.\n")},
		},
		{
			name:        "edit keeping a problem from outside speki",
			files:       map[string]string{"odd.md": "---\ntitle: Odd\nstatus: wip\n---\nBody text.\n"},
			req:         knowledge.EditRequest{Vault: "ours/kb", Path: "odd.md", OldText: "Body text.", NewText: "Better text."},
			wantChanges: []gh.FileChange{{Path: "odd.md", Content: []byte("---\ntitle: Odd\nstatus: wip\nupdated: " + nowStamp + "\n---\nBetter text.\n")}},
			wantMessage: "Update odd.md",
			wantResult:  knowledge.Result{Version: blobID("---\ntitle: Odd\nstatus: wip\nupdated: " + nowStamp + "\n---\nBetter text.\n")},
		},
		{
			name:        "edit of a long title from outside speki",
			files:       map[string]string{"long.md": "---\ntitle: " + longTitle + "\n---\nBody text.\n"},
			req:         knowledge.EditRequest{Vault: "ours/kb", Path: "long.md", OldText: "Body", NewText: "New"},
			wantChanges: []gh.FileChange{{Path: "long.md", Content: []byte("---\ntitle: " + longTitle + "\nupdated: " + nowStamp + "\n---\nNew text.\n")}},
			wantMessage: "Update long.md",
			wantResult:  knowledge.Result{Version: blobID("---\ntitle: " + longTitle + "\nupdated: " + nowStamp + "\n---\nNew text.\n")},
		},
		{
			name:        "edit fixing broken frontmatter",
			files:       map[string]string{"broken.md": "---\ntitle: [\n---\nBody text.\n"},
			req:         knowledge.EditRequest{Vault: "ours/kb", Path: "broken.md", OldText: "title: [", NewText: "title: Fixed"},
			wantChanges: []gh.FileChange{{Path: "broken.md", Content: []byte("---\ntitle: Fixed\nupdated: " + nowStamp + "\n---\nBody text.\n")}},
			wantMessage: "Update broken.md",
			wantResult:  knowledge.Result{Version: blobID("---\ntitle: Fixed\nupdated: " + nowStamp + "\n---\nBody text.\n")},
		},
		{
			name:        "move",
			files:       moving,
			req:         knowledge.MoveRequest{Vault: "ours/kb", From: "a.md", To: "archive/a.md", Version: blobID("# A\n")},
			wantChanges: []gh.FileChange{{Path: "a.md"}, {Path: "archive/a.md", Content: []byte("# A\n")}},
			wantMessage: "Move a.md to archive/a.md",
			wantResult:  knowledge.Result{Version: blobID("# A\n"), LinkedFrom: []string{"b.md", "c.md"}},
		},
		{
			name:        "move to another letter case",
			files:       moving,
			req:         knowledge.MoveRequest{Vault: "ours/kb", From: "taken.md", To: "Taken.md", Version: blobID("# Taken\n")},
			wantChanges: []gh.FileChange{{Path: "taken.md"}, {Path: "Taken.md", Content: []byte("# Taken\n")}},
			wantMessage: "Move taken.md to Taken.md",
			wantResult:  knowledge.Result{Version: blobID("# Taken\n")},
		},
		{
			name:        "move an empty document",
			files:       moving,
			req:         knowledge.MoveRequest{Vault: "ours/kb", From: "empty.md", To: "still-empty.md", Version: blobID("")},
			wantChanges: []gh.FileChange{{Path: "empty.md"}, {Path: "still-empty.md", Content: []byte{}}},
			wantMessage: "Move empty.md to still-empty.md",
			wantResult:  knowledge.Result{Version: blobID("")},
		},
		{
			name:        "delete",
			files:       moving,
			req:         knowledge.DeleteRequest{Vault: "ours/kb", Path: "a.md", Version: blobID("# A\n")},
			wantChanges: []gh.FileChange{{Path: "a.md"}},
			wantMessage: "Delete a.md",
			wantResult:  knowledge.Result{LinkedFrom: []string{"b.md", "c.md"}},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var opts []fixtureOption
			if testCase.files != nil {
				opts = append(opts, withKB(testCase.files))
			}
			f := newFixture(t, opts...)
			got, err := f.change(t.Context(), f.grant, testCase.req)
			if err != nil {
				t.Fatalf("change: %v", err)
			}
			want := testCase.wantResult
			want.Commit = "commit-1"
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("result (-want +got):\n%s", diff)
			}
			c := f.lastCommit(t)
			c.Message = withoutRequestID(c.Message)
			wantCommit := gh.CommitRequest{
				Repo:         "ours/kb",
				Branch:       "main",
				ExpectedHead: "c1",
				Message:      testCase.wantMessage + "\n\nSpeki-Client: Claude Code",
				Changes:      testCase.wantChanges,
			}
			if diff := cmp.Diff(wantCommit, c); diff != "" {
				t.Errorf("commit (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff([]string{"1:commit-1"}, f.notices.all()); diff != "" {
				t.Errorf("notices (-want +got):\n%s", diff)
			}
			// The index has the change by the time the change answers.
			last := testCase.wantChanges[len(testCase.wantChanges)-1]
			p, err := f.base.Read(t.Context(), f.grant, knowledge.ReadRequest{Vault: "ours/kb", Path: last.Path})
			if last.Content == nil {
				wantCode(t, err, knowledge.CodeNotFound)
				return
			}
			if err != nil || p.Document.Version != got.Version {
				t.Errorf("read after the change: got version %q, %v, want %q", p.Document.Version, err, got.Version)
			}
		})
	}
}

// Changes that are refused before anything is committed.
func TestChangeRejects(t *testing.T) {
	t.Parallel()
	nfd := norm.NFD.String
	f := newFixture(t, withKB(map[string]string{
		"README.md":              readme,
		"services/edge-proxy.md": edgeProxy,
		"services/db/pg.md":      postgres,
		"notes.txt":              "x",
		"dir.md/inside.md":       "# Inside\n",
		nfd("menü/brötchen.md"):  "# Brötchen\n",
		"café.md":                "# Café\n",
		"body.md":                "# B\n\nab ab ab\n",
		"taken.md":               "# Taken\n",
		"broken.md":              "---\ntitle: [\n---\nBody text.\n",
	}))
	f.copy(ours.RepoID).setFile("c1", "huge.md", []byte("too large to index"))
	readOnly := access.NewGrant(access.User{Login: "octocat", Token: "ghu_x"}, []vault.Vault{ours, shared}, []int64{ours.RepoID})
	write := func(path, content string) knowledge.WriteRequest {
		return knowledge.WriteRequest{Vault: "ours/kb", Path: path, Content: content}
	}
	edit := func(oldText, newText string) knowledge.EditRequest {
		return knowledge.EditRequest{Vault: "ours/kb", Path: "services/edge-proxy.md", OldText: oldText, NewText: newText}
	}
	testCases := []struct {
		name        string
		grant       *access.Grant // f.grant when nil
		req         any
		code        knowledge.Code
		wantVersion string
		wantMessage string // part of it
	}{
		{name: "read-only session", grant: readOnly, req: write("new.md", "# New\n"), code: knowledge.CodeReadOnlySession},
		// The grant is checked before the request itself, so a read-only session always hears so.
		{name: "read-only session, bad edit", grant: readOnly, req: knowledge.EditRequest{Vault: "ours/kb", Path: "a.md"}, code: knowledge.CodeReadOnlySession},
		{name: "read-only session, bad move", grant: readOnly, req: knowledge.MoveRequest{Vault: "ours/kb", From: "a.md", To: "a.md"}, code: knowledge.CodeReadOnlySession},
		{name: "read-only session, bad delete", grant: readOnly, req: knowledge.DeleteRequest{Vault: "ours/kb", Path: "a.md"}, code: knowledge.CodeReadOnlySession},
		{name: "no write access", req: knowledge.WriteRequest{Vault: "ours/shared", Path: "new.md", Content: "# New\n"}, code: knowledge.CodeNoWriteAccess},
		{name: "write to a vault the user can't read", req: knowledge.WriteRequest{Vault: "ours/secret", Path: "x.md", Content: "# X\n"}, code: knowledge.CodeNotFound},
		{name: "delete in a vault the user can't read", req: knowledge.DeleteRequest{Vault: "ours/secret", Path: "services/edge-proxy.md", Version: "v"}, code: knowledge.CodeNotFound},

		{name: "create over a document", req: write("README.md", "# New\n"), code: knowledge.CodeAlreadyExists, wantVersion: blobID(readme)},
		{name: "create over a file the index skips", req: write("huge.md", "# New\n"), code: knowledge.CodeAlreadyExists},
		{name: "create over a folder", req: write("dir.md", "# New\n"), code: knowledge.CodeAlreadyExists},
		{name: "create under a file", req: write("README.md/x.md", "# New\n"), code: knowledge.CodeAlreadyExists},
		{name: "create over a name git stores in NFD", req: write("menü/brötchen.md", "# New\n"), code: knowledge.CodeAlreadyExists},
		{name: "create in another letter case", req: write("readme.md", "# New\n"), code: knowledge.CodeInvalid},
		{name: "create in a folder in another letter case", req: write("Services/new.md", "# New\n"), code: knowledge.CodeInvalid},
		{name: "create not markdown", req: write("notes2.txt", "# New\n"), code: knowledge.CodeInvalid},
		{name: "content that doesn't parse", req: write("new.md", "---\ntitle: [\n---\n# A\n"), code: knowledge.CodeInvalid},
		{name: "content that fails the checks", req: write("new.md", "---\ntitle: A\nstatus: wip\n---\n"), code: knowledge.CodeInvalid},
		{name: "content too large", req: write("new.md", "# A\n"+strings.Repeat("x", 1<<20)), code: knowledge.CodeTooLarge},
		{name: "replace a stale version", req: knowledge.WriteRequest{Vault: "ours/kb", Path: "README.md", Version: "0000", Content: "# Stale\n"}, code: knowledge.CodeConflict, wantVersion: blobID(readme)},
		{name: "replace a missing document", req: knowledge.WriteRequest{Vault: "ours/kb", Path: "gone.md", Version: "0000", Content: "# Gone\n"}, code: knowledge.CodeNotFound},

		{name: "edit without old text", req: edit("", "x"), code: knowledge.CodeInvalid},
		{name: "edit of text that isn't there", req: edit("not there", "x"), code: knowledge.CodeInvalid},
		{name: "edit of text that's there twice", req: edit("Edge proxy", "Proxy"), code: knowledge.CodeInvalid, wantMessage: "2 times"},
		{name: "edit of overlapping matches", req: knowledge.EditRequest{Vault: "ours/kb", Path: "body.md", OldText: "ab ab", NewText: "x"}, code: knowledge.CodeInvalid, wantMessage: "2 times"},
		{name: "create with a key the vault doesn't allow", req: write("new.md", "---\ntitle: A\npublish: true\n---\n"), code: knowledge.CodeInvalid, wantMessage: `"publish" can't be added`},
		{name: "edit adding a key the vault doesn't allow", req: edit("tags: [edge]", "tags: [edge]\npublish: true"), code: knowledge.CodeInvalid, wantMessage: `"publish" can't be added`},
		{name: "create with related to a missing document", req: write("new.md", "---\ntitle: A\nrelated: [README.md, nope.md, \"[[nope]]\"]\n---\n"), code: knowledge.CodeInvalid, wantMessage: `related: "nope.md" isn't a document in this vault; related: "[[nope]]"`},
		{name: "edit to content that fails the checks", req: edit("tags: [edge]", "status: wip"), code: knowledge.CodeInvalid},
		{name: "edit keeping frontmatter that doesn't parse", req: knowledge.EditRequest{Vault: "ours/kb", Path: "broken.md", OldText: "Body text.", NewText: "Better text."}, code: knowledge.CodeInvalid},
		{name: "edit of a stale version", req: knowledge.EditRequest{Vault: "ours/kb", Path: "services/db/pg.md", OldText: "Postgres", NewText: "PG", Version: "0000"}, code: knowledge.CodeConflict, wantVersion: blobID(postgres)},
		{name: "edit of a missing document", req: knowledge.EditRequest{Vault: "ours/kb", Path: "gone.md", OldText: "x", NewText: "y"}, code: knowledge.CodeNotFound},

		{name: "move without a version", req: knowledge.MoveRequest{Vault: "ours/kb", From: "taken.md", To: "z.md"}, code: knowledge.CodeInvalid},
		{name: "move onto a document", req: knowledge.MoveRequest{Vault: "ours/kb", From: "café.md", To: "taken.md", Version: blobID("# Café\n")}, code: knowledge.CodeAlreadyExists, wantVersion: blobID("# Taken\n")},
		{name: "move of a stale version", req: knowledge.MoveRequest{Vault: "ours/kb", From: "café.md", To: "z.md", Version: "0000"}, code: knowledge.CodeConflict, wantVersion: blobID("# Café\n")},
		{name: "move to the same path in NFD", req: knowledge.MoveRequest{Vault: "ours/kb", From: "café.md", To: nfd("café.md"), Version: blobID("# Café\n")}, code: knowledge.CodeInvalid},

		{name: "delete without a version", req: knowledge.DeleteRequest{Vault: "ours/kb", Path: "taken.md"}, code: knowledge.CodeInvalid},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			g := f.grant
			if testCase.grant != nil {
				g = testCase.grant
			}
			_, err := f.change(t.Context(), g, testCase.req)
			kerr := wantError(t, err, testCase.code)
			if kerr.Version != testCase.wantVersion {
				t.Errorf("got version %q, want %q", kerr.Version, testCase.wantVersion)
			}
			if !strings.Contains(kerr.Message, testCase.wantMessage) {
				t.Errorf("got message %q, want it to contain %q", kerr.Message, testCase.wantMessage)
			}
			if n := len(f.github.sent()); n != 0 {
				t.Errorf("got %d commits, want none", n)
			}
		})
	}
}

// A push that leaves a document unparseable must not let an agent overwrite it with the version it read
// before. Delete with the current version is the only change allowed.
func TestBrokenDocument(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const path = "services/edge-proxy.md"
	old := blobID(edgeProxy)
	bad := "---\ntitle: Edge proxy\n---\n# Edge proxy\n\nA human's edit \xff\n"
	f.copy(ours.RepoID).apply(t, "c2", path, []byte(bad))
	now := blobID(bad)

	_, err := f.base.Read(t.Context(), f.grant, knowledge.ReadRequest{Vault: "ours/kb", Path: path})
	kerr := wantError(t, err, knowledge.CodeBroken)
	if kerr.Version != now || !strings.Contains(kerr.Message, "not UTF-8") {
		t.Errorf("got read error %+v, want broken at version %s", kerr, now)
	}

	content := "---\ntitle: Edge proxy\n---\nOverwritten.\n"
	testCases := []struct {
		name string
		req  any
	}{
		{name: "edit, old version", req: knowledge.EditRequest{Vault: "ours/kb", Path: path, OldText: "Edge", NewText: "X", Version: old}},
		{name: "edit, current version", req: knowledge.EditRequest{Vault: "ours/kb", Path: path, OldText: "Edge", NewText: "X", Version: now}},
		{name: "replace, old version", req: knowledge.WriteRequest{Vault: "ours/kb", Path: path, Content: content, Version: old}},
		{name: "replace, current version", req: knowledge.WriteRequest{Vault: "ours/kb", Path: path, Content: content, Version: now}},
		{name: "create", req: knowledge.WriteRequest{Vault: "ours/kb", Path: path, Content: content}},
		{name: "move, old version", req: knowledge.MoveRequest{Vault: "ours/kb", From: path, To: "x.md", Version: old}},
		{name: "move, current version", req: knowledge.MoveRequest{Vault: "ours/kb", From: path, To: "x.md", Version: now}},
	}
	// Not parallel itself, so the delete below waits for the refusals.
	t.Run("refused", func(t *testing.T) {
		for _, testCase := range testCases {
			t.Run(testCase.name, func(t *testing.T) {
				t.Parallel()
				_, err := f.change(t.Context(), f.grant, testCase.req)
				wantCode(t, err, knowledge.CodeBroken)
			})
		}
	})
	if n := len(f.github.sent()); n != 0 {
		t.Fatalf("got %d commits, want none", n)
	}

	_, err = f.base.Delete(t.Context(), f.grant, knowledge.DeleteRequest{Vault: "ours/kb", Path: path, Version: old})
	wantCode(t, err, knowledge.CodeConflict)
	_, err = f.base.Delete(t.Context(), f.grant, knowledge.DeleteRequest{Vault: "ours/kb", Path: path, Version: now})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if diff := cmp.Diff([]gh.FileChange{{Path: path}}, f.lastCommit(t).Changes); diff != "" {
		t.Errorf("delete changes (-want +got):\n%s", diff)
	}
}
