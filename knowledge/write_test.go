package knowledge_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus"
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
			req:         knowledge.EditRequest{Vault: "ours/kb", Path: "services/esi-proxy.md", OldText: "Run the job.", NewText: "Run the deploy job."},
			wantChanges: []gh.FileChange{{Path: "services/esi-proxy.md", Content: []byte(strings.Replace(strings.Replace(esiProxy, "Run the job.", "Run the deploy job.", 1), "tags: [esi]\n", "tags: [esi]\nupdated: "+nowStamp+"\n", 1))}},
			wantMessage: "Update services/esi-proxy.md",
			wantResult:  knowledge.Result{Version: blobID(strings.Replace(strings.Replace(esiProxy, "Run the job.", "Run the deploy job.", 1), "tags: [esi]\n", "tags: [esi]\nupdated: "+nowStamp+"\n", 1))},
		},
		{
			name:        "edit every match",
			req:         knowledge.EditRequest{Vault: "ours/kb", Path: "services/esi-proxy.md", OldText: "ESI proxy", NewText: "ESI gateway", ReplaceAll: true},
			wantChanges: []gh.FileChange{{Path: "services/esi-proxy.md", Content: []byte(strings.Replace(strings.ReplaceAll(esiProxy, "ESI proxy", "ESI gateway"), "tags: [esi]\n", "tags: [esi]\nupdated: "+nowStamp+"\n", 1))}},
			wantMessage: "Update services/esi-proxy.md",
			wantResult:  knowledge.Result{Version: blobID(strings.Replace(strings.ReplaceAll(esiProxy, "ESI proxy", "ESI gateway"), "tags: [esi]\n", "tags: [esi]\nupdated: "+nowStamp+"\n", 1))},
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
			req:         knowledge.WriteRequest{Vault: "ours/kb", Path: "services/new.md", Content: "---\ntitle: New\nrelated: [esi-proxy.md, /README.md]\n---\n"},
			wantChanges: []gh.FileChange{{Path: "services/new.md", Content: []byte("---\ntitle: New\nrelated: [esi-proxy.md, /README.md]\n" + dated + "---\n")}},
			wantMessage: "Create services/new.md",
			wantResult:  knowledge.Result{Version: blobID("---\ntitle: New\nrelated: [esi-proxy.md, /README.md]\n" + dated + "---\n")},
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

func TestCommitMessage(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		message string
		client  string
		path    string // of a document to delete, to get the default message; a.md is created when empty
		want    string
	}{
		{name: "default", client: "Claude Code", want: "Create a.md\n\nSpeki-Client: Claude Code"},
		{name: "agent's first line, forged trailer dropped", message: "Fix typo\n\nSpeki-Client: forged", client: "claude.ai", want: "Fix typo\n\nSpeki-Client: claude.ai"},
		{name: "only a forged trailer", message: "Speki-Client: forged", client: "x", want: "Create a.md\n\nSpeki-Client: x"},
		{name: "control characters, no client", message: "  tabs\tand\u202econtrol\x00  ", want: "tabs andcontrol\n\nSpeki-Client: unknown"},
		{name: "long line, client with a line break", message: strings.Repeat("é", 150), client: "c\nlient", want: strings.Repeat("é", 100) + "\n\nSpeki-Client: c lient"},
		// Default messages name paths from git, which may hold line breaks and format characters.
		{name: "path with a line break", path: "a\nSpeki-Client: x.md", client: "c", want: "Delete a Speki-Client: x.md\n\nSpeki-Client: c"},
		{name: "path with a format character", path: "a\u202eb.md", client: "c", want: "Delete ab.md\n\nSpeki-Client: c"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var f *fixture
			var req any = knowledge.WriteRequest{Vault: "ours/kb", Path: "a.md", Content: "# A\n", Message: testCase.message}
			if testCase.path != "" {
				f = newFixture(t, withKB(map[string]string{testCase.path: "# A\n"}))
				req = knowledge.DeleteRequest{Vault: "ours/kb", Path: testCase.path, Version: blobID("# A\n"), Message: testCase.message}
			} else {
				f = newFixture(t, withKB(map[string]string{}))
			}
			g := access.NewGrant(access.User{ID: 7, Token: "ghu_x", Client: testCase.client, Write: true}, []vault.Vault{ours}, []int64{ours.RepoID})
			if _, err := f.change(t.Context(), g, req); err != nil {
				t.Fatalf("change: %v", err)
			}
			if got := withoutRequestID(f.lastCommit(t).Message); got != testCase.want {
				t.Errorf("got message %q, want %q", got, testCase.want)
			}
		})
	}
}

