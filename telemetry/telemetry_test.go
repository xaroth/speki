package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

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

func TestRequestID(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name     string
		incoming string
		keep     bool
	}{
		{name: "none", incoming: ""},
		{name: "valid", incoming: "abc-123_X", keep: true},
		{name: "too long", incoming: strings.Repeat("a", 65)},
		{name: "bad characters", incoming: "abc\r\nX-Evil: 1"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var seen string
			h := telemetry.RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				seen = telemetry.RequestIDFrom(r.Context())
			}))
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			if testCase.incoming != "" {
				req.Header.Set(telemetry.RequestIDHeader, testCase.incoming)
			}
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if seen == "" {
				t.Fatal("got no request ID on the context, want one")
			}
			if got := rec.Header().Get(telemetry.RequestIDHeader); got != seen {
				t.Errorf("got response header %q, want the context's %q", got, seen)
			}
			if testCase.keep && seen != testCase.incoming {
				t.Errorf("got ID %q, want incoming %q", seen, testCase.incoming)
			}
			if !testCase.keep && seen == testCase.incoming {
				t.Errorf("got invalid incoming ID %q kept, want a new one", seen)
			}
		})
	}
}

// Not parallel: Trace uses the global tracer provider and propagator, which this test replaces.
func TestTrace(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	oldTP, oldProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(oldTP)
		otel.SetTextMapPropagator(oldProp)
	})
	const callerTrace = "4bf92f3577b34da6a3ce929d0e0e4736"

	testCases := []struct {
		name        string
		target      string
		status      int
		traceparent string
		wantName    string
		wantError   bool
		wantLink    bool
	}{
		{name: "query string left out", target: "/oauth/github/callback?code=secret&state=secret", status: http.StatusOK, wantName: "GET /oauth/github/callback"},
		{name: "server error", target: "/mcp", status: http.StatusBadGateway, wantName: "GET /mcp", wantError: true},
		{name: "well-known path", target: "/.well-known/oauth-authorization-server", status: http.StatusOK, wantName: "GET /.well-known/oauth-*"},
		{name: "unknown path", target: "/wp-admin/x.php", status: http.StatusNotFound, wantName: "GET other"},
		// Anyone can send a traceparent to the public edge, so it starts a new trace and keeps the
		// incoming one only as a link.
		{
			name:        "caller's trace",
			target:      "/mcp",
			status:      http.StatusOK,
			traceparent: "00-" + callerTrace + "-00f067aa0ba902b7-01",
			wantName:    "GET /mcp",
			wantLink:    true,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			before := len(rec.Ended())
			h := telemetry.Trace(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(testCase.status)
			}))
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, testCase.target, nil)
			if testCase.traceparent != "" {
				req.Header.Set("traceparent", testCase.traceparent)
			}

			h.ServeHTTP(httptest.NewRecorder(), req)

			spans := rec.Ended()[before:]
			if len(spans) != 1 {
				t.Fatalf("got %d spans, want 1", len(spans))
			}
			s := spans[0]
			if s.Name() != testCase.wantName {
				t.Errorf("got span name %q, want %q", s.Name(), testCase.wantName)
			}
			for _, a := range s.Attributes() {
				if strings.Contains(a.Value.String(), "secret") {
					t.Errorf("got attribute %s = %q, want no query string", a.Key, a.Value.String())
				}
			}
			if got := s.Status().Code == codes.Error; got != testCase.wantError {
				t.Errorf("got error status %v, want %v", got, testCase.wantError)
			}
			if s.Parent().IsValid() || s.SpanContext().TraceID().String() == callerTrace {
				t.Errorf("got the span in the caller's trace (parent %v), want a new root", s.Parent())
			}
			gotLink := len(s.Links()) == 1 && s.Links()[0].SpanContext.TraceID().String() == callerTrace
			if gotLink != testCase.wantLink || (!testCase.wantLink && len(s.Links()) != 0) {
				t.Errorf("got links %v, want link to the caller's trace %v", s.Links(), testCase.wantLink)
			}
		})
	}
}

func TestTraceKeepsFlushing(t *testing.T) {
	t.Parallel()
	h := telemetry.Trace(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("got a writer without http.Flusher behind Trace, want one")
			return
		}
		f.Flush()
	}))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/mcp", nil))

	if !rec.Flushed {
		t.Error("got no flush on the writer, want one")
	}
}
