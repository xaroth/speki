package gh

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// A token request is tried again only when it failed before it was sent: GitHub spends a refresh token
// it has seen, even when its answer never arrives.
func TestTokenRequestTriesAgainOnlyBeforeSending(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	noHost := &net.DNSError{Err: "no such host", Name: "github.example", IsNotFound: true}
	tests := []struct {
		name        string
		dialErrs    []error // returned by the first dials, in order; later dials connect
		handler     http.HandlerFunc
		timeout     time.Duration
		wantErr     bool
		unreached   bool
		wantDials   int
		wantHandled int
	}{
		{
			name:        "connection refused, then connected",
			dialErrs:    []error{refused, refused},
			wantDials:   3,
			wantHandled: 1,
		},
		{
			name:      "name lookup fails every time",
			dialErrs:  []error{noHost, noHost, noHost, noHost},
			wantErr:   true,
			unreached: true,
			wantDials: 3,
		},
		{
			name: "no answer after sending",
			handler: func(_ http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body) // so the server notices the client leaving
				<-r.Context().Done()
			},
			timeout:     200 * time.Millisecond,
			wantErr:     true,
			wantDials:   1,
			wantHandled: 1,
		},
		{
			name: "connection dropped after sending",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
			},
			wantErr:     true,
			wantDials:   1,
			wantHandled: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var handled atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handled.Add(1)
				if tt.handler != nil {
					tt.handler(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"access_token":"ghu_1","expires_in":28800,"refresh_token":"ghr_2"}`))
			}))
			t.Cleanup(srv.Close)
			c, err := New(Config{AppID: 1, PrivateKey: testKey(t), APIURL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			var dials atomic.Int32
			c.web = &http.Client{Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					n := int(dials.Add(1))
					if n <= len(tt.dialErrs) {
						return nil, tt.dialErrs[n-1]
					}
					var d net.Dialer
					return d.DialContext(ctx, network, addr)
				},
			}}
			ctx := t.Context()
			if tt.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tt.timeout)
				defer cancel()
			}

			_, err = c.Refresh(ctx, OAuthConfig{ClientID: "id", ClientSecret: "secret", URL: srv.URL}, "ghr_1")
			if (err != nil) != tt.wantErr || errors.Is(err, ErrUnreached) != tt.unreached {
				t.Errorf("Refresh: %v", err)
			}
			if got := int(dials.Load()); got != tt.wantDials {
				t.Errorf("dialled %d times, want %d", got, tt.wantDials)
			}
			if got := int(handled.Load()); got != tt.wantHandled {
				t.Errorf("GitHub got %d requests, want %d", got, tt.wantHandled)
			}
		})
	}
}

func TestTokenRequestStopsTryingAtTheDeadline(t *testing.T) {
	c, o := newUserTestClient(t)
	c.web = &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
		},
	}}
	ctx, cancel := context.WithTimeout(t.Context(), tokenTryPause/2)
	defer cancel()
	start := time.Now()
	_, err := c.Refresh(ctx, o, "ghr_1")
	if !errors.Is(err, ErrUnreached) {
		t.Errorf("Refresh: %v", err)
	}
	if waited := time.Since(start); waited >= tokenTryPause {
		t.Errorf("gave up after %v, past the deadline", waited)
	}
}

func TestUnreached(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"dial", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		{"name lookup", &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host"}}, true},
		{"TLS handshake", &handshakeError{err: errors.New("remote error: tls: handshake failure")}, true},
		{"read", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, false},
		{"deadline", context.DeadlineExceeded, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := unreached(tt.err); got != tt.want {
				t.Errorf("unreached = %v, want %v", got, tt.want)
			}
		})
	}
}
