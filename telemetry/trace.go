package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
)

// StartTracing sends traces over OTLP/HTTP, configured by the standard OTEL_EXPORTER_OTLP_* variables,
// when enabled. Otherwise tracing stays a no-op. The returned function flushes what's left and stops.
func StartTracing(ctx context.Context, enabled bool, version string) (func(context.Context) error, error) {
	if !enabled {
		return func(context.Context) error { return nil }, nil
	}
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("tracing: %w", err)
	}
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(semconv.SchemaURL,
		semconv.ServiceName("speki"),
		semconv.ServiceVersion(version),
	))
	if err != nil && !errors.Is(err, resource.ErrSchemaURLConflict) {
		return nil, fmt.Errorf("tracing: %w", err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return tp.Shutdown, nil
}

// Tracer returns speki's tracer.
func Tracer() trace.Tracer { return otel.Tracer("github.com/eve-online-tools/speki") }

// Trace starts a server span for every request. It records the method, the path and the status, never
// the query string: the GitHub callback carries code and state there.
func Trace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := Tracer().Start(ctx, r.Method+" "+route(r.URL.Path),
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				semconv.HTTPRequestMethodKey.String(r.Method),
				semconv.URLPath(r.URL.Path),
				attribute.String("speki.request_id", RequestIDFrom(r.Context())),
			),
		)
		defer span.End()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r.WithContext(ctx))
		span.SetAttributes(semconv.HTTPResponseStatusCode(rec.status))
		if rec.status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(rec.status))
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(status int) {
	s.status = status
	s.ResponseWriter.WriteHeader(status)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Flush passes on to the underlying writer, for handlers that stream.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// route names a request's span from a fixed set, so scanners probing random paths don't create a span
// name each.
func route(path string) string {
	switch path {
	case "/mcp", "/healthz", "/readyz",
		"/oauth/authorize", "/oauth/token", "/oauth/register", "/oauth/github/callback":
		return path
	}
	if strings.HasPrefix(path, "/.well-known/oauth-") {
		return "/.well-known/oauth-*"
	}
	return "other"
}