// Changes that are refused before anything is committed.
func TestChangeRejects(t *testing.T) {
	t.Parallel()
	nfd := norm.NFD.String
	f := newFixture(t, withKB(map[string]string{
		"README.md":             readme,
		"services/esi-proxy.md": esiProxy,
		"services/db/pg.md":     postgres,
		"notes.txt":             "x",
		"dir.md/inside.md":      "# Inside\n",
		nfd("menü/brötchen.md"): "# Brötchen\n",
		"café.md":               "# Café\n",
		"body.md":               "# B\n\nab ab ab\n",
		"taken.md":              "# Taken\n",
		"broken.md":             "---\ntitle: [\n---\nBody text.\n",
	}))
	f.copy(ours.RepoID).setFile("c1", "huge.md", []byte("too large to index"))
	readOnly := access.NewGrant(access.User{Login: "octocat", Token: "ghu_x"}, []vault.Vault{ours, shared}, []int64{ours.RepoID})
	write := func(path, content string) knowledge.WriteRequest {
		return knowledge.WriteRequest{Vault: "ours/kb", Path: path, Content: content}
	}
	edit := func(oldText, newText string) knowledge.EditRequest {
		return knowledge.EditRequest{Vault: "ours/kb", Path: "services/esi-proxy.md", OldText: oldText, NewText: newText}
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
		{name: "delete in a vault the user can't read", req: knowledge.DeleteRequest{Vault: "ours/secret", Path: "services/esi-proxy.md", Version: "v"}, code: knowledge.CodeNotFound},

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
		{name: "edit of text that's there twice", req: edit("ESI proxy", "Proxy"), code: knowledge.CodeInvalid, wantMessage: "2 times"},
		{name: "edit of overlapping matches", req: knowledge.EditRequest{Vault: "ours/kb", Path: "body.md", OldText: "ab ab", NewText: "x"}, code: knowledge.CodeInvalid, wantMessage: "2 times"},
		{name: "create with a key the vault doesn't allow", req: write("new.md", "---\ntitle: A\npublish: true\n---\n"), code: knowledge.CodeInvalid, wantMessage: `"publish" can't be added`},
		{name: "edit adding a key the vault doesn't allow", req: edit("tags: [esi]", "tags: [esi]\npublish: true"), code: knowledge.CodeInvalid, wantMessage: `"publish" can't be added`},
		{name: "create with related to a missing document", req: write("new.md", "---\ntitle: A\nrelated: [README.md, nope.md, \"[[nope]]\"]\n---\n"), code: knowledge.CodeInvalid, wantMessage: `related: "nope.md" isn't a document in this vault; related: "[[nope]]"`},
		{name: "edit to content that fails the checks", req: edit("tags: [esi]", "status: wip"), code: knowledge.CodeInvalid},
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

// The agent read a version GitHub has but speki's index doesn't yet: speki syncs and finds it.
func TestWriteSyncsWhenTheIndexIsBehind(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	newer := "---\ntitle: About\n---\nChanged on GitHub.\n"
	f.github.push("ours/kb", "README.md", []byte(newer))

	_, err := f.base.Write(t.Context(), f.grant, knowledge.WriteRequest{Vault: "ours/kb", Path: "README.md", Version: blobID(newer), Content: "---\ntitle: About\n---\nMine.\n"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := f.lastCommit(t).ExpectedHead; got != "push-1" {
		t.Errorf("got a commit based on %s, want push-1", got)
	}
}

// A blob the copy no longer has (a fresh clone dropped it) is fetched again rather than failing.
func TestWriteRecoversFromAMissingBlob(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.copy(ours.RepoID).failBlobs(1)
	_, err := f.base.Write(t.Context(), f.grant, knowledge.WriteRequest{Vault: "ours/kb", Path: "README.md", Version: blobID(readme), Content: "---\ntitle: About\n---\nNew.\n"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := f.vaults.syncCount(); got != 2 { // the extra one, then the one after the commit
		t.Errorf("got %d syncs, want 2", got)
	}
}

func TestWriteDuringARebuild(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.github.push("ours/kb", "README.md", []byte("# Changed\n"))
	f.vaults.setSyncErr(knowledge.ErrRebuilding)
	_, err := f.base.Write(t.Context(), f.grant, knowledge.WriteRequest{Vault: "ours/kb", Path: "README.md", Version: blobID(readme), Content: "# Mine\n"})
	kerr := wantError(t, err, knowledge.CodeUnavailable)
	if !strings.Contains(kerr.Message, "rebuilding") {
		t.Errorf("got %q, want it to say the vault is rebuilding", kerr.Message)
	}
}

func TestIndexPending(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.github.before = func(*fakeRepo) { f.vaults.setSyncErr(errors.New("fetch timed out")) }
	res, err := f.base.Write(t.Context(), f.grant, knowledge.WriteRequest{Vault: "ours/kb", Path: "new.md", Content: "# New\n"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !res.IndexPending || res.Commit != "commit-1" {
		t.Errorf("got %+v, want commit-1 with the index pending", res)
	}
}

func TestCommitRetriesWhenTheBranchMoves(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name        string
		moves       int // how many attempts find the branch moved
		req         any
		code        knowledge.Code
		wantVersion string
	}{
		{
			name:  "lands on the last attempt",
			moves: knowledge.CommitAttempts - 1,
			req:   knowledge.EditRequest{Vault: "ours/kb", Path: "README.md", OldText: "zeppelin", NewText: "airship"},
		},
		{
			name:        "keeps moving",
			moves:       knowledge.CommitAttempts,
			req:         knowledge.EditRequest{Vault: "ours/kb", Path: "README.md", OldText: "zeppelin", NewText: "airship"},
			code:        knowledge.CodeConflict,
			wantVersion: blobID(readme),
		},
		{
			name:        "keeps moving under a move",
			moves:       knowledge.CommitAttempts,
			req:         knowledge.MoveRequest{Vault: "ours/kb", From: "README.md", To: "about.md", Version: blobID(readme)},
			code:        knowledge.CodeConflict,
			wantVersion: blobID(readme), // of the source
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			moves := testCase.moves
			f.github.before = func(r *fakeRepo) {
				if moves > 0 {
					moves--
					r.head += "+someone-else"
				}
			}
			_, err := f.change(t.Context(), f.grant, testCase.req)
			if n := len(f.github.sent()); n != knowledge.CommitAttempts {
				t.Errorf("got %d commit attempts, want %d", n, knowledge.CommitAttempts)
			}
			if testCase.code == "" {
				if err != nil {
					t.Errorf("got %v, want no error", err)
				}
				return
			}
			kerr := wantError(t, err, testCase.code)
			if kerr.Version != testCase.wantVersion || !strings.Contains(kerr.Message, testCase.wantVersion) {
				t.Errorf("got %+v, want a conflict with version %s", kerr, testCase.wantVersion)
			}
		})
	}
}

// GitHub's refusals reach the agent as codes, without looking for a commit that can't have landed.
func TestCommitErrors(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name string
		err  error
		code knowledge.Code // empty: a plain error
	}{
		{name: "secret", err: &gh.SecretError{Detail: "GitHub Personal Access Token"}, code: knowledge.CodeSecretDetected},
		{name: "branch rules", err: gh.ErrBranchRules, code: knowledge.CodeVaultReadOnly},
		{name: "forbidden", err: gh.ErrForbidden, code: knowledge.CodeNoWriteAccess},
		{name: "rate limited", err: &gh.RateLimitError{RetryAfter: 30 * time.Second}, code: knowledge.CodeRateLimited},
		{name: "signed out", err: gh.ErrUnauthorized, code: knowledge.CodeUnavailable},
		{name: "refused", err: fmt.Errorf("%w: something (UNPROCESSABLE)", gh.ErrRefused)},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.github.err = testCase.err
			_, err := f.base.Write(t.Context(), f.grant, knowledge.WriteRequest{Vault: "ours/kb", Path: "new.md", Content: "# New\n"})
			if testCase.code != "" {
				wantCode(t, err, testCase.code)
			} else if _, ok := errors.AsType[*knowledge.Error](err); ok || err == nil {
				t.Errorf("got %v, want a plain error", err)
			}
			if f.github.lookups != 0 {
				t.Errorf("got %d lookups for the commit, want none", f.github.lookups)
			}
		})
	}
}

// When GitHub's answer to a commit is lost, speki looks for the commit, twice, so the agent doesn't
// retry an edit that would apply twice. synctest stands in for the wait between the lookups.
func TestLostAnswerToACommit(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name        string
		err         error // Commit fails with it, landing nothing
		lostErr     error // Commit lands, then fails with it
		wantLanded  bool
		wantLookups int
	}{
		{name: "landed", lostErr: errors.New("502 Bad Gateway"), wantLanded: true, wantLookups: 1},
		{name: "never landed", err: errors.New("connection reset"), wantLookups: 2},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t)
				f.github.err, f.github.lostErr = testCase.err, testCase.lostErr
				res, err := f.base.Edit(t.Context(), f.grant, knowledge.EditRequest{Vault: "ours/kb", Path: "README.md", OldText: "# About", NewText: "# About\n\nAn intro."})
				if f.github.lookups != testCase.wantLookups {
					t.Errorf("got %d lookups, want %d", f.github.lookups, testCase.wantLookups)
				}
				if !testCase.wantLanded {
					if _, ok := errors.AsType[*knowledge.Error](err); ok || err == nil {
						t.Errorf("got %v, want a plain error", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("Edit: %v", err)
				}
				if res.Commit != f.github.head("ours/kb") || res.Version != f.version(t, "README.md") {
					t.Errorf("got %+v, want the commit speki made, at the indexed version", res)
				}
			})
		})
	}
}

