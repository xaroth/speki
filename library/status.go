package library

import (
	"cmp"
	"slices"

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

// publish writes every vault's status to the gauges. Resetting first drops vaults that are gone.
// Restore, Run, Poll, Update and Close call it, so the gauges lag the real status by a poll round at
// most. Calls take turns, so an older snapshot can't overwrite a newer one.
func (l *Library) publish() {
	l.publishMu.Lock()
	defer l.publishMu.Unlock()
	vaultUp.Reset()
	vaultDocuments.Reset()
	vaultBytes.Reset()
	vaultFresh.Reset()
	for _, s := range l.Status() {
		up := 0.0
		if s.State == StateServing && s.Error == "" {
			up = 1
		}
		vaultUp.WithLabelValues(s.Name).Set(up)
		vaultDocuments.WithLabelValues(s.Name).Set(float64(s.Documents))
		vaultBytes.WithLabelValues(s.Name).Set(float64(s.Bytes))
		if !s.Fresh.IsZero() {
			vaultFresh.WithLabelValues(s.Name).Set(float64(s.Fresh.Unix()))
		}
	}
}

// Vaults returns every vault by state, each sorted by name. It reads each vault's state once, so a vault
// changing state meanwhile is in one of the lists, never in none. Failed vaults are retried by the poller,
// with backoff.
func (l *Library) Vaults() (serving, building, failed []vault.Vault) {
	for _, v := range l.all() {
		switch v.Status().State {
		case StateServing:
			serving = append(serving, v.Info())
		case StateBuilding:
			building = append(building, v.Info())
		case StateFailed:
			failed = append(failed, v.Info())
		}
	}
	for _, vs := range [][]vault.Vault{serving, building, failed} {
		slices.SortFunc(vs, func(a, b vault.Vault) int { return cmp.Compare(a.Name, b.Name) })
	}
	return serving, building, failed
}

// Changed returns a channel that's closed the next time a vault is added, changed or removed, or changes
// state.
func (l *Library) Changed() <-chan struct{} {
	l.changedMu.Lock()
	defer l.changedMu.Unlock()
	return l.changed
}

// notify wakes everyone waiting on Changed.
func (l *Library) notify() {
	l.changedMu.Lock()
	defer l.changedMu.Unlock()
	close(l.changed)
	l.changed = make(chan struct{})
}
