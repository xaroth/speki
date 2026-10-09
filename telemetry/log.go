// Package telemetry sets up logging, request IDs and metrics shared by the rest of speki.
package telemetry

import (
	"fmt"
	"io"
	"log/slog"
)

// redacted lists attribute keys whose values never belong in a log line. Code shouldn't log them in
// the first place; this is the safety net.
var redacted = map[string]bool{
	"token":         true,
	"authorization": true,
	"code":          true,
	"state":         true,
	"cookie":        true,
	"refresh_token": true,
	"content":       true,
	"sql":           true,
	"old_text":      true,
	"new_text":      true,
}

// NewLogger returns a JSON logger writing to w at the given level, with sensitive attributes redacted.
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: redact,
	})
	return slog.New(h)
}

func redact(_ []string, a slog.Attr) slog.Attr {
	if redacted[a.Key] {
		return slog.String(a.Key, "[redacted]")
	}
	return a
}

// ParseLevel turns a configured log level into a slog.Level. An empty string means info.
func ParseLevel(s string) (slog.Level, error) {
	if s == "" {
		return slog.LevelInfo, nil
	}
	var l slog.Level
	err := l.UnmarshalText([]byte(s))
	if err != nil {
		return 0, fmt.Errorf("log level %q: want debug, info, warn or error", s)
	}
	return l, nil
}
