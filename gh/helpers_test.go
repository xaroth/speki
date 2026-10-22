package gh_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/xaroth/speki/gh"
	"github.com/xaroth/speki/gh/ghtest"
)

const (
	writer  = "ghu_writer"
	reader  = "ghu_reader"
	revoked = "ghu_revoked"
)

var kb = gh.Repository{ID: 100, FullName: "ours/kb", DefaultBranch: "main", PushedAt: time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)}

// withKB is installation 1 with the repository ours/kb on branch main.
func withKB() ghtest.Option {
	return func(tb testing.TB, f *ghtest.Fake) {
		tb.Helper()
		ghtest.ExpectInstallation(gh.Installation{ID: 1, AccountID: 42}, kb)(tb, f)
		ghtest.ExpectBranch(kb.FullName, "main", "abc123")(tb, f)
	}
}

// withUsers adds octocat, who may write to ours/kb, and reader, who may only read it.
func withUsers() ghtest.Option {
	return func(tb testing.TB, f *ghtest.Fake) {
		tb.Helper()
		ghtest.ExpectUser(writer, gh.User{ID: 7, Login: "octocat"})(tb, f)
		ghtest.ExpectRole(writer, kb.FullName, gh.RoleWrite)(tb, f)
		ghtest.ExpectUser(reader, gh.User{ID: 8, Login: "reader"})(tb, f)
		ghtest.ExpectRole(reader, kb.FullName, gh.RoleRead)(tb, f)
	}
}

// respond answers a route with a fixed response.
func respond(route string, status int, header http.Header, body string) ghtest.Option {
	return ghtest.ExpectRoute(route, func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			for k, v := range header {
				w.Header()[k] = v
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		})
	})
}
