// Package vault knows which repositories are vaults: it discovers them through the GitHub App's
// installations and reads their speki.yaml.
package vault

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/eve-online-tools/speki/gh"
)

// ConfigFile is the file that makes a repository a vault.
const ConfigFile = "speki.yaml"

// Limits on speki.yaml fields. They keep guide output readable when a user has many vaults.
const (
	maxTitle       = 100
	maxDescription = 300
	maxGuidelines  = 2000
)

// Config is a vault's speki.yaml.
type Config struct {
	Title       string  `yaml:"title"`
	Description string  `yaml:"description"`
	Guidelines  string  `yaml:"guidelines"`
	ReadRole    gh.Role `yaml:"read_role"` // the lowest repository role that sees the vault; unset means read
}

// ParseConfig reads speki.yaml. Unknown keys are ignored, so a newer file doesn't break an older speki.
func ParseConfig(raw []byte) (Config, error) {
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
	)
	err = errors.Join(errs...)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", ConfigFile, err)
	}
	return c, nil
}

func checkLength(field, s string, limit int) error {
	if n := utf8.RuneCountInString(s); n > limit {
		return fmt.Errorf("%s is %d characters, at most %d allowed", field, n, limit)
	}
	return nil
}
