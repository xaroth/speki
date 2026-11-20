package knowledge_test

import (
	"strings"
	"testing"

	"github.com/xaroth/speki/document"
	"github.com/xaroth/speki/knowledge"
)

func TestOKFChanges(t *testing.T) {
	t.Parallel()
	const generated = "generated:\n  by: claude-code\n  at: " + nowStamp + "\n"
	v02 := map[string]string{
		"index.md":           "---\nokf_version: \"0.2\"\n---\n# KB\n\n* [Deploy](./deploy.md) - how we deploy\n",
		"ops/index.md":       "# Ops\n",
		"ops/log.md":         "# Log\n\n## 2026-10-01\n* **Creation**: ops.\n",
		"deploy.md":          "---\ntype: Guide\ntitle: Deploy\ngenerated:\n  by: human:alice\n  at: 2026-01-01T00:00:00Z\n  note: kept\n---\n# Deploy\n",
		"untyped.md":         "---\ntitle: Untyped\n---\n# Untyped\n",
		"old.md":             "---\ntype: Reference\ntimestamp: 2026-01-01T00:00:00Z\n---\n# Old\n",
		"dated.md":           "---\ntype: Reference\nupdated: 2026-01-01T00:00:00Z\n---\n# Dated\n",
		"ops/with-front.md":  "---\ntype: Note\n---\n# Note\n",
		"ops/sub/index.md":   "---\ntitle: Sub\n---\n# Sub\n",
		"ops/sub/concept.md": "---\ntype: Note\n---\n# Concept\n",
	}
	v01 := map[string]string{"index.md": "---\nokf_version: \"0.1\"\n---\n# KB\n"}
	bare := map[string]string{"index.md": "# KB\n", "a.md": "---\ntype: Note\nverified: {by: human:alice, at: 2026-01-01T00:00:00Z}\n---\n# A\n"}
	write := func(path, content string) knowledge.WriteRequest {
		return knowledge.WriteRequest{Vault: "ours/kb", Path: path, Content: content}
	}
	replace := func(files map[string]string, path, content string) knowledge.WriteRequest {
		return knowledge.WriteRequest{Vault: "ours/kb", Path: path, Version: blobID(files[path]), Content: content}
	}
	testCases := []struct {
		name    string
		files   map[string]string
		req     knowledge.WriteRequest
		want    string // the committed file
		wantErr string // part of the error, instead
	}{
		{
			name:  "a new concept gets generated",
			files: v02,
			req:   write("runbooks/restart.md", "---\ntype: Playbook\ntitle: Restart\nverified: {by: human:octocat, at: 2026-10-01T00:00:00Z}\n---\n# Restart\n"),
			want:  "---\ntype: Playbook\ntitle: Restart\nverified: {by: 'human:octocat', at: '2026-10-01T00:00:00Z'}\n" + generated + "---\n# Restart\n",
		},
		{
			name:  "a new concept in a v0.1 vault gets timestamp",
			files: v01,
			req:   write("restart.md", "---\ntype: Playbook\n---\n# Restart\n"),
			want:  "---\ntype: Playbook\ntimestamp: " + nowStamp + "\n---\n# Restart\n",
		},
		{
			name:  "generated keeps its other keys",
			files: v02,
			req:   replace(v02, "deploy.md", strings.Replace(v02["deploy.md"], "# Deploy\n", "# Deploy\n\nNew.\n", 1)),
			want:  "---\ntype: Guide\ntitle: Deploy\ngenerated:\n  by: claude-code\n  at: " + nowStamp + "\n  note: kept\n---\n# Deploy\n\nNew.\n",
		},
		{
			name:  "timestamp stays timestamp",
			files: v02,
			req:   replace(v02, "old.md", "---\ntype: Reference\ntimestamp: 2026-01-01T00:00:00Z\n---\n# Old\n\nNew.\n"),
			want:  "---\ntype: Reference\ntimestamp: " + nowStamp + "\n---\n# Old\n\nNew.\n",
		},
		{
			name:  "updated is kept current",
			files: v02,
			req:   replace(v02, "dated.md", "---\ntype: Reference\nupdated: 2026-01-01T00:00:00Z\n---\n# Dated\n\nNew.\n"),
			want:  "---\ntype: Reference\nupdated: " + nowStamp + "\n" + generated + "---\n# Dated\n\nNew.\n",
		},
		{
			name:  "a concept that had no type may keep going without one",
			files: v02,
			req:   replace(v02, "untyped.md", "---\ntitle: Untyped\n---\n# Untyped\n\nNew.\n"),
			want:  "---\ntitle: Untyped\n" + generated + "---\n# Untyped\n\nNew.\n",
		},
		{
			name:  "a nested index.md isn't dated",
			files: v02,
			req:   replace(v02, "ops/index.md", "# Ops\n\n* [Note](./with-front.md) - a note\n"),
			want:  "# Ops\n\n* [Note](./with-front.md) - a note\n",
		},
		{
			name:  "a nested index.md that has frontmatter may keep it",
			files: v02,
			req:   replace(v02, "ops/sub/index.md", "---\ntitle: Sub\n---\n# Sub\n\nMore.\n"),
			want:  "---\ntitle: Sub\n---\n# Sub\n\nMore.\n",
		},
		{
			name:  "the root index.md isn't dated",
			files: v02,
			req:   replace(v02, "index.md", "---\nokf_version: \"0.2\"\n---\n# KB\n"),
			want:  "---\nokf_version: \"0.2\"\n---\n# KB\n",
		},
		{
			name:  "log.md isn't dated",
			files: v02,
			req:   replace(v02, "ops/log.md", "# Log\n\n## 2026-10-09\n* **Update**: more.\n"),
			want:  "# Log\n\n## 2026-10-09\n* **Update**: more.\n",
		},
		{
			name:  "the root index.md may gain okf_version",
			files: bare,
			req:   replace(bare, "index.md", "---\nokf_version: \"0.2\"\n---\n# KB\n"),
			want:  "---\nokf_version: \"0.2\"\n---\n# KB\n",
		},
		{
			name:  "a new log.md needs no title",
			files: v02,
			req:   write("ops/sub/log.md", "## 2026-10-09\n* **Creation**: sub.\n"),
			want:  "## 2026-10-09\n* **Creation**: sub.\n",
		},
		{
			name:  "a person's verification already there stays",
			files: bare,
			req:   replace(bare, "a.md", strings.Replace(bare["a.md"], "# A\n", "# A\n\nNew.\n", 1)),
			want:  "---\ntype: Note\nverified: {by: 'human:alice', at: '2026-01-01T00:00:00Z'}\n" + generated + "---\n# A\n\nNew.\n",
		},
		{
			name:    "an agent can't add another person's verification",
			files:   v02,
			req:     write("x.md", "---\ntype: Note\nverified:\n  - {by: process:nightly, at: 2026-10-01T00:00:00Z}\n  - {by: human:boss, at: 2026-10-01T00:00:00Z}\n---\n# X\n"),
			wantErr: `"human:boss" can't be added`,
		},
		{
			name:    "created can't be set",
			files:   v02,
			req:     write("x.md", "---\ntype: Note\ncreated: 1999-01-01T00:00:00Z\n---\n# X\n"),
			wantErr: `"created" can't be added`,
		},
		{
			name:    "a new concept needs a type",
			files:   v02,
			req:     write("restart.md", "---\ntitle: Restart\n---\n"),
			wantErr: "type: missing",
		},
		{
			name:    "a type can't be removed",
			files:   v02,
			req:     replace(v02, "old.md", "---\ntimestamp: 2026-01-01T00:00:00Z\n---\n# Old\n"),
			wantErr: "type: missing",
		},
		{
			name:    "a nested index.md can't gain frontmatter",
			files:   v02,
			req:     replace(v02, "ops/index.md", "---\ntitle: Ops\n---\n# Ops\n"),
			wantErr: "an index.md in an OKF vault has no frontmatter",
		},
		{
			name:    "the root index.md holds only okf_version",
			files:   v02,
			req:     replace(v02, "index.md", "---\nokf_version: \"0.2\"\ntitle: KB\n---\n# KB\n"),
			wantErr: `"title" can't be added`,
		},
		{
			name:    "keys outside OKF's and the vault's stay locked",
			files:   v02,
			req:     write("x.md", "---\ntype: Note\nrisk: high\n---\n# X\n"),
			wantErr: `"risk" can't be added`,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, withKB(testCase.files), withFormat(document.FormatOKF))
			_, err := f.base.Write(t.Context(), f.grant, testCase.req)
			if testCase.wantErr != "" {
				e := wantError(t, err, knowledge.CodeInvalid)
				if !strings.Contains(e.Message, testCase.wantErr) {
					t.Errorf("got error %q, want it to mention %q", e.Message, testCase.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("write: %v", err)
			}
			changes := f.lastCommit(t).Changes
			if len(changes) != 1 {
				t.Fatalf("got %d changes, want 1", len(changes))
			}
			if got := string(changes[0].Content); got != testCase.want {
				t.Errorf("got file\n%s\nwant\n%s", got, testCase.want)
			}
		})
	}
}

func TestOKFMoveRejectsIndexFiles(t *testing.T) {
	t.Parallel()
	files := map[string]string{"index.md": "# KB\n", "a.md": "---\ntype: Note\n---\n# A\n", "ops/log.md": "# Log\n"}
	testCases := []struct {
		name     string
		from, to string
	}{
		{name: "onto an index.md", from: "a.md", to: "ops/index.md"},
		{name: "a log.md", from: "ops/log.md", to: "ops/history.md"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, withKB(files), withFormat(document.FormatOKF))
			_, err := f.base.Move(t.Context(), f.grant, knowledge.MoveRequest{Vault: "ours/kb", From: testCase.from, To: testCase.to, Version: blobID(files[testCase.from])})
			wantCode(t, err, knowledge.CodeInvalid)
		})
	}
}
