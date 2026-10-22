package gh

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bradleyfalzon/ghinstallation/v2"
)

// ghinstallation leaves a failed token response's body open; closeTokenError closes it, however wrapped.
func TestCloseTokenError(t *testing.T) {
	t.Parallel()
	body := &closeCounter{Reader: strings.NewReader("{}")}
	err := &ghinstallation.HTTPError{Response: &http.Response{StatusCode: http.StatusForbidden, Body: body}}
	closeTokenError(fmt.Errorf("wrapped: %w", err))
	closeTokenError(&ghinstallation.HTTPError{Response: &http.Response{}}) // no body: no panic
	if body.closed != 1 {
		t.Errorf("got the body closed %d times, want 1", body.closed)
	}
}

type closeCounter struct {
	io.Reader
	closed int
}

func (c *closeCounter) Close() error {
	c.closed++
	return nil
}