func TestConcurrentEdits(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for _, e := range []knowledge.EditRequest{
		{Path: "services/esi-proxy.md", OldText: "Run the job.", NewText: "Run the deploy job."},
		{Path: "services/esi-proxy.md", OldText: "caches", NewText: "stores"},
		{Path: "services/db/pg.md", OldText: "Zeppelin database.", NewText: "Main database."},
	} {
		wg.Go(func() {
			e.Vault = "ours/kb"
			_, err := f.base.Edit(ctx, f.grant, e)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Edit: %v", err)
		}
	}
	got := f.read(t, "services/esi-proxy.md").Content
	if !strings.Contains(got, "Run the deploy job.") || !strings.Contains(got, "stores") {
		t.Errorf("got\n%s\nwant both edits", got)
	}

	// Two writes from the same version: one wins, the other conflicts.
	v := f.version(t, "README.md")
	results := make(chan error, 2)
	for _, text := range []string{"One.", "Two."} {
		wg.Go(func() {
			_, err := f.base.Write(ctx, f.grant, knowledge.WriteRequest{Vault: "ours/kb", Path: "README.md", Version: v, Content: "---\ntitle: About\n---\n" + text + "\n"})
			results <- err
		})
	}
	wg.Wait()
	close(results)
	var ok, conflicts int
	for err := range results {
		kerr, isKerr := errors.AsType[*knowledge.Error](err)
		switch {
		case err == nil:
			ok++
		case isKerr && kerr.Code == knowledge.CodeConflict:
			conflicts++
		default:
			t.Errorf("Write: %v", err)
		}
	}
	if ok != 1 || conflicts != 1 {
		t.Errorf("got %d succeeded and %d conflicted, want 1 and 1", ok, conflicts)
	}
}

// A change queued behind a slow one gives up when its request ends, or once the lock wait runs out
// while its request still has time. synctest stands in for the lock wait.
func TestWriteLockWait(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		timeout time.Duration // of the queued request; none when zero
		code    knowledge.Code
		wantErr error
	}{
		{name: "request ends first", timeout: time.Second, wantErr: context.DeadlineExceeded},
		{name: "lock wait runs out", code: knowledge.CodeUnavailable},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t)
				committing, release := make(chan struct{}), make(chan struct{})
				f.github.before = func(*fakeRepo) {
					close(committing)
					<-release
				}
				slow := make(chan error, 1)
				go func() {
					_, err := f.base.Write(t.Context(), f.grant, knowledge.WriteRequest{Vault: "ours/kb", Path: "slow.md", Content: "# Slow\n"})
					slow <- err
				}()
				<-committing

				ctx := t.Context()
				if testCase.timeout > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, testCase.timeout)
					defer cancel()
				}
				_, err := f.base.Write(ctx, f.grant, knowledge.WriteRequest{Vault: "ours/kb", Path: "queued.md", Content: "# Queued\n"})
				if testCase.wantErr != nil && !errors.Is(err, testCase.wantErr) {
					t.Errorf("got %v, want %v", err, testCase.wantErr)
				}
				if testCase.code != "" {
					kerr := wantError(t, err, testCase.code)
					if !strings.Contains(kerr.Message, "another change to ours/kb is still being committed") {
						t.Errorf("got %q, want it to name the change being committed", kerr.Message)
					}
				}
				close(release)
				if err := <-slow; err != nil {
					t.Errorf("slow write: got %v, want no error", err)
				}
			})
		})
	}
}

