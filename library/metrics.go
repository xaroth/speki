package library

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var syncs = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "speki_sync_total",
	Help: "Vault syncs, by result: unchanged, applied, rebuilt, deferred (a rebuild left to the poller) or failed.",
}, []string{"result"})

var vaultUp = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Name: "speki_vault_up",
	Help: "1 when the vault is served and its last sync succeeded.",
}, []string{"vault"})

var vaultDocuments = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Name: "speki_vault_documents",
	Help: "Documents in the vault's index.",
}, []string{"vault"})

var vaultBytes = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Name: "speki_vault_bytes",
	Help: "Markdown bytes in the vault's index.",
}, []string{"vault"})

// vaultFresh is a timestamp, not an age: if the poller stalls, nothing publishes, and an age would freeze
// at a healthy value.
var vaultFresh = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Name: "speki_vault_fresh_timestamp_seconds",
	Help: "Unix time when speki last confirmed the vault's index matches GitHub.",
}, []string{"vault"})
