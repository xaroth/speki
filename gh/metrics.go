package gh

import (
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

type metrics struct {
	requests  *prometheus.CounterVec
	remaining *prometheus.GaugeVec
}

func newMetrics(reg prometheus.Registerer) (*metrics, error) {
	m := &metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "speki_github_requests_total",
			Help: "GitHub API requests by credential kind and HTTP status.",
		}, []string{"kind", "status"}),
		remaining: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "speki_github_rate_remaining",
			Help: "Requests left in the current GitHub rate limit window, by credential kind.",
		}, []string{"kind"}),
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
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := next.RoundTrip(r)
		if err != nil {
			m.requests.WithLabelValues(kind, "error").Inc()
			return nil, err
		}
		m.requests.WithLabelValues(kind, strconv.Itoa(resp.StatusCode)).Inc()
		if n, err := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining")); err == nil {
			m.remaining.WithLabelValues(kind).Set(float64(n))
		}
		return resp, nil
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
