package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/xaroth/speki/telemetry"
)

// requestContext returns the context a handler behind telemetry.RequestID sees for a request with id.
func requestContext(tb testing.TB, id string) context.Context {
	tb.Helper()
	var ctx context.Context
	req := httptest.NewRequestWithContext(tb.Context(), http.MethodGet, "/", nil)
	req.Header.Set(telemetry.RequestIDHeader, id)
	telemetry.RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		ctx = r.Context()
	})).ServeHTTP(httptest.NewRecorder(), req)
	return ctx
}

func TestNewLogger(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name  string
		level slog.Level
		log   func(tb testing.TB, logger *slog.Logger)
		want  []map[string]any
	}{
		{
			name:  "redacts secrets",
			level: slog.LevelInfo,
			log: func(_ testing.TB, logger *slog.Logger) {
				logger.Info("signed in", "token", "ghu_secret", "login", "octocat")
			},
			want: []map[string]any{{"level": "INFO", "msg": "signed in", "token": "[redacted]", "login": "octocat"}},
		},
		{
			name:  "request ID from the context",
			level: slog.LevelInfo,
			log: func(tb testing.TB, logger *slog.Logger) {
				logger = logger.WithGroup("g").With("component", "test")
				logger.InfoContext(requestContext(tb, "req-1"), "in a request")
				logger.Info("outside")
			},
			want: []map[string]any{
				{"level": "INFO", "msg": "in a request", "g": map[string]any{"component": "test", "request_id": "req-1"}},
				{"level": "INFO", "msg": "outside", "g": map[string]any{"component": "test"}},
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			testCase.log(t, telemetry.NewLogger(&buf, testCase.level))

			var got []map[string]any
			dec := json.NewDecoder(&buf)
			for dec.More() {
				var line map[string]any
				err := dec.Decode(&line)
				if err != nil {
					t.Fatalf("decode log line: %v", err)
				}
				delete(line, "time")
				got = append(got, line)
			}
			if diff := cmp.Diff(testCase.want, got); diff != "" {
				t.Errorf("log lines (-want +got):\n%s", diff)
			}
		})
	}
}

func TestParseLevel(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		in      string
		want    slog.Level
		wantErr bool
	}{
		{name: "empty", in: "", want: slog.LevelInfo},
		{name: "named", in: "warn", want: slog.LevelWarn},
		{name: "unknown", in: "loud", wantErr: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, err := telemetry.ParseLevel(testCase.in)
			if (err != nil) != testCase.wantErr {
				t.Fatalf("got error %v, want error %v", err, testCase.wantErr)
			}
			if got != testCase.want {
				t.Errorf("got %v, want %v", got, testCase.want)
			}
		})
	}
}
