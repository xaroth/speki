package library

import (
	"cmp"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eve-online-tools/speki/vault"
)

// Status returns every vault's status, sorted by name.
func (l *Library) Status() []Status {
	var out []Status
	for _, v := range l.all() {
		out = append(out, v.Status())
	}
	slices.SortFunc(out, func(a, b Status) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

// Collector returns a Prometheus collector for the per-vault metrics. It reads the vaults' status on
// every scrape, so removed vaults disappear from the metrics.
func (l *Library) Collector() prometheus.Collector {
	return &collector{
		l: l,
		up: prometheus.NewDesc("speki_vault_up",
			"1 when the vault is served and its last sync succeeded.", []string{"vault"}, nil),
		documents: prometheus.NewDesc("speki_vault_documents",
			"Documents in the vault's index.", []string{"vault"}, nil),
		bytes: prometheus.NewDesc("speki_vault_bytes",
			"Markdown bytes in the vault's index.", []string{"vault"}, nil),
		fresh: prometheus.NewDesc("speki_vault_fresh_seconds",
			"Seconds since speki last confirmed the vault's index matches GitHub.", []string{"vault"}, nil),
	}
}

type collector struct {
	l                           *Library
	up, documents, bytes, fresh *prometheus.Desc
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.up
	ch <- c.documents
	ch <- c.bytes
	ch <- c.fresh
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	now := time.Now()
	for _, s := range c.l.Status() {
		up := 0.0
		if s.State == StateServing && s.Error == "" {
			up = 1
		}
		ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, up, s.Name)
		ch <- prometheus.MustNewConstMetric(c.documents, prometheus.GaugeValue, float64(s.Documents), s.Name)
		ch <- prometheus.MustNewConstMetric(c.bytes, prometheus.GaugeValue, float64(s.Bytes), s.Name)
		if !s.Fresh.IsZero() {
			ch <- prometheus.MustNewConstMetric(c.fresh, prometheus.GaugeValue, now.Sub(s.Fresh).Seconds(), s.Name)
		}
	}
}

// Vaults returns the vaults being served, sorted by name.
func (l *Library) Vaults() []vault.Vault {
	var out []vault.Vault
	for _, v := range l.all() {
		if v.Status().State == StateServing {
			out = append(out, v.Info())
		}
	}
	slices.SortFunc(out, func(a, b vault.Vault) int { return cmp.Compare(a.Name, b.Name) })
	return out
}
