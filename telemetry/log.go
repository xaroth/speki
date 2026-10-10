// Package telemetry sets up logging and request IDs shared by the rest of speki.
package telemetry

import (
	"context"
	"fmt"
	"io"
	"log/slog"
)

// NewLogger returns a JSON logger writing to w at the given level. Sensitive attributes are redacted,
// and records logged with a request's context carry its request ID.
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: redact,
	})
	return slog.New(requestIDHandler{h})
}

// redact replaces the values of attributes that never belong in a log line. Code shouldn't log them in
// the first place; this is the safety net.
func redact(_ []string, a slog.Attr) slog.Attr {
	switch a.Key {
	case "token", "authorization", "code", "state", "cookie", "refresh_token", "content", "sql", "old_text", "new_text":
		return slog.String(a.Key, "[redacted]")
	}
	return a
}

// requestIDHandler adds the request ID from a record's context.
type requestIDHandler struct{ slog.Handler }

func (h requestIDHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestIDFrom(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

func (h requestIDHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return requestIDHandler{h.Handler.WithAttrs(attrs)}
}

func (h requestIDHandler) WithGroup(name string) slog.Handler {
	return requestIDHandler{h.Handler.WithGroup(name)}
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
