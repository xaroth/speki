package service

import (
	"errors"
	"fmt"
	"net"

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
	Telemetry     Telemetry `yaml:"telemetry"`
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
