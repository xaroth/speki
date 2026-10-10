package gh

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/prometheus/client_golang/prometheus"
)

// How long an installation's rate limit reading counts towards the installation gauge at most, and how
// often a low budget is warned about per credential.
const (
	budgetWindow = 5 * time.Minute
	warnEvery    = 10 * time.Minute
)

type metrics struct {
	requests  *prometheus.CounterVec
	remaining *prometheus.GaugeVec
	logger    *slog.Logger

	mu       sync.Mutex
	installs map[int64]reading     // the latest reading per installation
	warned   map[warnKey]time.Time // the last low-budget warning per kind and installation
}

type reading struct {
	remaining int
	at        time.Time
	reset     time.Time // when GitHub refills the budget; zero if it didn't say
}

type warnKey struct {
	kind         string
	installation int64
}

func newMetrics(reg prometheus.Registerer, logger *slog.Logger) (*metrics, error) {
	m := &metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "speki_github_requests_total",
			Help: "GitHub API requests by credential kind and HTTP status.",
		}, []string{"kind", "status"}),
		remaining: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "speki_github_rate_remaining",
			Help: "Requests left in the current GitHub rate limit window, by credential kind; for installations, the lowest across installations heard from in the last few minutes.",
		}, []string{"kind"}),
		logger:   logger,
		installs: make(map[int64]reading),
		warned:   make(map[warnKey]time.Time),
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
	return m.observed(kind, 0, func(*http.Request) bool { return true }, next)
}

// observeApp wraps the app transport's base, which carries both the app's own calls, signed with a JWT,
// and the calls of installations created from it. Only the app's are counted here: installations count
// their own, one level up.
func (m *metrics) observeApp(next http.RoundTripper) http.RoundTripper {
	return m.observed("app", 0, signedByApp, next)
}

// observeInstallation wraps one installation's transport. When its token can't be had, no installation
// call was made (the app's transport counted the token request), and ghinstallation leaves that
// response's body open: it's closed here, so a failing installation doesn't leak a connection per try.
func (m *metrics) observeInstallation(id int64, next http.RoundTripper) http.RoundTripper {
	counted := m.observed("installation", id, func(*http.Request) bool { return true }, next)
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := counted.RoundTrip(r)
		closeTokenError(err)
		return resp, err
	})
}

// closeTokenError drains and closes the response body of a failed installation token request, which
// ghinstallation returns unread and open. Closing twice is harmless.
func closeTokenError(err error) {
	var terr *ghinstallation.HTTPError
	if errors.As(err, &terr) && terr.Response != nil && terr.Response.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(terr.Response.Body, 4<<10))
		_ = terr.Response.Body.Close()
	}
}

// signedByApp reports whether r is authorised with the app's JWT rather than a token.
func signedByApp(r *http.Request) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && strings.Count(token, ".") == 2
}

// observed counts the requests next makes that counts accepts, decided before the request goes out.
func (m *metrics) observed(kind string, installation int64, counts func(*http.Request) bool, next http.RoundTripper) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		count := counts(r)
		resp, err := next.RoundTrip(r)
		var terr *ghinstallation.HTTPError
		if !count || errors.As(err, &terr) { // a failed token request isn't this kind's call
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
// Installations each have their own budget; the gauge shows the lowest of them, so an installation
// close to its limit isn't hidden by another that's barely used. A reading counts until its budget is
// refilled, or for budgetWindow at most.
func (m *metrics) record(kind string, installation int64, h http.Header) {
	remaining, err := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	if err != nil {
		return
	}
	now := time.Now()
	var reset time.Time
	if s, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		reset = time.Unix(s, 0)
	}

	m.mu.Lock()
	lowest := remaining
	if kind == "installation" {
		m.installs[installation] = reading{remaining: remaining, at: now, reset: reset}
		for id, r := range m.installs {
			if now.Sub(r.at) > budgetWindow || !r.reset.IsZero() && now.After(r.reset) {
				delete(m.installs, id)
				continue
			}
			lowest = min(lowest, r.remaining)
		}
	}
	m.remaining.WithLabelValues(kind).Set(float64(lowest))
	limit, err := strconv.Atoi(h.Get("X-RateLimit-Limit"))
	key := warnKey{kind, installation}
	warn := err == nil && limit > 0 && remaining*10 < limit && now.Sub(m.warned[key]) >= warnEvery
	if warn {
		m.warned[key] = now
	}
	m.mu.Unlock()

	if warn {
		args := []any{"kind", kind, "remaining", remaining, "limit", limit, "reset", reset}
		if installation != 0 {
			args = append(args, "installation", installation)
		}
		m.logger.Warn("GitHub rate limit budget is low", args...)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
