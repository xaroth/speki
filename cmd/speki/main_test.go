package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/eve-online-tools/speki/service"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	err := os.WriteFile(path, []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name    string
		path    string // "file": write file to a temp path; "missing": a path that doesn't exist; "": default
		file    string
		environ []string
		want    service.Config
		wantErr string
	}{
		{
			name: "file only",
			path: "file",
			file: "listen: \":8000\"\ntelemetry:\n  log_level: debug\n",
			want: service.Config{Listen: ":8000", Telemetry: service.Telemetry{LogLevel: "debug"}},
		},
		{
			name:    "environment wins over file",
			path:    "file",
			file:    "listen: \":8000\"\n",
			environ: []string{"SPEKI_LISTEN=:7000", "SPEKI_TELEMETRY_LOG_LEVEL=warn", "HOME=/root"},
			want:    service.Config{Listen: ":7000", Telemetry: service.Telemetry{LogLevel: "warn"}},
		},
		{
			name:    "missing default file is fine",
			environ: []string{"SPEKI_METRICS_LISTEN=:9999"},
			want:    service.Config{MetricsListen: ":9999"},
		},
		{
			name:    "missing explicit file is an error",
			path:    "missing",
			wantErr: "read config",
		},
		{
			name: "empty file",
			path: "file",
			want: service.Config{},
		},
		{
			name:    "unknown key",
			path:    "file",
			file:    "listen: \":8000\"\nlisten_port: 1\n",
			wantErr: "listen_port",
		},
		{
			name:    "lists, numbers and durations from the environment",
			environ: []string{"SPEKI_GITHUB_APP_ID=12", "SPEKI_GITHUB_ALLOWED_ACCOUNTS=1, 2", "SPEKI_SYNC_POLL_INTERVAL=30s"},
			want: service.Config{
				GitHub: service.GitHub{AppID: 12, AllowedAccounts: []int64{1, 2}},
				Sync:   service.Sync{PollInterval: 30 * time.Second},
			},
		},
		{
			name: "durations and lists in the file",
			path: "file",
			file: "sync:\n  discovery_interval: 2m\ngithub:\n  allowed_accounts: [7]\n",
			want: service.Config{
				GitHub: service.GitHub{AllowedAccounts: []int64{7}},
				Sync:   service.Sync{DiscoveryInterval: 2 * time.Minute},
			},
		},
		{
			name:    "bad number",
			environ: []string{"SPEKI_GITHUB_APP_ID=twelve"},
			wantErr: "SPEKI_GITHUB_APP_ID",
		},
		{
			name:    "other SPEKI variables are ignored",
			environ: []string{"SPEKI_PORT=tcp://10.0.0.1:8080", "SPEKI_SERVICE_HOST=10.0.0.1"},
			want:    service.Config{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir()) // no config.yaml in the working directory
			path := ""
			switch tt.path {
			case "file":
				path = writeFile(t, tt.file)
			case "missing":
				path = filepath.Join(t.TempDir(), "absent.yaml")
			}

			got, err := loadConfig(path, tt.environ)

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("config (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSecretsFromFiles(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key.pem")
	err := os.WriteFile(keyFile, []byte("-----BEGIN KEY-----\nabc\n-----END KEY-----\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	want := service.Secret("-----BEGIN KEY-----\nabc\n-----END KEY-----")

	tests := []struct {
		name    string
		file    string
		environ []string
		wantErr string
	}{
		{name: "file key in YAML", file: "github:\n  private_key_file: " + keyFile + "\n"},
		{name: "file variable", environ: []string{"SPEKI_GITHUB_PRIVATE_KEY_FILE=" + keyFile}},
		{
			name:    "both in YAML",
			file:    "github:\n  private_key: x\n  private_key_file: " + keyFile + "\n",
			wantErr: "both set",
		},
		{
			name:    "both in the environment",
			environ: []string{"SPEKI_GITHUB_PRIVATE_KEY=x", "SPEKI_GITHUB_PRIVATE_KEY_FILE=" + keyFile},
			wantErr: "both set",
		},
		{name: "missing file", environ: []string{"SPEKI_GITHUB_PRIVATE_KEY_FILE=/nonexistent"}, wantErr: "PRIVATE_KEY_FILE"},
		{name: "file key that isn't a secret", file: "listen_file: x\n", wantErr: "listen_file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			path := ""
			if tt.file != "" {
				path = writeFile(t, tt.file)
			}
			cfg, err := loadConfig(path, tt.environ)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if cfg.GitHub.PrivateKey != want {
				t.Errorf("private key = %q, want %q", cfg.GitHub.PrivateKey.Reveal(), want.Reveal())
			}
		})
	}
}

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(t.Context(), []string{"--version"}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(stdout.String()) != version {
		t.Errorf("printed %q, want %q", stdout.String(), version)
	}
}

func TestRunRejectsBadConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	path := writeFile(t, "listen: \":1\"\nmetrics_listen: \":1\"\n")

	err := run(t.Context(), []string{"--config", path}, nil, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "metrics_listen") {
		t.Errorf("run = %v, want a configuration error", err)
	}
}

func TestRunConfigFile(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key.pem")
	err := os.WriteFile(keyFile, testKey(t), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	environ := []string{
		"SPEKI_LISTEN=127.0.0.1:0",
		"SPEKI_METRICS_LISTEN=127.0.0.1:0",
		"SPEKI_GITHUB_APP_ID=1",
		"SPEKI_GITHUB_PRIVATE_KEY_FILE=" + keyFile,
		"SPEKI_GITHUB_ALLOWED_ACCOUNTS=42",
		"SPEKI_GITHUB_API_URL=http://127.0.0.1:1", // nothing listens: the run is cancelled straight away
		"SPEKI_SYNC_DATA_DIR=" + t.TempDir(),
	}
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"explicit missing file", []string{"--config=" + filepath.Join(t.TempDir(), "absent.yaml")}, "read config"},
		{"default file may be missing", nil, ""},
		{"unexpected argument", []string{"serve"}, "unexpected arguments"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			var stdout, stderr bytes.Buffer
			ctx, cancel := context.WithCancel(t.Context())
			cancel() // start and stop straight away

			err := run(ctx, tt.args, environ, &stdout, &stderr)

			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("run: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("run = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func testKey(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}