// A commit that lands while the sync after it is slow is a success with its SHA. The sync and the notice
// finish after the answer, and Stop waits for them. synctest stands in for the request's deadline.
func TestCommitLandsWhileItsSyncIsSlow(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.vaults.hold = make(chan struct{}) // a new document: the only sync is the one after the commit
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		res, err := f.base.Write(ctx, f.grant, knowledge.WriteRequest{Vault: "ours/kb", Path: "new.md", Content: "# New\n"})
		if err != nil {
			t.Fatalf("write whose sync is slow: %v", err)
		}
		if res.Commit != "commit-1" || !res.IndexPending {
			t.Errorf("got %+v, want commit-1 with the index pending", res)
		}
		if got := f.notices.all(); len(got) != 0 {
			t.Errorf("got notices %v before the sync finished, want none", got)
		}

		close(f.vaults.hold)
		if err := f.base.Stop(t.Context()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if diff := cmp.Diff([]string{"1:commit-1"}, f.notices.all()); diff != "" {
			t.Errorf("notices (-want +got):\n%s", diff)
		}

		// Once stopped, a change still answers; the pollers catch up with it.
		res, err = f.base.Write(t.Context(), f.grant, knowledge.WriteRequest{Vault: "ours/kb", Path: "later.md", Content: "# Later\n"})
		if err != nil || res.Commit != "commit-2" || !res.IndexPending {
			t.Errorf("write after Stop: got %+v, %v, want commit-2 with the index pending", res, err)
		}
	})
}

