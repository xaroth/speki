// Internal: the metrics are unexported process-wide collectors, and the budget warnings need the
// metrics' clock. The metric tests don't run in parallel: other tests' requests move the same collectors.
package gh

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// An installation call counts once, as installation, and its token request as app; each credential's
// budget shows under its own kind. A refused token request counts as the app's, not as an installation
// call.
func TestMetricsByKind(t *testing.T) {
	testCases := []struct {
		name          string
		installation  int64
		want          map[[2]string]float64 // kind, status: requests
		wantRemaining map[string]float64
	}{
		{
			name:          "installation call",
			installation:  1,
			want:          map[[2]string]float64{{"app", "201"}: 1, {"app", "200"}: 0, {"installation", "200"}: 1},
			wantRemaining: map[string]float64{"app": 4999, "installation": 2999},
		},
		{
			name:          "token refused",
			installation:  8,
			want:          map[[2]string]float64{{"app", "403"}: 1, {"installation", "error"}: 0},
			wantRemaining: map[string]float64{"app": 4999},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := NewFake(t, ExpectInstallation(Installation{ID: 1}, Repository{ID: 100, FullName: "ours/kb"}),
				ExpectInstallation(Installation{ID: 8}), ExpectInstallation_Refused(8),
				ExpectRateLimit(1, 3000))
			before := map[[2]string]float64{}
			for k := range testCase.want {
				before[k] = testutil.ToFloat64(githubRequests.WithLabelValues(k[0], k[1]))
			}
			githubRemaining.Reset()
			_, err := fake.RepositoryID(t.Context(), testCase.installation, "ours/kb")
			if (err != nil) != (testCase.installation == 8) {
				t.Fatalf("RepositoryID: got %v", err)
			}
			for k, want := range testCase.want {
				if got := testutil.ToFloat64(githubRequests.WithLabelValues(k[0], k[1])) - before[k]; got != want {
					t.Errorf("%s %s requests: got %v, want %v", k[0], k[1], got, want)
				}
			}
			for kind, want := range testCase.wantRemaining {
				if got := testutil.ToFloat64(githubRemaining.WithLabelValues(kind)); got != want {
					t.Errorf("%s remaining: got %v, want %v", kind, got, want)
				}
			}
		})
	}
}

// The installation gauge shows the lowest recent budget, and a budget below 10% is warned about at most
// every warnEvery.
func TestRateBudget(t *testing.T) {
	var logged bytes.Buffer
	m := newMetrics(slog.New(slog.NewTextHandler(&logged, nil)))
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	githubRemaining.Reset()
	header := func(remaining string) http.Header {
		return http.Header{"X-Ratelimit-Remaining": {remaining}, "X-Ratelimit-Limit": {"5000"}}
	}
	gauge := func() float64 { return testutil.ToFloat64(githubRemaining.WithLabelValues("installation")) }
	warnings := func() int { return strings.Count(logged.String(), "budget is low") }

	m.record("installation", 1, header("100"))
	m.record("installation", 2, header("4000"))
	if got := gauge(); got != 100 {
		t.Errorf("installation remaining: got %v, want the lowest, 100", got)
	}
	m.record("installation", 1, header("50"))
	if got := warnings(); got != 1 || !strings.Contains(logged.String(), "installation=1") {
		t.Errorf("got %d warnings, want 1 for installation 1:\n%s", got, logged.String())
	}
	m.record("installation", 2, header("20"))
	if got := warnings(); got != 2 || !strings.Contains(logged.String(), "installation=2") {
		t.Errorf("got %d warnings, want 2, one per installation:\n%s", got, logged.String())
	}

	// Installation 1's reading ages out; installation 2's budget is refilled at its reset.
	m.record("installation", 2, http.Header{
		"X-Ratelimit-Remaining": {"20"},
		"X-Ratelimit-Limit":     {"5000"},
		"X-Ratelimit-Reset":     {strconv.FormatInt(now.Add(time.Minute).Unix(), 10)},
	})
	start := now
	now = now.Add(2 * time.Minute)
	m.record("installation", 3, header("3900"))
	if got := gauge(); got != 50 {
		t.Errorf("installation remaining after installation 2's refill: got %v, want installation 1's 50", got)
	}
	now = start.Add(budgetWindow + time.Second)
	m.record("installation", 3, header("3900"))
	if got := gauge(); got != 3900 {
		t.Errorf("installation remaining after installation 1's reading aged out: got %v, want 3900", got)
	}

	now = now.Add(warnEvery)
	m.record("installation", 1, header("40"))
	if got := warnings(); got != 3 {
		t.Errorf("got %d warnings, want 3: a low budget is warned about again after %v:\n%s", got, warnEvery, logged.String())
	}
}

// ghinstallation leaves a failed token response's body open; closeTokenError closes it, however wrapped.
func TestCloseTokenError(t *testing.T) {
	t.Parallel()
	body := &closeCounter{Reader: strings.NewReader("{}")}
	err := &ghinstallation.HTTPError{Response: &http.Response{StatusCode: http.StatusForbidden, Body: body}}
	closeTokenError(fmt.Errorf("wrapped: %w", err))
	closeTokenError(&ghinstallation.HTTPError{Response: &http.Response{}}) // no body: no panic
	if body.closed != 1 {
		t.Errorf("got the body closed %d times, want 1", body.closed)
	}
}

type closeCounter struct {
	io.Reader
	closed int
}

func (c *closeCounter) Close() error {
	c.closed++
	return nil
}
