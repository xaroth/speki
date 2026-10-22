package telemetry_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xaroth/speki/telemetry"
)

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
