// Internal: package main can't be imported, so its tests live inside it.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/xaroth/speki/service"
)

func TestLoadConfig(t *testing.T) {
	t.Parallel()
	keyFile := writeFile(t, "-----BEGIN KEY-----\nabc\n-----END KEY-----\n")
	key := "-----BEGIN KEY-----\nabc\n-----END KEY-----" // the trailing line break dropped
	listenFile := writeFile(t, ":8081\n")
	missing := filepath.Join(t.TempDir(), "absent")

	testCases := []struct {
		name    string
		file    string // the config file; "missing" for a path that doesn't exist
		environ []string
		want    service.Config
		wantErr string
	}{
		{
			name: "file only",
			file: "listen: \":8000\"\ntelemetry:\n  log_level: debug\nsync:\n  discovery_interval: 2m\ngithub:\n  allowed_accounts: [7]\n",
			want: service.Config{
				Listen:    ":8000",
				Telemetry: service.Telemetry{LogLevel: "debug"},
				GitHub:    service.GitHub{AllowedAccounts: []int64{7}},
				Sync:      service.Sync{DiscoveryInterval: 2 * time.Minute},
			},
		},
		{
			name:    "environment wins over file",
			file:    "listen: \":8000\"\n",
			environ: []string{"SPEKI_LISTEN=:7000", "HOME=/root"},
			want:    service.Config{Listen: ":7000"},
		},
		{
			name:    "lists, numbers, durations and booleans from the environment",
			environ: []string{"SPEKI_GITHUB_APP_ID=12", "SPEKI_GITHUB_ALLOWED_ACCOUNTS=1, 2", "SPEKI_SYNC_POLL_INTERVAL=30s", "SPEKI_TELEMETRY_TRACING=true"},
			want: service.Config{
				GitHub:    service.GitHub{AppID: 12, AllowedAccounts: []int64{1, 2}},
				Sync:      service.Sync{PollInterval: 30 * time.Second},
				Telemetry: service.Telemetry{Tracing: true},
			},
		},
		{
			name:    "other SPEKI variables are ignored",
			environ: []string{"SPEKI_PORT=tcp://10.0.0.1:8080", "SPEKI_SERVICE_HOST=10.0.0.1"},
		},
		{name: "empty file", file: ""},
		{name: "empty document", file: "---\n"},
		{name: "secret file in YAML", file: "github:\n  private_key_file: " + keyFile + "\n", want: service.Config{GitHub: service.GitHub{PrivateKey: key}}},
		{name: "secret file variable", environ: []string{"SPEKI_GITHUB_PRIVATE_KEY_FILE=" + keyFile}, want: service.Config{GitHub: service.GitHub{PrivateKey: key}}},
		{name: "top-level text from a file", file: "listen_file: " + listenFile + "\n", want: service.Config{Listen: ":8081"}},
		{
			name:    "environment over a secret file in YAML",
			file:    "github:\n  private_key_file: /run/secrets/not-mounted\n",
			environ: []string{"SPEKI_GITHUB_PRIVATE_KEY_FILE=" + keyFile},
			want:    service.Config{GitHub: service.GitHub{PrivateKey: key}},
		},
		{name: "missing explicit file", file: "missing", wantErr: "read config"},
		{name: "bad number", environ: []string{"SPEKI_GITHUB_APP_ID=twelve"}, wantErr: "SPEKI_GITHUB_APP_ID"},
		{name: "secret twice in YAML", file: "github:\n  private_key: x\n  private_key_file: " + keyFile + "\n", wantErr: "both set"},
		{name: "secret twice in the environment", environ: []string{"SPEKI_GITHUB_PRIVATE_KEY=x", "SPEKI_GITHUB_PRIVATE_KEY_FILE=" + keyFile}, wantErr: "both set"},
		{name: "missing secret file variable", environ: []string{"SPEKI_GITHUB_PRIVATE_KEY_FILE=" + missing}, wantErr: "PRIVATE_KEY_FILE"},
		{name: "missing secret file in YAML", file: "github:\n  app_id: 1\n  private_key_file: " + missing + "\n", wantErr: "line 3: private_key_file"},
		{name: "file for a setting that isn't text", file: "sync:\n  poll_interval_file: x\n", wantErr: "unknown setting \"poll_interval_file\""},
		{name: "empty file path", file: "github:\n  private_key_file: \"\"\n", wantErr: "want a file path"},
		// Errors name the line as written, whatever secret files the YAML takes out.
		{name: "unknown key", file: "# comment\n\ngithub:\n  private_key_file: " + keyFile + "\n  app_id: 1\n  typo: x\n", wantErr: "line 6: unknown setting \"typo\""},
		{name: "bad value", file: "github:\n  private_key_file: " + keyFile + "\nsync:\n  poll_interval: soon\n", wantErr: "line 4"},
		{name: "not a mapping", file: "- a\n- b\n", wantErr: "line 1: want a mapping"},
		{name: "section as text", file: "github: just text\n", wantErr: "line 1"},
		{name: "anchors", file: "base: &b\n  app_id: 1\ngithub: *b\n", wantErr: "anchors"},
		{name: "merge keys", file: "github:\n  <<: {app_id: 1}\n", wantErr: "merge keys"},
		{name: "repeated key", file: "github:\n  private_key_file: a\n  private_key_file: b\n", wantErr: "line 3: private_key_file appears twice"},
		{name: "key that is no setting name", file: "-: x\n", wantErr: "unknown setting"},
		{name: "not yaml", file: "listen: [\n", wantErr: "config "},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			path := missing
			if testCase.file != "missing" {
				path = writeFile(t, testCase.file)
			}

			got, err := loadConfig(path, testCase.environ)

			if testCase.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
					t.Fatalf("got error %v, want it to mention %q", err, testCase.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if diff := cmp.Diff(testCase.want, got); diff != "" {
				t.Errorf("config (-want +got):\n%s", diff)
			}
		})
	}
}

// Not parallel: t.Chdir, for the default config file in the working directory.
func TestLoadConfigDefaultFile(t *testing.T) {
	t.Chdir(t.TempDir())
	environ := []string{"SPEKI_METRICS_LISTEN=:9999"}

	got, err := loadConfig("", environ)
	if err != nil {
		t.Fatalf("missing default file: %v", err)
	}
	if diff := cmp.Diff(service.Config{MetricsListen: ":9999"}, got); diff != "" {
		t.Errorf("missing default file: config (-want +got):\n%s", diff)
	}

	err = os.WriteFile(defaultConfigPath, []byte("listen: \":8000\"\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	got, err = loadConfig("", environ)
	if err != nil {
		t.Fatalf("default file: %v", err)
	}
	if diff := cmp.Diff(service.Config{Listen: ":8000", MetricsListen: ":9999"}, got); diff != "" {
		t.Errorf("default file: config (-want +got):\n%s", diff)
	}
}
