package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewLoggerRedacts(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelInfo)

	log.Info("signed in", "token", "ghu_secret", "login", "octocat", "outcome", "ok")

	var line map[string]any
	err := json.Unmarshal(buf.Bytes(), &line)
	if err != nil {
		t.Fatalf("decode log line: %v", err)
	}
	if line["token"] != "[redacted]" {
		t.Errorf("token = %v, want [redacted]", line["token"])
	}
	if line["login"] != "octocat" || line["outcome"] != "ok" {
		t.Errorf("other attributes changed: %v", line)
	}
	if strings.Contains(buf.String(), "ghu_secret") {
		t.Errorf("secret leaked into %q", buf.String())
	}
}

func TestNewLoggerLevel(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelWarn)

	log.Info("hidden")
	if buf.Len() != 0 {
		t.Errorf("info logged at warn level: %q", buf.String())
	}
}

func TestRequestID(t *testing.T) {
	tests := []struct {
		name     string
		incoming string
		keep     bool
	}{
		{"none", "", false},
		{"valid", "abc-123_X", true},
		{"too long", strings.Repeat("a", 65), false},
		{"bad characters", "abc def", false},
		{"header injection", "abc\r\nX-Evil: 1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen string
			h := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				seen = RequestIDFrom(r.Context())
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.incoming != "" {
				req.Header.Set(RequestIDHeader, tt.incoming)
			}
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if seen == "" {
				t.Fatal("no request ID on context")
			}
			if got := rec.Header().Get(RequestIDHeader); got != seen {
				t.Errorf("response header %q, context %q", got, seen)
			}
			if tt.keep && seen != tt.incoming {
				t.Errorf("ID = %q, want incoming %q", seen, tt.incoming)
			}
			if !tt.keep && seen == tt.incoming {
				t.Errorf("kept invalid incoming ID %q", tt.incoming)
			}
		})
	}
}

func TestLogLinesCarryRequestID(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelInfo).With("component", "test")
	var ctx context.Context
	RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		ctx = r.Context()
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	log.InfoContext(ctx, "in a request")
	log.Info("outside")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %q", lines)
	}
	var in, out map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &in)
	_ = json.Unmarshal([]byte(lines[1]), &out)
	if in["request_id"] != RequestIDFrom(ctx) || in["request_id"] == "" || in["component"] != "test" {
		t.Errorf("request line = %v", in)
	}
	if _, ok := out["request_id"]; ok {
		t.Errorf("line without a request has an ID: %v", out)
	}
}
