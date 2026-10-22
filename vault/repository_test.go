package vault_test

import (
	"strings"
	"testing"

	"github.com/xaroth/speki/vault"
)

func TestParseRemote(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{name: "owner/repo", in: "xaroth/speki", want: "github.com/xaroth/speki"},
		{name: "host/owner/repo", in: "github.com/xaroth/speki", want: "github.com/xaroth/speki"},
		{name: "case", in: "GitHub.com/Xaroth/Speki", want: "github.com/xaroth/speki"},
		{name: "spaces", in: "  xaroth/speki\n", want: "github.com/xaroth/speki"},
		{name: "https", in: "https://github.com/xaroth/speki", want: "github.com/xaroth/speki"},
		{name: "https .git", in: "https://github.com/xaroth/speki.git", want: "github.com/xaroth/speki"},
		{name: "https trailing slash", in: "https://github.com/xaroth/speki/", want: "github.com/xaroth/speki"},
		{name: "https query and fragment", in: "https://github.com/xaroth/speki?tab=readme#top", want: "github.com/xaroth/speki"},
		{name: "https credentials", in: "https://x-access-token:ghp_secret@github.com/xaroth/speki.git", want: "github.com/xaroth/speki"},
		{name: "http port", in: "http://git.example.com:8080/team/repo", want: "git.example.com/team/repo"},
		{name: "ssh url", in: "ssh://git@github.com/xaroth/speki.git", want: "github.com/xaroth/speki"},
		{name: "ssh url port", in: "ssh://git@ghe.example.com:2222/xaroth/speki.git", want: "ghe.example.com/xaroth/speki"},
		{name: "git+ssh", in: "git+ssh://git@github.com/xaroth/speki", want: "github.com/xaroth/speki"},
		{name: "git", in: "git://github.com/xaroth/speki.git", want: "github.com/xaroth/speki"},
		{name: "scp", in: "git@github.com:xaroth/speki.git", want: "github.com/xaroth/speki"},
		{name: "scp without user", in: "github.com:xaroth/speki", want: "github.com/xaroth/speki"},
		{name: "subgroup", in: "git@gitlab.com:team/sub/repo.git", want: "gitlab.com/team/sub/repo"},
		{name: "localhost", in: "localhost/team/repo", want: "localhost/team/repo"},
		{name: "dots and underscores", in: "xaroth/my_repo.v2", want: "github.com/xaroth/my_repo.v2"},

		{name: "empty", in: " ", wantErr: "not a repository"},
		{name: "name only", in: "speki", wantErr: "not a repository"},
		{name: "owner only", in: "github.com/xaroth", wantErr: "not a repository"},
		{name: "github path after repo", in: "https://github.com/xaroth/speki/tree/main", wantErr: "owner/repo, with nothing after it"},
		{name: "wildcard", in: "xaroth/mantine-*", wantErr: "can't contain *"},
		{name: "wildcard host", in: "*.example.com/team/repo", wantErr: "can't contain *"},
		{name: "absolute path", in: "/home/me/speki", wantErr: "local path"},
		{name: "relative path", in: "./speki", wantErr: "local path"},
		{name: "home path", in: "~/speki", wantErr: "local path"},
		{name: "file url", in: "file:///home/me/speki", wantErr: "local path"},
		{name: "unknown scheme", in: "ftp://github.com/xaroth/speki", wantErr: "not a repository"},
		{name: "dot dot", in: "github.com/xaroth/..", wantErr: "not a repository"},
		{name: "bad character", in: "xaroth/spe ki", wantErr: "not a repository"},
		{name: "ipv6", in: "ssh://git@[::1]/team/repo", wantErr: "not a repository"},
		{name: "too long", in: "example.com/" + strings.Repeat("a", 200) + "/repo", wantErr: "too long"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, err := vault.ParseRemote(testCase.in)
			if testCase.wantErr != "" {
				if err == nil {
					t.Fatalf("got %s, want an error", got)
				}
				if !strings.Contains(err.Error(), testCase.wantErr) {
					t.Errorf("got error %q, want it to mention %q", err, testCase.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRemote: %v", err)
			}
			if got.String() != testCase.want {
				t.Errorf("got %s, want %s", got, testCase.want)
			}
		})
	}
}

