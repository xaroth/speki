package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	environ := []string{"SPEKI_LISTEN=127.0.0.1:0", "SPEKI_METRICS_LISTEN=127.0.0.1:0"}
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
