package service

import "log/slog"

// Secret is a configuration value that must never be printed or logged.
type Secret string

// String hides the value from fmt.
func (Secret) String() string { return "[redacted]" }

// GoString hides the value from %#v.
func (Secret) GoString() string { return "[redacted]" }

// LogValue hides the value from slog.
func (Secret) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// MarshalJSON hides the value from encoding/json, which slog's JSON handler uses for nested structs.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// MarshalText hides the value from text encoders, YAML among them.
func (Secret) MarshalText() ([]byte, error) { return []byte("[redacted]"), nil }

// Reveal returns the actual value, for the code that uses it.
func (s Secret) Reveal() string { return string(s) }
