package oauth_test

import (
	"net/http"
	"strings"
	"testing"
)

// The real transport refuses addresses that aren't public, whatever the host name says.
func TestMetadataFetchRefusesPrivateAddresses(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withRealMetadataTransport(), withMetadataHosts("127.0.0.1"))
	rec := e.do(t, authorizeURL("https://127.0.0.1/client.json", "https://127.0.0.1/cb", nil), nil)
	checkErrorPage(t, rec, http.StatusBadRequest, "This program isn't known to speki.", stepChoose)
	if !strings.Contains(e.logs.String(), "refusing to connect to 127.0.0.1") {
		t.Errorf("got logs %s, want the connection refused", e.logs)
	}
}
