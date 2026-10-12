package service

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/eve-online-tools/speki/gh"
	"github.com/eve-online-tools/speki/index"
	"github.com/eve-online-tools/speki/seal"
	"github.com/eve-online-tools/speki/telemetry"
)

// Config is everything speki needs to run. It knows nothing about files or environment variables;
// cmd/speki fills it in. Zero values mean "use the default".
type Config struct {
	// PublicURL is the address clients reach speki at. Tokens are bound to it, and every URL speki hands
	// out is built from it, never from request headers.
	PublicURL string `yaml:"public_url"`
	// Listen is the address of the public server: MCP, sign-in, health checks.
	Listen string `yaml:"listen"`
	// MetricsListen is the address of the internal server: metrics and the status page. It must differ
	// from Listen, so the internal endpoints are never reachable through the public port.
	MetricsListen string    `yaml:"metrics_listen"`
	GitHub        GitHub    `yaml:"github"`
	Access        Access    `yaml:"access"`
	Auth          Auth      `yaml:"auth"`
	Sync          Sync      `yaml:"sync"`
	Redis         Redis     `yaml:"redis"`
	Telemetry     Telemetry `yaml:"telemetry"`
	Instance      Instance  `yaml:"instance"`
	SQLite        SQLite    `yaml:"sqlite"`

	// Version is speki's version, shown to MCP clients. Not configuration: cmd/speki sets it.
	Version string `yaml:"-"`
}

// Access configures who gets in.
type Access struct {
	// RequiredOrgs: users must be an active member of at least one. Empty means no organisation check.
	RequiredOrgs []string `yaml:"required_orgs"`
}

// Auth configures sign-in.
type Auth struct {
	// Key is a single key speki seals its tokens with: base64 of at least 32 random bytes. It can't be
	// rotated; Keys can. Set one of Key and Keys.
	Key string `yaml:"key"`
	// Keys is the key file speki seals its tokens with.
	Keys string `yaml:"keys"`
	// ClientMetadataHosts are the hosts whose client metadata documents are accepted. Default claude.ai.
	ClientMetadataHosts []string `yaml:"client_metadata_hosts"`
}

func (a Auth) keyring() (*seal.Keyring, error) {
	if a.Key != "" {
		k, err := seal.SingleKey(a.Key)
		if err != nil {
			return nil, fmt.Errorf("auth.key: %w", err)
		}
		return k, nil
	}
	k, err := seal.ParseKeyring([]byte(a.Keys))
	if err != nil {
		return nil, fmt.Errorf("auth.keys: %w", err)
	}
	return k, nil
}

// GitHub identifies the GitHub App and where to reach GitHub.
type GitHub struct {
	AppID        int64  `yaml:"app_id"`
	PrivateKey   string `yaml:"private_key"` // PEM
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	// AllowedAccounts are the numeric IDs of the accounts whose installations count. Empty means the
	// account that owns the app.
	AllowedAccounts []int64 `yaml:"allowed_accounts"`
	APIURL          string  `yaml:"api_url"`   // default https://api.github.com/
	GitURL          string  `yaml:"git_url"`   // default https://github.com
	OAuthURL        string  `yaml:"oauth_url"` // default https://github.com
}

// Sync configures the local copies of vaults.
type Sync struct {
	DataDir           string        `yaml:"data_dir"`
	PollInterval      time.Duration `yaml:"poll_interval"`
	DiscoveryInterval time.Duration `yaml:"discovery_interval"`
}

// SQLite sets the memory limits of the SQLite in speki, for the whole process, in MiB.
type SQLite struct {
	// MemoryLimit is where SQLite's allocations fail. Searches and index builds fail past it too.
	MemoryLimit int64 `yaml:"memory_limit_mib"`
	// QueryMemory is where agents' SQL queries are stopped, below MemoryLimit so searches and index
	// builds keep the rest.
	QueryMemory int64 `yaml:"query_memory_mib"`
}

// Redis is the shared store that lets several instances run side by side. Without it, speki runs as a
// single instance and keeps that state in memory.
type Redis struct {
	// URL is redis:// or rediss://, with any password in it.
	URL string `yaml:"url"`
}

// Instance says what this speki is, for people and agents using more than one. All optional.
type Instance struct {
	// Name is a short identifier: the advertised implementation name becomes speki-<name>.
	Name string `yaml:"name"`
	// Title is shown to people and agents, on one line.
	Title string `yaml:"title"`
	// Description says what belongs here, so agents search and write in the right place.
	Description string `yaml:"description"`
}

// instanceName is what Instance.Name must look like.
var instanceName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// hiddenOrBreak reports whether r breaks a line, or is a control or format character that can hide or
// reorder text. Instance texts go to every user's agent.
func hiddenOrBreak(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp)
}

// maxInstanceDescription is the longest Instance.Description, in characters. It goes into the MCP
// instructions, which Claude Code cuts at 2,048 characters.
const maxInstanceDescription = 300

// Telemetry configures logging and tracing.
type Telemetry struct {
	// LogLevel is debug, info, warn or error.
	LogLevel string `yaml:"log_level"`
	// Tracing sends OpenTelemetry traces where the standard OTEL_EXPORTER_OTLP_* variables say.
	Tracing bool `yaml:"tracing"`
}

