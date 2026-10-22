// Package vault knows which repositories are vaults: it discovers them through the GitHub App's
// installations and reads their speki.yaml.
package vault

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/xaroth/speki/document"
	"github.com/xaroth/speki/gh"
)

// ConfigFile is the file that makes a repository a vault.
const ConfigFile = "speki.yaml"

// Limits on speki.yaml fields. They keep guide output readable when a user has many vaults.
const (
	maxTitle       = 100
	maxDescription = 300
	maxGuidelines  = 2000
	maxAllow       = 50
	maxAllowKey    = 100
	maxRepos       = 50
)

// Config is a vault's speki.yaml.
type Config struct {
	Title       string  `yaml:"title"`
	Description string  `yaml:"description"`
	Guidelines  string  `yaml:"guidelines"`
	ReadRole    gh.Role `yaml:"read_role"` // the lowest repository role that sees the vault; unset means read
	// Readers and Writers narrow who may read and write the vault, on top of GitHub's permissions. Nil
	// means no narrowing.
	Readers     []Principal `yaml:"readers"`
	Writers     []Principal `yaml:"writers"`
	Frontmatter Frontmatter `yaml:"frontmatter"`
	// Repositories are the code repositories the vault's knowledge is about.
	Repositories Repositories `yaml:"repositories"`
}

// Repositories lists the repositories a vault covers.
type Repositories []Repository

// UnmarshalYAML reads the list, naming the line of an entry it can't read instead of quoting it.
func (rs *Repositories) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.SequenceNode {
		return fmt.Errorf("repositories, line %d: not a list", node.Line)
	}
	out := make(Repositories, 0, len(node.Content))
	for _, n := range node.Content {
		var r Repository
		if n.Kind != yaml.ScalarNode {
			return fmt.Errorf("repositories, line %d: not a repository", n.Line)
		}
		err := r.UnmarshalText([]byte(n.Value))
		if err != nil {
			return fmt.Errorf("repositories, line %d: %w", n.Line, err)
		}
		out = append(out, r)
	}
	*rs = out
	return nil
}

// Covers reports whether any entry covers repo.
func (rs Repositories) Covers(repo Repository) bool {
	return slices.ContainsFunc(rs, func(r Repository) bool { return r.Match(repo) })
}

// Strings returns the entries normalised, as speki shows them.
func (rs Repositories) Strings() []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.String())
	}
	return out
}

// Frontmatter says which frontmatter keys agents may set, on top of speki's writer fields. Other keys
// keep the value they have.
type Frontmatter struct {
	Allow []string `yaml:"allow"`
}

// Principal is a GitHub user or team in readers or writers. Written as "login" or "org/team-slug", with
// an optional leading @ as in CODEOWNERS.
type Principal struct {
	Login string // a user, or
	Org   string // the organisation and
	Team  string // slug of a team
}

// GitHub's own rules for logins, organisation names and team slugs, loosened to accept any case.
var loginPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
var slugPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,255}$`)

func (p Principal) String() string {
	if p.Team != "" {
		return p.Org + "/" + p.Team
	}
	return p.Login
}

// MarshalText writes p as it's written in speki.yaml.
func (p Principal) MarshalText() ([]byte, error) { return []byte(p.String()), nil }

// UnmarshalText reads a user or team as written in speki.yaml.
func (p *Principal) UnmarshalText(text []byte) error {
	s := strings.TrimPrefix(strings.TrimSpace(string(text)), "@")
	org, team, isTeam := strings.Cut(s, "/")
	switch {
	case isTeam && loginPattern.MatchString(org) && slugPattern.MatchString(team):
		*p = Principal{Org: org, Team: strings.ToLower(team)} // slugs are lower case
	case !isTeam && loginPattern.MatchString(s):
		*p = Principal{Login: s}
	default:
		return fmt.Errorf("%q is neither a GitHub login nor org/team", text)
	}
	return nil
}

// ParseConfig reads the speki.yaml of a repository owned by owner. Unknown keys are ignored, so a newer
// file doesn't break an older speki.
func ParseConfig(raw []byte, owner string) (Config, error) {
	var c Config
	err := yaml.Unmarshal(raw, &c)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", ConfigFile, err)
	}
	c.Title = strings.TrimSpace(c.Title)
	c.Description = strings.TrimSpace(c.Description)
	c.Guidelines = strings.TrimSpace(c.Guidelines)

	var errs []error
	if c.Title == "" {
		errs = append(errs, errors.New("title is required"))
	}
	if c.Description == "" {
		errs = append(errs, errors.New("description is required"))
	}
	errs = append(errs,
		checkLength("title", c.Title, maxTitle),
		checkLength("description", c.Description, maxDescription),
		checkLength("guidelines", c.Guidelines, maxGuidelines),
		checkPrincipals("readers", c.Readers, owner),
		checkPrincipals("writers", c.Writers, owner),
		checkAllow(c.Frontmatter.Allow),
		checkRepositories(c.Repositories),
	)
	err = errors.Join(errs...)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", ConfigFile, err)
	}
	return c, nil
}

func checkAllow(keys []string) error {
	if len(keys) > maxAllow {
		return fmt.Errorf("frontmatter.allow has %d keys, at most %d allowed", len(keys), maxAllow)
	}
	var errs []error
	for _, k := range keys {
		switch {
		case strings.TrimSpace(k) == "":
			errs = append(errs, errors.New("frontmatter.allow holds an empty key"))
		case document.SetBySpeki(k):
			errs = append(errs, fmt.Errorf("frontmatter.allow: %s is set by speki", k))
		case utf8.RuneCountInString(k) > maxAllowKey:
			errs = append(errs, fmt.Errorf("frontmatter.allow: a key is longer than %d characters", maxAllowKey))
		}
	}
	return errors.Join(errs...)
}

func checkRepositories(rs Repositories) error {
	if len(rs) > maxRepos {
		return fmt.Errorf("repositories has %d entries, at most %d allowed", len(rs), maxRepos)
	}
	seen := make(map[Repository]bool, len(rs))
	for _, r := range rs {
		if seen[r] {
			return fmt.Errorf("repositories lists %s twice", r)
		}
		seen[r] = true
	}
	return nil
}

func checkLength(field, s string, limit int) error {
	if n := utf8.RuneCountInString(s); n > limit {
		return fmt.Errorf("%s is %d characters, at most %d allowed", field, n, limit)
	}
	return nil
}

// checkPrincipals rejects an empty list, which would otherwise read as "nobody" to some and "everybody" to
// others, and teams of other organisations: speki can only ask GitHub about the vault's own.
func checkPrincipals(field string, ps []Principal, owner string) error {
	if ps == nil {
		return nil
	}
	if len(ps) == 0 {
		return fmt.Errorf("%s is empty: list at least one user or team, or leave it out", field)
	}
	for _, p := range ps {
		if p.Team != "" && !strings.EqualFold(p.Org, owner) {
			return fmt.Errorf("%s: team %s isn't in %s, the organisation that owns the vault", field, p, owner)
		}
	}
	return nil
}
