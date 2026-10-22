// The metric tests don't run in parallel: other tests' requests move the same process-wide collectors.
package gh_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/xaroth/speki/gh"
	"github.com/xaroth/speki/gh/ghtest"
)

// An installation call counts once, as installation, and its token request as app; each credential's
// budget shows under its own kind. A refused token request counts as the app's, not as an installation
// call.
func TestMetricsByKind(t *testing.T) {
	testCases := []struct {
		name          string
		installation  int64
		want          map[[2]string]float64 // kind, status: requests
		wantRemaining map[string]float64
	}{
		{
			name:          "installation call",
			installation:  1,
			want:          map[[2]string]float64{{"app", "201"}: 1, {"app", "200"}: 0, {"installation", "200"}: 1},
			wantRemaining: map[string]float64{"app": 4999, "installation": 2999},
		},
		{
			name:          "token refused",
			installation:  8,
			want:          map[[2]string]float64{{"app", "403"}: 1, {"installation", "error"}: 0},
			wantRemaining: map[string]float64{"app": 4999},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := ghtest.New(t, ghtest.ExpectInstallation(gh.Installation{ID: 1}, gh.Repository{ID: 100, FullName: "ours/kb"}),
				ghtest.ExpectInstallation(gh.Installation{ID: 8}), ghtest.ExpectInstallation_Refused(8),
				ghtest.ExpectRateLimit(1, 3000))
			before := map[[2]string]float64{}
			for k := range testCase.want {
				before[k] = testutil.ToFloat64(gh.GithubRequests.WithLabelValues(k[0], k[1]))
			}
			gh.GithubRemaining.Reset()
			_, err := fake.RepositoryID(t.Context(), testCase.installation, "ours/kb")
			if (err != nil) != (testCase.installation == 8) {
				t.Fatalf("RepositoryID: got %v", err)
			}
			for k, want := range testCase.want {
				if got := testutil.ToFloat64(gh.GithubRequests.WithLabelValues(k[0], k[1])) - before[k]; got != want {
					t.Errorf("%s %s requests: got %v, want %v", k[0], k[1], got, want)
				}
			}
			for kind, want := range testCase.wantRemaining {
				if got := testutil.ToFloat64(gh.GithubRemaining.WithLabelValues(kind)); got != want {
					t.Errorf("%s remaining: got %v, want %v", kind, got, want)
				}
			}
		})
	}
}