func (c Config) withDefaults() Config {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.MetricsListen == "" {
		c.MetricsListen = ":9090"
	}
	if c.GitHub.GitURL == "" {
		c.GitHub.GitURL = "https://github.com"
	}
	c.GitHub.GitURL = strings.TrimSuffix(c.GitHub.GitURL, "/")
	c.PublicURL = strings.TrimSuffix(c.PublicURL, "/")
	if c.GitHub.OAuthURL == "" {
		c.GitHub.OAuthURL = gh.DefaultOAuthURL
	}
	if len(c.Auth.ClientMetadataHosts) == 0 {
		c.Auth.ClientMetadataHosts = []string{"claude.ai"}
	}
	if c.Version == "" {
		c.Version = "dev"
	}
	if c.Sync.DataDir == "" {
		c.Sync.DataDir = "data"
	}
	if c.Sync.PollInterval == 0 {
		c.Sync.PollInterval = time.Minute
	}
	if c.Sync.DiscoveryInterval == 0 {
		c.Sync.DiscoveryInterval = 5 * time.Minute
	}
	if c.SQLite.MemoryLimit == 0 {
		c.SQLite.MemoryLimit = index.DefaultMemoryLimit
	}
	if c.SQLite.QueryMemory == 0 {
		c.SQLite.QueryMemory = index.DefaultQueryMemory
	}
	if c.Telemetry.LogLevel == "" {
		c.Telemetry.LogLevel = "info"
	}
	return c
}

// validate reports every problem with c at once, so an operator can fix them in one go.
func (c Config) validate() error {
	var errs []error
	if c.Listen == c.MetricsListen && !anyPort(c.Listen) {
		errs = append(errs, fmt.Errorf("listen and metrics_listen are both %q: the internal server needs its own address", c.Listen))
	}
	if c.GitHub.AppID <= 0 {
		errs = append(errs, errors.New("github.app_id is required"))
	}
	if c.GitHub.PrivateKey == "" {
		errs = append(errs, errors.New("github.private_key (or github.private_key_file) is required"))
	} else if err := gh.CheckPrivateKey([]byte(c.GitHub.PrivateKey)); err != nil {
		errs = append(errs, fmt.Errorf("github.private_key: %w", err))
	}
	for _, id := range c.GitHub.AllowedAccounts {
		if id <= 0 {
			errs = append(errs, fmt.Errorf("github.allowed_accounts: %d isn't an account ID", id))
		}
	}
	if c.GitHub.ClientID == "" || c.GitHub.ClientSecret == "" {
		errs = append(errs, errors.New("github.client_id and github.client_secret are required"))
	}
	errs = append(errs, checkPublicURL(c.PublicURL))
	switch {
	case c.Auth.Key == "" && c.Auth.Keys == "":
		errs = append(errs, errors.New("auth.key or auth.keys (or auth.key_file or auth.keys_file) is required"))
	case c.Auth.Key != "" && c.Auth.Keys != "":
		errs = append(errs, errors.New("auth.key and auth.keys are both set: use one"))
	default:
		_, err := c.Auth.keyring()
		errs = append(errs, err)
	}
	for _, h := range c.Auth.ClientMetadataHosts {
		if h == "" || strings.ContainsAny(h, ":/@ ") || h != strings.ToLower(h) || strings.HasSuffix(h, ".") {
			errs = append(errs, fmt.Errorf("auth.client_metadata_hosts: %q isn't a host name in lower case (write claude.ai, not https://claude.ai)", h))
		}
	}
	if c.Sync.PollInterval < 0 {
		errs = append(errs, errors.New("sync.poll_interval must be positive"))
	}
	if c.Sync.DiscoveryInterval < 0 {
		errs = append(errs, errors.New("sync.discovery_interval must be positive"))
	}
	if c.SQLite.MemoryLimit < 128 || c.SQLite.MemoryLimit > 1<<20 {
		errs = append(errs, errors.New("sqlite.memory_limit_mib must be between 128 and 1048576 (1 TiB)"))
	}
	if c.SQLite.QueryMemory <= 0 || c.SQLite.QueryMemory > c.SQLite.MemoryLimit*3/4 {
		errs = append(errs, errors.New("sqlite.query_memory_mib must be positive and at most 3/4 of sqlite.memory_limit_mib, so searches and index builds keep some"))
	}
	if c.Instance.Name != "" && !instanceName.MatchString(c.Instance.Name) {
		errs = append(errs, fmt.Errorf("instance.name: %q must be lower-case letters, digits and dashes, at most 32", c.Instance.Name))
	}
	if strings.ContainsFunc(c.Instance.Title, hiddenOrBreak) || utf8.RuneCountInString(c.Instance.Title) > 100 {
		errs = append(errs, errors.New("instance.title must be one line of at most 100 characters, without control or format characters"))
	}
	desc := strings.Join(strings.Fields(c.Instance.Description), " ") // line breaks fold to spaces, as YAML's > does
	if strings.ContainsFunc(desc, hiddenOrBreak) || utf8.RuneCountInString(desc) > maxInstanceDescription {
		errs = append(errs, fmt.Errorf("instance.description must be at most %d characters, without control or format characters", maxInstanceDescription))
	}
	_, err := telemetry.ParseLevel(c.Telemetry.LogLevel)
	if err != nil {
		errs = append(errs, fmt.Errorf("telemetry.log_level: %w", err))
	}
	return errors.Join(errs...)
}

// anyPort reports whether addr asks for a port picked by the system, which is different every time.
func anyPort(addr string) bool {
	_, port, err := net.SplitHostPort(addr)
	return err == nil && port == "0"
}

// checkPublicURL requires https, except for a local address during development.
func checkPublicURL(raw string) error {
	if raw == "" {
		return errors.New("public_url is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("public_url %q isn't a plain URL", raw)
	}
	if u.Path != "" {
		return fmt.Errorf("public_url %q must not have a path: speki's routes are at the root", raw)
	}
	local := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"
	devHTTP := u.Scheme == "http" && local
	if u.Scheme != "https" && !devHTTP {
		return fmt.Errorf("public_url %q must be https", raw)
	}
	return nil
}