// A push that leaves a document unparseable must not let an agent overwrite it with the version it read
// before. Delete with the current version is the only change allowed.
func TestBrokenDocument(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const path = "services/esi-proxy.md"
	old := blobID(esiProxy)
	bad := "---\ntitle: ESI proxy\n---\n# ESI proxy\n\nA human's edit \xff\n"
	f.copy(ours.RepoID).apply(t, "c2", path, []byte(bad))
	now := blobID(bad)

	_, err := f.base.Read(t.Context(), f.grant, knowledge.ReadRequest{Vault: "ours/kb", Path: path})
	kerr := wantError(t, err, knowledge.CodeBroken)
	if kerr.Version != now || !strings.Contains(kerr.Message, "not UTF-8") {
		t.Errorf("got read error %+v, want broken at version %s", kerr, now)
	}

	content := "---\ntitle: ESI proxy\n---\nOverwritten.\n"
	testCases := []struct {
		name string
		req  any
	}{
		{name: "edit, old version", req: knowledge.EditRequest{Vault: "ours/kb", Path: path, OldText: "ESI", NewText: "X", Version: old}},
		{name: "edit, current version", req: knowledge.EditRequest{Vault: "ours/kb", Path: path, OldText: "ESI", NewText: "X", Version: now}},
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

// guardMismatches reads speki_write_guard_mismatches_total.
func guardMismatches(tb testing.TB) float64 {
	tb.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		tb.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() == "speki_write_guard_mismatches_total" {
			return mf.GetMetric()[0].GetCounter().GetValue()
		}
	}
	tb.Fatal("got no speki_write_guard_mismatches_total, want the counter")
	return 0
}

// Before committing, the index's version is checked against git: an index that fell behind the tree must
// not let a change overwrite what it doesn't know about. Not parallel: it counts a global metric.
func TestChangeRefusesWhenTheIndexDisagreesWithGit(t *testing.T) {
	const path = "services/esi-proxy.md"
	v := blobID(esiProxy)
	changed := "# ESI proxy\n\nChanged in git.\n"
	changes := []struct {
		name string
		req  any
	}{
		{name: "edit", req: knowledge.EditRequest{Vault: "ours/kb", Path: path, OldText: "Run the job.", NewText: "Run it.", Version: v}},
		{name: "replace", req: knowledge.WriteRequest{Vault: "ours/kb", Path: path, Content: "# ESI proxy\n\nReplaced.\n", Version: v}},
		{name: "move", req: knowledge.MoveRequest{Vault: "ours/kb", From: path, To: "services/proxy.md", Version: v}},
		{name: "delete", req: knowledge.DeleteRequest{Vault: "ours/kb", Path: path, Version: v}},
	}
	testCases := []struct {
		name    string
		drift   []byte // nil deletes the file
		version string
	}{
		{name: "changed", drift: []byte(changed), version: blobID(changed)},
		{name: "deleted", drift: nil, version: ""},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var logs syncBuffer
			f := newFixture(t, withLogger(slog.New(slog.NewTextHandler(&logs, nil))))
			f.copy(ours.RepoID).setFile("c1", path, testCase.drift)
			before := guardMismatches(t)

			for _, change := range changes {
				_, err := f.change(t.Context(), f.grant, change.req)
				kerr := wantError(t, err, knowledge.CodeConflict)
				if kerr.Version != testCase.version {
					t.Errorf("%s: got conflict version %q, want %q", change.name, kerr.Version, testCase.version)
				}
			}
			if n := len(f.github.sent()); n != 0 {
				t.Errorf("got %d commits, want none", n)
			}
			if got := guardMismatches(t) - before; got != float64(len(changes)) {
				t.Errorf("got %v guard mismatches counted, want %d", got, len(changes))
			}
			want := fmt.Sprintf("level=WARN msg=\"index and git still disagree on a document after a sync\" vault=ours/kb path=%s commit=c1 index_version=%s git_version=%s", path, v, testCase.version)
			if n := strings.Count(logs.String(), want); n != len(changes) {
				t.Errorf("got %d warnings like %q in:\n%s\nwant %d", n, want, logs.String(), len(changes))
			}
		})
	}
}

// syncBuffer is a bytes.Buffer that work after a commit may log to while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The guard checks the document at the index's commit, not at a newer one the mirror already fetched.
// Not parallel: it counts a global metric.
func TestChangeChecksTheIndexHead(t *testing.T) {
	const path = "services/esi-proxy.md"
	f := newFixture(t)
	f.copy(ours.RepoID).setFile("c2", path, []byte("# ESI proxy\n\nFetched, not indexed yet.\n"))
	before := guardMismatches(t)

	_, err := f.base.Edit(t.Context(), f.grant, knowledge.EditRequest{Vault: "ours/kb", Path: path, OldText: "Run the job.", NewText: "Run it.", Version: blobID(esiProxy)})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if got := f.lastCommit(t).ExpectedHead; got != "c1" {
		t.Errorf("got expected head %q, want c1", got)
	}
	if got := guardMismatches(t) - before; got != 0 {
		t.Errorf("got %v guard mismatches counted, want 0", got)
	}
}
