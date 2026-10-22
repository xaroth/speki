package telemetry_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/xaroth/speki/telemetry"
)

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

// Behind Trace, http.ResponseController still reaches the connection: a stream needs its deadlines.
func TestTraceKeepsResponseController(t *testing.T) {
	t.Parallel()
	errs := make(chan error, 1)
	srv := httptest.NewServer(telemetry.Trace(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		errs <- http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Minute))
	})))
	t.Cleanup(srv.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}

	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()

	err = <-errs
	if err != nil {
		t.Errorf("got %v setting the write deadline, want nil", err)
	}
}

// Not parallel: StartTracing sets the global tracer provider and propagator, and reads the environment.
func TestStartTracing(t *testing.T) {
	oldTP, oldProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(oldTP)
		otel.SetTextMapPropagator(oldProp)
	})

	t.Run("disabled", func(t *testing.T) {
		stop, err := telemetry.StartTracing(t.Context(), false, "1.2.3")
		if err != nil {
			t.Fatal(err)
		}
		if got := otel.GetTracerProvider(); got != oldTP {
			t.Errorf("got tracer provider %T, want the global one left alone", got)
		}
		err = stop(t.Context())
		if err != nil {
			t.Errorf("got %v stopping, want nil", err)
		}
	})

	t.Run("exports to the configured endpoint", func(t *testing.T) {
		type export struct {
			path string
			body []byte
		}
		exports := make(chan export, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			exports <- export{path: r.URL.Path, body: body}
		}))
		t.Cleanup(srv.Close)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)

		stop, err := telemetry.StartTracing(t.Context(), true, "1.2.3")
		if err != nil {
			t.Fatal(err)
		}
		_, span := telemetry.Tracer().Start(t.Context(), "test span")
		span.End()
		err = stop(t.Context()) // flushes the span
		if err != nil {
			t.Fatalf("got %v stopping, want nil", err)
		}

		got := <-exports
		if got.path != "/v1/traces" {
			t.Errorf("got export to %q, want /v1/traces", got.path)
		}
		for _, want := range []string{"test span", "speki", "1.2.3"} {
			if !bytes.Contains(got.body, []byte(want)) {
				t.Errorf("got export without %q, want the span with speki's name and version", want)
			}
		}
		if got := otel.GetTextMapPropagator().Fields(); !slices.Contains(got, "traceparent") {
			t.Errorf("got propagator fields %v, want traceparent", got)
		}
	})
}
