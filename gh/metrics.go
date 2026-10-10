package gh

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// How long an installation's rate limit reading counts towards the installation gauge, and how often a
// low budget is warned about per credential kind.
const (
	budgetWindow = 5 * time.Minute
	warnEvery    = 10 * time.Minute
)

type metrics struct {
	requests  *prometheus.CounterVec
	remaining *prometheus.GaugeVec
	logger    *slog.Logger

	mu       sync.Mutex
	installs map[int64]reading    // the latest reading per installation
	warned   map[string]time.Time // the last low-budget warning per kind
}

type reading struct {
	remaining int
	at        time.Time
}

func newMetrics(reg prometheus.Registerer, logger *slog.Logger) (*metrics, error) {
	m := &metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "speki_github_requests_total",
			Help: "GitHub API requests by credential kind and HTTP status.",
		}, []string{"kind", "status"}),
		remaining: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "speki_github_rate_remaining",
			Help: "Requests left in the current GitHub rate limit window, by credential kind; for installations, the lowest of the last few minutes.",
		}, []string{"kind"}),
		logger:   logger,
		installs: make(map[int64]reading),
		warned:   make(map[string]time.Time),
	}
	for _, c := range []prometheus.Collector{m.requests, m.remaining} {
		err := reg.Register(c)
		if err != nil {
			return nil, err
		}
	}
	return m, nil
}

// observe wraps next so every response is counted and its rate limit header recorded.
func (m *metrics) observe(kind string, next http.RoundTripper) http.RoundTripper {
	return m.observeIf(kind, 0, nil, next)
}

// observeApp wraps the app transport's base, which carries both the app's own calls, signed with a JWT,
// and the calls of installations created from it. Only the app's are counted here: installations count
// their own, one level up.
func (m *metrics) observeApp(next http.RoundTripper) http.RoundTripper {
	return m.observeIf("app", 0, signedByApp, next)
}

// observeInstallation wraps one installation's transport.
func (m *metrics) observeInstallation(id int64, next http.RoundTripper) http.RoundTripper {
	return m.observeIf("installation", id, nil, next)
}

// signedByApp reports whether r is authorised with the app's JWT rather than a token.
func signedByApp(r *http.Request) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && strings.Count(token, ".") == 2
}

func (m *metrics) observeIf(kind string, installation int64, counts func(*http.Request) bool, next http.RoundTripper) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := next.RoundTrip(r)
		if counts != nil && !counts(r) {
			return resp, err
		}
		if err != nil {
			m.requests.WithLabelValues(kind, "error").Inc()
			return nil, err
		}
		m.requests.WithLabelValues(kind, strconv.Itoa(resp.StatusCode)).Inc()
		m.record(kind, installation, resp.Header)
		return resp, nil
	})
}

// record sets the remaining budget from a response's rate limit headers, and warns when it's below 10%.
// Installations each have their own budget; the gauge shows the lowest recent one, so an installation
// close to its limit isn't hidden by another that's barely used.
func (m *metrics) record(kind string, installation int64, h http.Header) {
	remaining, err := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	if err != nil {
		return
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	lowest := remaining
	if kind == "installation" {
		m.installs[installation] = reading{remaining: remaining, at: now}
		for id, r := range m.installs {
			if now.Sub(r.at) > budgetWindow {
				delete(m.installs, id)
				continue
			}
			lowest = min(lowest, r.remaining)
		}
	}
	m.remaining.WithLabelValues(kind).Set(float64(lowest))

	limit, err := strconv.Atoi(h.Get("X-RateLimit-Limit"))
	if err != nil || limit <= 0 || remaining*10 >= limit || now.Sub(m.warned[kind]) < warnEvery {
		return
	}
	m.warned[kind] = now
	args := []any{"kind", kind, "remaining", remaining, "limit", limit, "reset", h.Get("X-RateLimit-Reset")}
	if installation != 0 {
		args = append(args, "installation", installation)
	}
	m.logger.Warn("GitHub rate limit budget is low", args...)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
