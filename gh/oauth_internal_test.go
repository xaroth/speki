// Internal: these tests swap the client's HTTP transport for an in-memory network that fails dials
// and handshakes on cue, which the exported API can't reach.
package gh

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

// testKey is generated once: RSA key generation is slow, more so with -race.
var testKey = sync.OnceValues(func() ([]byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), nil
})

// pipeGitHub is a fake GitHub token endpoint on an in-memory network, so tests run in a synctest bubble.
// Its dials fail with dialErrs in order, then connect.
type pipeGitHub struct {
	conns    chan net.Conn
	dialErrs []error
	dials    atomic.Int32
	handled  atomic.Int32
	once     sync.Once
}

func (p *pipeGitHub) Accept() (net.Conn, error) {
	c, ok := <-p.conns
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}

func (p *pipeGitHub) Close() error {
	p.once.Do(func() { close(p.conns) })
	return nil
}

func (p *pipeGitHub) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 443} }

func (p *pipeGitHub) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	n := int(p.dials.Add(1))
	if n <= len(p.dialErrs) {
		return nil, p.dialErrs[n-1]
	}
	client, server := net.Pipe()
	select {
	case p.conns <- server:
		return client, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// startPipeGitHub serves handler, or a good token answer when it's nil, and points a client at it. With
// tls set, the client speaks TLS to a server that doesn't, so every handshake fails.
func startPipeGitHub(t testing.TB, dialErrs []error, tls bool, handler func(n int, w http.ResponseWriter, r *http.Request)) (*Client, OAuthConfig, *pipeGitHub) {
	t.Helper()
	p := &pipeGitHub{conns: make(chan net.Conn), dialErrs: dialErrs}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(p.handled.Add(1))
		if handler != nil {
			handler(n, w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"ghu_1","expires_in":28800,"refresh_token":"ghr_2"}`))
	})}
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(p)
		close(done)
	}()
	transport := &http.Transport{DialContext: p.dial}
	t.Cleanup(func() {
		transport.CloseIdleConnections()
		_ = srv.Close()
		<-done
	})
	key, err := testKey()
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{AppID: 1, PrivateKey: key})
	if err != nil {
		t.Fatal(err)
	}
	c.web = &http.Client{Transport: transport}
	base := "http://github.example"
	if tls {
		base = "https://github.example"
	}
	return c, OAuthConfig{ClientID: "id", ClientSecret: "secret", URL: base}, p
}

// A token request is tried again only when it failed before it was sent: GitHub spends a refresh token
// it has seen, even when its answer never arrives.
func TestTokenRequestTriesAgainOnlyBeforeSending(t *testing.T) {
	t.Parallel()
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	noHost := &net.DNSError{Err: "no such host", Name: "github.example", IsNotFound: true}
	testCases := []struct {
		name        string
		dialErrs    []error
		tls         bool
		handler     func(n int, w http.ResponseWriter, r *http.Request)
		timeout     time.Duration
		wantErr     bool
		unreached   bool
		wantDials   int
		wantHandled int
		wantTook    time.Duration
	}{
		{
			name:        "connection refused, then connected",
			dialErrs:    []error{refused, refused},
			wantDials:   3,
			wantHandled: 1,
			wantTook:    3 * tokenTryPause,
		},
		{
			name:      "name lookup fails every time",
			dialErrs:  []error{noHost, noHost, noHost, noHost},
			wantErr:   true,
			unreached: true,
			wantDials: 3,
			wantTook:  3 * tokenTryPause,
		},
		{
			name:      "TLS handshake fails every time",
			tls:       true,
			wantErr:   true,
			unreached: true,
			wantDials: 3,
			wantTook:  3 * tokenTryPause,
		},
		{
			name: "no answer after sending",
			handler: func(_ int, _ http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body) // so the server notices the client leaving
				<-r.Context().Done()
			},
			timeout:     time.Second,
			wantErr:     true,
			wantDials:   1,
			wantHandled: 1,
			wantTook:    time.Second,
		},
		{
			name: "connection dropped after sending",
			handler: func(_ int, w http.ResponseWriter, _ *http.Request) {
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
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				c, o, p := startPipeGitHub(t, testCase.dialErrs, testCase.tls, testCase.handler)
				ctx := t.Context()
				if testCase.timeout > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, testCase.timeout)
					defer cancel()
				}
				start := time.Now()
				_, err := c.Refresh(ctx, o, "ghr_1")
				if (err != nil) != testCase.wantErr || errors.Is(err, ErrUnreached) != testCase.unreached {
					t.Errorf("Refresh: got %v, want error %t, ErrUnreached %t", err, testCase.wantErr, testCase.unreached)
				}
				if got := int(p.dials.Load()); got != testCase.wantDials {
					t.Errorf("got %d dials, want %d", got, testCase.wantDials)
				}
				if got := int(p.handled.Load()); got != testCase.wantHandled {
					t.Errorf("got %d requests to GitHub, want %d", got, testCase.wantHandled)
				}
				if took := time.Since(start); took != testCase.wantTook {
					t.Errorf("got %v taken, want %v", took, testCase.wantTook)
				}
			})
		})
	}
}

// A kept-alive connection that dies after the request was written may have delivered it: no new try.
func TestTokenRequestOnAPooledConnectionThatDies(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		c, o, p := startPipeGitHub(t, nil, false, func(n int, w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			if n == 2 {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"ghu_1","expires_in":28800,"refresh_token":"ghr_2"}`))
		})
		_, err := c.Refresh(t.Context(), o, "ghr_1")
		if err != nil {
			t.Fatalf("first Refresh: got %v, want no error", err)
		}
		_, err = c.Refresh(t.Context(), o, "ghr_2")
		if err == nil || errors.Is(err, ErrUnreached) {
			t.Errorf("second Refresh: got %v, want an error other than ErrUnreached", err)
		}
		if dials, handled := p.dials.Load(), p.handled.Load(); dials != 1 || handled != 2 {
			t.Errorf("got %d dials and %d requests, want 1 and 2", dials, handled)
		}
	})
}

func TestTokenRequestStopsTryingAtTheDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
		c, o, _ := startPipeGitHub(t, []error{refused, refused, refused}, false, nil)
		ctx, cancel := context.WithTimeout(t.Context(), tokenTryPause/2)
		defer cancel()
		start := time.Now()
		_, err := c.Refresh(ctx, o, "ghr_1")
		if !errors.Is(err, ErrUnreached) {
			t.Errorf("Refresh: got %v, want ErrUnreached", err)
		}
		if took := time.Since(start); took != tokenTryPause/2 {
			t.Errorf("got %v before giving up, want %v", took, tokenTryPause/2)
		}
	})
}
