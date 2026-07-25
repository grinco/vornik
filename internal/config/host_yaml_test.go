package config

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// Regression: fresh-install workspace "Permission denied" (2026-07-25).
// Rootless default userns remaps the agent uid to a subordinate host uid,
// so the workspace bind mount is unwritable even when the baked uid matches.
// keep-id maps it back to the real host uid. See onboarding-hardening-design F3a.
//
// Uses a direct yaml.Unmarshal into Config (the package's existing byte-slice
// parse idiom, e.g. TestComposerParseFromYAML) rather than a bytes-loader
// helper — none exists in this package, and LoadFromPath's full Validate()
// pass would fail on this file's unexpanded ${VAR} placeholders (they're only
// expanded by expandEnvPlaceholders inside LoadFromPath itself, against
// process env vars this test doesn't set).
func TestHostYAMLShipsKeepID(t *testing.T) {
	data, err := os.ReadFile("../../deployments/podman/config/vornik.host.yaml")
	if err != nil {
		t.Fatalf("read host yaml: %v", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Runtime.UserNSMode != "keep-id" {
		t.Fatalf("host template must ship userns_mode=keep-id, got %q", cfg.Runtime.UserNSMode)
	}
}
