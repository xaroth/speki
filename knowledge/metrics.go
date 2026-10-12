package knowledge

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var guardMismatches = promauto.NewCounter(prometheus.CounterOpts{
	Name: "speki_write_guard_mismatches_total",
	Help: "Changes refused because the index and git disagreed on a document after a sync: an index bug.",
})
