package mcpserver

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var toolCalls = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "speki_tool_calls_total",
	Help: "Tool calls, by tool and outcome: ok, or the error code.",
}, []string{"tool", "outcome"})

var toolDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "speki_tool_duration_seconds",
	Help:    "How long tool calls take.",
	Buckets: prometheus.DefBuckets,
}, []string{"tool"})
