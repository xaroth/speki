// Internal: package main can't be imported, so its tests live inside it.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	t.Parallel()
	environ := []string{
		"SPEKI_LISTEN=127.0.0.1:0",
		"SPEKI_METRICS_LISTEN=127.0.0.1:0",
		"SPEKI_PUBLIC_URL=https://speki.example.com",
		"SPEKI_GITHUB_APP_ID=1",
		"SPEKI_GITHUB_PRIVATE_KEY_FILE=" + writeFile(t, string(testKey(t))),
		"SPEKI_GITHUB_CLIENT_ID=Iv1",
		"SPEKI_GITHUB_CLIENT_SECRET=secret",
		"SPEKI_AUTH_KEYS=keys: [{id: a, secret: MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=, primary: true}]",
		"SPEKI_GITHUB_ALLOWED_ACCOUNTS=42",
		"SPEKI_GITHUB_API_URL=http://127.0.0.1:1", // nothing listens: the run is cancelled straight away
	}
	emptyConfig := writeFile(t, "")

	testCases := []struct {
		name       string
		args       []string
		environ    []string
		wantStdout string
		wantErr    string
	}{
		{name: "runs until cancelled", args: []string{"--config", emptyConfig}, environ: environ},
		{name: "version", args: []string{"--version"}, wantStdout: version + "\n"},
		{name: "unknown flag", args: []string{"--verbose"}, wantErr: "flag provided but not defined"},
		{name: "unexpected argument", args: []string{"--config", emptyConfig, "serve"}, environ: environ, wantErr: "unexpected arguments"},
		{name: "missing explicit file", args: []string{"--config=" + filepath.Join(t.TempDir(), "absent.yaml")}, wantErr: "read config"},
		{name: "file from environment", environ: append([]string{"SPEKI_CONFIG=" + emptyConfig}, environ...)},
		{
			name:    "missing file from environment",
			environ: []string{"SPEKI_CONFIG=" + filepath.Join(t.TempDir(), "absent.yaml")},
			wantErr: "read config",
		},
		{
			name:    "flag wins over environment",
			args:    []string{"--config", emptyConfig},
			environ: append([]string{"SPEKI_CONFIG=" + filepath.Join(t.TempDir(), "absent.yaml")}, environ...),
		},
		{
			name:    "invalid configuration",
			args:    []string{"--config", writeFile(t, "listen: \":1\"\nmetrics_listen: \":1\"\n")},
			wantErr: "metrics_listen",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			environ := slices.Concat(testCase.environ, []string{"SPEKI_SYNC_DATA_DIR=" + t.TempDir()})

			err := run(ctx, testCase.args, environ, &stdout, &stderr)

			if testCase.wantErr == "" && err != nil {
				t.Errorf("got error %v, want nil", err)
			}
			if testCase.wantErr != "" && (err == nil || !strings.Contains(err.Error(), testCase.wantErr)) {
				t.Errorf("got error %v, want it to mention %q", err, testCase.wantErr)
			}
			if got := stdout.String(); got != testCase.wantStdout {
				t.Errorf("got stdout %q, want %q", got, testCase.wantStdout)
			}
		})
	}
}

func testKey(tb testing.TB) []byte {
	tb.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		tb.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}
