package service

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/eve-online-tools/speki/telemetry"
)

// Config is everything speki needs to run. It knows nothing about files or environment variables;
// cmd/speki fills it in. Zero values mean "use the default".
type Config struct {
	// Listen is the address of the public server: MCP, sign-in, health checks.
	Listen string `yaml:"listen"`
	// MetricsListen is the address of the internal server: metrics and the status page. It must differ
	// from Listen, so the internal endpoints are never reachable through the public port.
	MetricsListen string    `yaml:"metrics_listen"`
	GitHub        GitHub    `yaml:"github"`
	Sync          Sync      `yaml:"sync"`
	Telemetry     Telemetry `yaml:"telemetry"`
}

// GitHub identifies the GitHub App and where to reach GitHub.
type GitHub struct {
	AppID      int64  `yaml:"app_id"`
	PrivateKey Secret `yaml:"private_key"` // PEM
	// AllowedAccounts are the numeric IDs of the accounts whose installations count. Empty means the
	// account that owns the app.
	AllowedAccounts []int64 `yaml:"allowed_accounts"`
	APIURL          string  `yaml:"api_url"` // default https://api.github.com/
	GitURL          string  `yaml:"git_url"` // default https://github.com
}

// Sync configures the local copies of vaults.
type Sync struct {
	DataDir           string        `yaml:"data_dir"`
	PollInterval      time.Duration `yaml:"poll_interval"`
	DiscoveryInterval time.Duration `yaml:"discovery_interval"`
}

// Telemetry configures logging.
type Telemetry struct {
	// LogLevel is debug, info, warn or error.
	LogLevel string `yaml:"log_level"`
}

// withDefaults returns c with zero values replaced by defaults.
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
	if c.Sync.DataDir == "" {
		c.Sync.DataDir = "data"
	}
	if c.Sync.PollInterval == 0 {
		c.Sync.PollInterval = time.Minute
	}
	if c.Sync.DiscoveryInterval == 0 {
		c.Sync.DiscoveryInterval = 5 * time.Minute
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
	}
	if c.Sync.PollInterval < 0 || c.Sync.DiscoveryInterval < 0 {
		errs = append(errs, errors.New("sync intervals must be positive"))
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
