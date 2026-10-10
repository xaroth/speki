package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// A secret doesn't show in any of the ways a config might be printed.
func TestSecretIsRedacted(t *testing.T) {
	cfg := Config{GitHub: GitHub{ClientSecret: "hunter2"}}
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("starting", "github", cfg.GitHub, "secret", cfg.GitHub.ClientSecret)
	asJSON, _ := json.Marshal(cfg)
	asYAML, _ := yaml.Marshal(cfg)
	for name, out := range map[string]string{
		"%v":   fmt.Sprintf("%v", cfg),
		"%+v":  fmt.Sprintf("%+v", cfg),
		"%#v":  fmt.Sprintf("%#v", cfg),
		"%q":   fmt.Sprintf("%q", cfg.GitHub.ClientSecret),
		"slog": logged.String(),
		"json": string(asJSON),
		"yaml": string(asYAML),
	} {
		if strings.Contains(out, "hunter2") || !strings.Contains(out, "[redacted]") {
			t.Errorf("%s: %s", name, out)
		}
	}
	if cfg.GitHub.ClientSecret.Reveal() != "hunter2" {
		t.Error("Reveal lost the value")
	}
}
