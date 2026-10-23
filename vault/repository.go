package vault

import (
	"errors"
	"path"
	"strings"
)

// defaultHost is the host of a repository written as owner/repo.
const defaultHost = "github.com"

// Limits on repository references, in bytes.
const (
	maxRemote     = 500 // the input, before normalising
	maxRepository = 200 // host/owner/repo
)

// remoteSchemes are the URL schemes git remotes use.
var remoteSchemes = map[string]bool{"https": true, "http": true, "ssh": true, "git": true, "git+ssh": true}

// Errors never quote the input: an HTTPS remote can carry a token.
var (
	errNotRepository = errors.New("not a repository: use host/owner/repo, owner/repo or a git remote URL")
	errLocalPath     = errors.New("a local path, not a repository: pass the git remotes (git remote -v)")
	errPattern       = errors.New("a repository can't contain *")
	errPatternPlace  = errors.New("* can only be used in the repository name, not the host or owner")
	errGitHubDepth   = errors.New("a github.com repository is owner/repo, with nothing after it")
	errTooLong       = errors.New("the repository is too long")
)

// Repository is a repository a vault covers, normalised to lower case. In a vault's speki.yaml, Name may
// hold * to cover every repository of Owner whose name matches.
type Repository struct {
	Host  string
	Owner string // can hold subgroups, as in GitLab: team/sub
	Name  string
}

func (r Repository) String() string { return r.Host + "/" + r.Owner + "/" + r.Name }

// IsPattern reports whether r covers repositories by a * in its name.
func (r Repository) IsPattern() bool { return strings.Contains(r.Name, "*") }

// Match reports whether r covers repo, a repository without *. Owners match exactly, so a pattern covers
// one level: gitlab.com/team/* doesn't cover gitlab.com/team/sub/repo.
func (r Repository) Match(repo Repository) bool {
	if r.Host != repo.Host || r.Owner != repo.Owner {
		return false
	}
	ok, _ := path.Match(r.Name, repo.Name) // names hold no metacharacter but *, so the pattern is valid
	return ok
}

// MarshalText writes r normalised.
func (r Repository) MarshalText() ([]byte, error) { return []byte(r.String()), nil }

// UnmarshalText reads a repository as written in speki.yaml, where * may be used in the name.
func (r *Repository) UnmarshalText(text []byte) error {
	repo, err := parseRepository(string(text), true)
	if err != nil {
		return err
	}
	*r = repo
	return nil
}

// ParseRemote reads the repository an agent works in: a git remote URL (https://, ssh://, git://),
// scp-style (git@host:owner/repo.git), host/owner/repo, or owner/repo on github.com.
func ParseRemote(s string) (Repository, error) {
	return parseRepository(s, false)
}

func parseRepository(s string, pattern bool) (Repository, error) {
	s = strings.TrimSpace(s)
	if len(s) > maxRemote {
		return Repository{}, errTooLong
	}
	if s == "" {
		return Repository{}, errNotRepository
	}
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "~") ||
		strings.HasPrefix(strings.ToLower(s), "file:") || isDrivePath(s) {
		return Repository{}, errLocalPath
	}

	var host, rest string
	scheme, afterScheme, isURL := strings.Cut(s, "://")
	colon, slash := strings.Index(s, ":"), strings.Index(s, "/")
	switch {
	case isURL:
		if !remoteSchemes[strings.ToLower(scheme)] {
			return Repository{}, errNotRepository
		}
		var authority string
		afterScheme, _, _ = strings.Cut(afterScheme, "#")
		afterScheme, _, _ = strings.Cut(afterScheme, "?")
		authority, rest, _ = strings.Cut(afterScheme, "/")
		host = hostOf(authority)
		if host == "" {
			return Repository{}, errNotRepository
		}
	case colon > 0 && (slash < 0 || colon < slash): // scp-style: [user@]host:owner/repo
		host = hostOf(s[:colon])
		rest = s[colon+1:]
		if host == "" {
			return Repository{}, errNotRepository
		}
	default:
		rest = s
	}

	rest = lowerASCII(strings.Trim(rest, "/"))
	rest = strings.TrimSuffix(rest, ".git")
	segments := strings.Split(rest, "/")
	if host == "" {
		host = defaultHost
		if first := segments[0]; strings.Contains(first, ".") || first == "localhost" {
			host, segments = first, segments[1:]
		}
	}
	host = lowerASCII(host)
	if host == "ssh.github.com" { // GitHub's SSH over port 443
		host = defaultHost
	}
	if strings.Contains(host, "*") {
		if !pattern {
			return Repository{}, errPattern
		}
		return Repository{}, errPatternPlace
	}
	if len(segments) < 2 || !validHost(host) {
		return Repository{}, errNotRepository
	}
	if host == defaultHost && len(segments) > 2 {
		return Repository{}, errGitHubDepth
	}
	for i, seg := range segments {
		last := i == len(segments)-1
		if strings.Contains(seg, "*") {
			if !pattern {
				return Repository{}, errPattern
			}
			if !last {
				return Repository{}, errPatternPlace
			}
		}
		if !validSegment(seg, pattern && last) {
			return Repository{}, errNotRepository
		}
	}
	r := Repository{
		Host:  host,
		Owner: strings.Join(segments[:len(segments)-1], "/"),
		Name:  segments[len(segments)-1],
	}
	if len(r.String()) > maxRepository {
		return Repository{}, errTooLong
	}
	return r, nil
}

// isDrivePath reports whether s starts like a Windows path, C:\ or C:/, which would otherwise read as
// scp-style.
func isDrivePath(s string) bool {
	return len(s) >= 3 && (s[0] >= 'a' && s[0] <= 'z' || s[0] >= 'A' && s[0] <= 'Z') && s[1] == ':' && (s[2] == '\\' || s[2] == '/')
}

// lowerASCII lower-cases ASCII letters only: Unicode folding would turn the Kelvin sign into k, passing
// validation as a name nobody wrote.
func lowerASCII(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
}

// hostOf returns the host of an authority, [user[:password]@]host[:port], or "" for an IPv6 literal.
func hostOf(authority string) string {
	if i := strings.LastIndex(authority, "@"); i >= 0 {
		authority = authority[i+1:]
	}
	if strings.HasPrefix(authority, "[") {
		return ""
	}
	host, _, _ := strings.Cut(authority, ":")
	return host
}

func validHost(host string) bool {
	if host == "" || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") || strings.Contains(host, "..") {
		return false
	}
	for _, c := range host {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '-':
		default:
			return false
		}
	}
	return true
}

// validSegment reports whether seg is an owner, subgroup or repository name. star allows *.
func validSegment(seg string, star bool) bool {
	if seg == "" || seg == "." || seg == ".." {
		return false
	}
	for _, c := range seg {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
		case c == '*' && star:
		default:
			return false
		}
	}
	return true
}
