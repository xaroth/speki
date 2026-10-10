package oauth

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var signIns = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "speki_signins_total",
	Help: "Sign-ins through GitHub, by result: ok, declined, refused (not allowed in) or failed.",
}, []string{"result"})