// A remote can carry a token; errors must not repeat it.
func TestParseRemoteErrorsDontQuoteInput(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"https://x-access-token:ghp_secret@github.com/xaroth/speki/tree/main",
		"https://x-access-token:ghp_secret@github.com/xaroth",
		"ftp://user:ghp_secret@github.com/xaroth/speki",
	} {
		_, err := vault.ParseRemote(in)
		if err == nil {
			t.Fatalf("ParseRemote accepted an invalid remote")
		}
		if strings.Contains(err.Error(), "ghp_secret") {
			t.Errorf("error %q quotes the token", err)
		}
	}
}

func TestRepositoryPatterns(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		in      string
		want    string
		pattern bool
		wantErr string
	}{
		{name: "exact", in: "xaroth/speki", want: "github.com/xaroth/speki"},
		{name: "prefix", in: "github.com/xaroth/mantine-*", want: "github.com/xaroth/mantine-*", pattern: true},
		{name: "suffix", in: "xaroth/*-api", want: "github.com/xaroth/*-api", pattern: true},
		{name: "whole owner", in: "xaroth/*", want: "github.com/xaroth/*", pattern: true},
		{name: "remote url", in: "git@github.com:Xaroth/Mantine-*.git", want: "github.com/xaroth/mantine-*", pattern: true},
		{name: "owner wildcard", in: "*/speki", wantErr: "only be used in the repository name"},
		{name: "subgroup wildcard", in: "gitlab.com/team/*/repo", wantErr: "only be used in the repository name"},
		{name: "host wildcard", in: "*.example.com/team/repo", wantErr: "only be used in the repository name"},
		{name: "other metacharacter", in: "xaroth/mantine-?", wantErr: "not a repository"},
		{name: "class", in: "xaroth/mantine-[ab]", wantErr: "not a repository"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var got vault.Repository
			err := got.UnmarshalText([]byte(testCase.in))
			if testCase.wantErr != "" {
				if err == nil {
					t.Fatalf("got %s, want an error", got)
				}
				if !strings.Contains(err.Error(), testCase.wantErr) {
					t.Errorf("got error %q, want it to mention %q", err, testCase.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("UnmarshalText: %v", err)
			}
			if got.String() != testCase.want || got.IsPattern() != testCase.pattern {
				t.Errorf("got %s (pattern %t), want %s (pattern %t)", got, got.IsPattern(), testCase.want, testCase.pattern)
			}
			text, err := got.MarshalText()
			if err != nil || string(text) != testCase.want {
				t.Errorf("MarshalText: got %q, %v, want %q", text, err, testCase.want)
			}
		})
	}
}

func TestRepositoryMatch(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		entry string
		repo  string
		want  bool
	}{
		{entry: "xaroth/speki", repo: "git@github.com:Xaroth/Speki.git", want: true},
		{entry: "xaroth/speki", repo: "xaroth/speki-infra", want: false},
		{entry: "xaroth/mantine-*", repo: "xaroth/mantine-core", want: true},
		{entry: "xaroth/mantine-*", repo: "xaroth/mantine-", want: true},
		{entry: "xaroth/mantine-*", repo: "xaroth/mantine", want: false},
		{entry: "xaroth/mantine-*", repo: "other/mantine-core", want: false},
		{entry: "xaroth/mantine-*", repo: "gitlab.com/xaroth/mantine-core", want: false},
		{entry: "xaroth/*-api", repo: "xaroth/orders-api", want: true},
		{entry: "xaroth/*", repo: "xaroth/anything", want: true},
		{entry: "gitlab.com/team/*", repo: "gitlab.com/team/repo", want: true},
		{entry: "gitlab.com/team/*", repo: "gitlab.com/team/sub/repo", want: false},
		{entry: "gitlab.com/team/sub/*", repo: "gitlab.com/team/sub/repo", want: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.entry+" "+testCase.repo, func(t *testing.T) {
			t.Parallel()
			var entry vault.Repository
			err := entry.UnmarshalText([]byte(testCase.entry))
			if err != nil {
				t.Fatalf("entry: %v", err)
			}
			repo, err := vault.ParseRemote(testCase.repo)
			if err != nil {
				t.Fatalf("repo: %v", err)
			}
			if got := entry.Match(repo); got != testCase.want {
				t.Errorf("%s.Match(%s) = %t, want %t", entry, repo, got, testCase.want)
			}
		})
	}
}
