package mcpserver

import "github.com/prometheus/client_golang/prometheus"

// metrics counts and times tool calls.
type metrics struct {
	calls    *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func newMetrics() *metrics {
	return &metrics{
		calls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "speki_tool_calls_total",
			Help: "Tool calls, by tool and outcome: ok, or the error code.",
		}, []string{"tool", "outcome"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "speki_tool_duration_seconds",
			Help:    "How long tool calls take.",
			Buckets: prometheus.DefBuckets,
		}, []string{"tool"}),
	}
}

func (m *metrics) Describe(ch chan<- *prometheus.Desc) {
	m.calls.Describe(ch)
	m.duration.Describe(ch)
}

func (m *metrics) Collect(ch chan<- prometheus.Metric) {
	m.calls.Collect(ch)
	m.duration.Collect(ch)
}
