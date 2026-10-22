package vault_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/xaroth/speki/gh"
	"github.com/xaroth/speki/vault"
)

func TestParseConfig(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		raw     string
		want    vault.Config
		wantErr []string
	}{
		{
			name: "full",
			raw:  "title: Infra\ndescription: >\n  How we run things.\nguidelines: |\n  One doc per service.\nread_role: triage\nfuture_key: ignored\n",
			want: vault.Config{Title: "Infra", Description: "How we run things.", Guidelines: "One doc per service.", ReadRole: gh.RoleTriage},
		},
		{
			name: "readers and writers",
			raw:  "title: T\ndescription: D\nreaders: [ours/Editors, \"@alice\", \"@ours/leads\"]\nwriters:\n  - ours/leads\n",
			want: vault.Config{
				Title:       "T",
				Description: "D",
				Readers:     []vault.Principal{{Org: "ours", Team: "editors"}, {Login: "alice"}, {Org: "ours", Team: "leads"}},
				Writers:     []vault.Principal{{Org: "ours", Team: "leads"}},
			},
		},
		{
			name: "frontmatter",
			raw:  "title: T\ndescription: D\nfrontmatter:\n  allow: [owner, review_by]\n",
			want: vault.Config{Title: "T", Description: "D", Frontmatter: vault.Frontmatter{Allow: []string{"owner", "review_by"}}},
		},
		{
			name: "repositories",
			raw:  "title: T\ndescription: D\nrepositories:\n  - Xaroth/Speki\n  - git@gitlab.com:team/sub/repo.git\n  - github.com/xaroth/mantine-*\n",
			want: vault.Config{Title: "T", Description: "D", Repositories: vault.Repositories{
				{Host: "github.com", Owner: "xaroth", Name: "speki"},
				{Host: "gitlab.com", Owner: "team/sub", Name: "repo"},
				{Host: "github.com", Owner: "xaroth", Name: "mantine-*"},
			}},
		},
		{name: "repositories null", raw: "title: T\ndescription: D\nrepositories:\n", want: vault.Config{Title: "T", Description: "D"}},
		{name: "repositories empty", raw: "title: T\ndescription: D\nrepositories: []\n", want: vault.Config{Title: "T", Description: "D", Repositories: vault.Repositories{}}},
		{name: "bad repository", raw: "title: T\ndescription: D\nrepositories:\n  - xaroth/speki\n  - https://user:ghp_secret@github.com/xaroth\n", wantErr: []string{"repositories, line 5: not a repository"}},
		{name: "repository pattern in owner", raw: "title: T\ndescription: D\nrepositories: [\"*/speki\"]\n", wantErr: []string{"repositories, line 3: * can only"}},
		{name: "repositories not a list", raw: "title: T\ndescription: D\nrepositories: xaroth/speki\n", wantErr: []string{"repositories, line 3: not a list"}},
		{name: "repository not a string", raw: "title: T\ndescription: D\nrepositories:\n  - {repo: xaroth/speki}\n", wantErr: []string{"repositories, line 4: not a repository"}},
		{name: "repository twice", raw: "title: T\ndescription: D\nrepositories: [xaroth/speki, \"https://github.com/Xaroth/speki.git\"]\n", wantErr: []string{"repositories lists github.com/xaroth/speki twice"}},
		{name: "too many repositories", raw: "title: T\ndescription: D\nrepositories: [" + manyRepositories(51) + "]\n", wantErr: []string{"repositories has 51 entries, at most 50"}},
		{name: "frontmatter set by speki", raw: "title: T\ndescription: D\nfrontmatter:\n  allow: [updated, \"\"]\n", wantErr: []string{"updated is set by speki", "empty key"}},
		{name: "unknown read role", raw: "title: T\ndescription: D\nread_role: owner\n", wantErr: []string{`unknown role "owner"`}},
		{name: "empty readers", raw: "title: T\ndescription: D\nreaders: []\n", wantErr: []string{"readers is empty"}},
		{name: "team elsewhere", raw: "title: T\ndescription: D\nwriters: [theirs/leads]\n", wantErr: []string{"team theirs/leads isn't in Ours"}},
		{name: "bad principal", raw: "title: T\ndescription: D\nreaders: [a/b/c]\n", wantErr: []string{`"a/b/c" is neither`}},
		{name: "missing fields", raw: "guidelines: x\n", wantErr: []string{"title is required", "description is required"}},
		{name: "too long", raw: "title: T\ndescription: " + strings.Repeat("d", 301) + "\n", wantErr: []string{"description is 301 characters"}},
		{name: "not yaml", raw: "title: [\n", wantErr: []string{"speki.yaml"}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, err := vault.ParseConfig([]byte(testCase.raw), "Ours")
			if len(testCase.wantErr) > 0 {
				if err == nil {
					t.Fatalf("got config %+v, want an error", got)
				}
				for _, want := range testCase.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("got error %q, want it to mention %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseConfig: %v", err)
			}
			if diff := cmp.Diff(testCase.want, got); diff != "" {
				t.Errorf("config (-want +got):\n%s", diff)
			}
		})
	}
}

func manyRepositories(n int) string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("xaroth/repo-%d", i)
	}
	return strings.Join(names, ", ")
}

// Vault configurations are stored as JSON with the index, and must come back the same.
func TestConfigJSON(t *testing.T) {
	t.Parallel()
	want := vault.Config{
		Title:   "T",
		Readers: []vault.Principal{{Org: "ours", Team: "editors"}, {Login: "alice"}},
		Writers: []vault.Principal{{Login: "bob"}},
		Repositories: vault.Repositories{
			{Host: "github.com", Owner: "xaroth", Name: "speki"},
			{Host: "gitlab.com", Owner: "team/sub", Name: "mantine-*"},
		},
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got vault.Config
	err = json.Unmarshal(raw, &got)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("round trip (-want +got):\n%s", diff)
	}
}
