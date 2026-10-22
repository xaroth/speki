package oauth_test

import (
	"net/http"
	"regexp"
	"testing"

	"github.com/google/go-cmp/cmp"
)

var fontURL = regexp.MustCompile(`url\((/oauth/fonts/[^)]+)\)`)

// Every font the pages name is served; nothing else is.
func TestFonts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	page := e.do(t, authorizeURL(claudeClient, claudeCallback, nil), nil)
	urls := fontURL.FindAllStringSubmatch(page.Body.String(), -1)
	if len(urls) == 0 {
		t.Fatal("got no fonts on the page, want some")
	}
	type served struct {
		Status       int
		ContentType  string
		CacheControl string
		NoSniff      string
		Empty        bool
	}
	testCases := []struct {
		name   string
		target string
		want   served
	}{
		{name: "missing", target: "/oauth/fonts/missing.woff2", want: served{Status: 404, ContentType: "text/plain; charset=utf-8", NoSniff: "nosniff"}},
		{name: "not a font", target: "/oauth/fonts/..%2Fconsent.html", want: served{Status: 404, ContentType: "text/plain; charset=utf-8", NoSniff: "nosniff"}},
	}
	for _, m := range urls {
		testCases = append(testCases, struct {
			name   string
			target string
			want   served
		}{name: m[1], target: m[1], want: served{Status: 200, ContentType: "font/woff2", CacheControl: "public, max-age=86400", NoSniff: "nosniff"}})
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			rec := e.do(t, testCase.target, nil)
			got := served{
				Status:       rec.Code,
				ContentType:  rec.Header().Get("Content-Type"),
				CacheControl: rec.Header().Get("Cache-Control"),
				NoSniff:      rec.Header().Get("X-Content-Type-Options"),
				Empty:        rec.Code == http.StatusOK && rec.Body.Len() == 0,
			}
			if diff := cmp.Diff(testCase.want, got); diff != "" {
				t.Errorf("response mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
