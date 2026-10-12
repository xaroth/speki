package oauth

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var signIns = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "speki_signins_total",
	Help: "Sign-ins through GitHub, by result: ok, declined, refused (not allowed in) or failed.",
}, []string{"result"})

var refreshes = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "speki_refreshes_total",
	Help: "Token refreshes, by result: ok, shared (another request's result for the same token), " +
		"github_failed (no usable answer from GitHub), signed_out (GitHub refused the token) or " +
		"lost_to_timeout (GitHub refused a token whose earlier refresh got no answer).",
}, []string{"result"})
