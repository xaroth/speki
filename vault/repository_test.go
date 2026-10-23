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
		{name: "owner/repo", in: "acme/speki", want: "github.com/acme/speki"},
		{name: "host/owner/repo", in: "github.com/acme/speki", want: "github.com/acme/speki"},
		{name: "case", in: "GitHub.com/Acme/Speki", want: "github.com/acme/speki"},
		{name: "spaces", in: "  acme/speki\n", want: "github.com/acme/speki"},
		{name: "https", in: "https://github.com/acme/speki", want: "github.com/acme/speki"},
		{name: "https .git", in: "https://github.com/acme/speki.git", want: "github.com/acme/speki"},
		{name: "https trailing slash", in: "https://github.com/xaroth/speki/", want: "github.com/xaroth/speki"},
		{name: "https query and fragment", in: "https://github.com/acme/speki?tab=readme#top", want: "github.com/acme/speki"},
		{name: "https credentials", in: "https://x-access-token:ghp_secret@github.com/acme/speki.git", want: "github.com/acme/speki"},
		{name: "http port", in: "http://git.example.com:8080/team/repo", want: "git.example.com/team/repo"},
		{name: "ssh url", in: "ssh://git@github.com/acme/speki.git", want: "github.com/acme/speki"},
		{name: "ssh url port", in: "ssh://git@ghe.example.com:2222/acme/speki.git", want: "ghe.example.com/acme/speki"},
		{name: "git+ssh", in: "git+ssh://git@github.com/acme/speki", want: "github.com/acme/speki"},
		{name: "git", in: "git://github.com/acme/speki.git", want: "github.com/acme/speki"},
		{name: "scp", in: "git@github.com:acme/speki.git", want: "github.com/acme/speki"},
		{name: "scp without user", in: "github.com:acme/speki", want: "github.com/acme/speki"},
		{name: "subgroup", in: "git@gitlab.com:team/sub/repo.git", want: "gitlab.com/team/sub/repo"},
		{name: "localhost", in: "localhost/team/repo", want: "localhost/team/repo"},
		{name: "ssh over 443", in: "ssh://git@ssh.github.com:443/acme/speki.git", want: "github.com/acme/speki"},
		{name: "dots and underscores", in: "acme/my_repo.v2", want: "github.com/acme/my_repo.v2"},

		{name: "empty", in: " ", wantErr: "not a repository"},
		{name: "name only", in: "speki", wantErr: "not a repository"},
		{name: "owner only", in: "github.com/acme", wantErr: "not a repository"},
		{name: "github path after repo", in: "https://github.com/xaroth/speki/tree/main", wantErr: "owner/repo, with nothing after it"},
		{name: "wildcard", in: "acme/web-*", wantErr: "can't contain *"},
		{name: "wildcard host", in: "*.example.com/team/repo", wantErr: "can't contain *"},
		{name: "absolute path", in: "/home/me/speki", wantErr: "local path"},
		{name: "relative path", in: "./speki", wantErr: "local path"},
		{name: "home path", in: "~/speki", wantErr: "local path"},
		{name: "file url", in: "file:///home/me/speki", wantErr: "local path"},
		{name: "windows path", in: `C:\src\speki`, wantErr: "local path"},
		{name: "windows path, forward slashes", in: "C:/src/acme/speki", wantErr: "local path"},
		{name: "kelvin sign", in: "github.com/\u212aaroth/speki", wantErr: "not a repository"},
		{name: "host trailing dot", in: "example.com./team/repo", wantErr: "not a repository"},
		{name: "host dot dot", in: "https://example..com/team/repo", wantErr: "not a repository"},
		{name: "unknown scheme", in: "ftp://github.com/acme/speki", wantErr: "not a repository"},
		{name: "dot dot", in: "github.com/acme/..", wantErr: "not a repository"},
		{name: "bad character", in: "acme/spe ki", wantErr: "not a repository"},
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
		"https://x-access-token:ghp_secret@github.com/acme",
		"ftp://user:ghp_secret@github.com/acme/speki",
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
		{name: "exact", in: "acme/speki", want: "github.com/acme/speki"},
		{name: "prefix", in: "github.com/acme/web-*", want: "github.com/acme/web-*", pattern: true},
		{name: "suffix", in: "acme/*-api", want: "github.com/acme/*-api", pattern: true},
		{name: "whole owner", in: "acme/*", want: "github.com/acme/*", pattern: true},
		// Remotes lose .git the same way, so *.git could match nothing as written.
		{name: "remote url", in: "git@github.com:Acme/Web-*.git", want: "github.com/acme/web-*", pattern: true},
		{name: "owner wildcard", in: "*/speki", wantErr: "only be used in the repository name"},
		{name: "subgroup wildcard", in: "gitlab.com/team/*/repo", wantErr: "only be used in the repository name"},
		{name: "host wildcard", in: "*.example.com/team/repo", wantErr: "only be used in the repository name"},
		{name: "other metacharacter", in: "acme/web-?", wantErr: "not a repository"},
		{name: "class", in: "acme/web-[ab]", wantErr: "not a repository"},
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
		{entry: "acme/speki", repo: "git@github.com:Acme/Speki.git", want: true},
		{entry: "acme/speki", repo: "acme/speki-infra", want: false},
		{entry: "acme/web-*", repo: "acme/web-core", want: true},
		{entry: "acme/web-*", repo: "acme/web-", want: true},
		{entry: "acme/web-*", repo: "acme/web", want: false},
		{entry: "acme/web-*", repo: "other/web-core", want: false},
		{entry: "acme/web-*", repo: "gitlab.com/acme/web-core", want: false},
		{entry: "acme/*-api", repo: "acme/orders-api", want: true},
		{entry: "acme/*", repo: "acme/anything", want: true},
		{entry: "gitlab.com/acme/*", repo: "github.com/acme/speki", want: false},
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
