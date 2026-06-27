package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestAdminConfig_IsAdminKey locks the helper's gating: empty
// keys never match, disabled config never matches, and matching
// requires exact-string equality against admin.allowed_keys.
func TestAdminConfig_IsAdminKey(t *testing.T) {
	cases := []struct {
		name    string
		cfg     AdminConfig
		key     string
		wantHit bool
	}{
		{"disabled never matches", AdminConfig{Enabled: false, AllowedKeys: []string{"sk-x"}}, "sk-x", false},
		{"empty key never matches", AdminConfig{Enabled: true, AllowedKeys: []string{"sk-x"}}, "", false},
		{"non-member returns false", AdminConfig{Enabled: true, AllowedKeys: []string{"sk-x"}}, "sk-y", false},
		{"exact member returns true", AdminConfig{Enabled: true, AllowedKeys: []string{"sk-x", "sk-y"}}, "sk-y", true},
		{"empty allowlist never matches", AdminConfig{Enabled: true, AllowedKeys: []string{}}, "sk-x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.IsAdminKey(tc.key); got != tc.wantHit {
				t.Fatalf("IsAdminKey(%q) = %v, want %v", tc.key, got, tc.wantHit)
			}
		})
	}
}

// TestAdminConfig_YAMLUnmarshal verifies the YAML shape land on
// the struct as advertised in the design doc.
func TestAdminConfig_YAMLUnmarshal(t *testing.T) {
	const sample = `
admin:
  enabled: true
  allowed_keys:
    - sk-vornik-admin-1
    - sk-vornik-admin-2
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(sample), &cfg); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	if !cfg.Admin.Enabled {
		t.Error("Admin.Enabled: want true")
	}
	if len(cfg.Admin.AllowedKeys) != 2 || cfg.Admin.AllowedKeys[0] != "sk-vornik-admin-1" {
		t.Errorf("Admin.AllowedKeys: got %v", cfg.Admin.AllowedKeys)
	}
}

// TestAdminConfig_DefaultsToDisabled — operator who hasn't opted
// in still parses cleanly and lands with Enabled=false.
func TestAdminConfig_DefaultsToDisabled(t *testing.T) {
	const sample = `server:
  address: ":8080"`
	var cfg Config
	if err := yaml.Unmarshal([]byte(sample), &cfg); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	if cfg.Admin.Enabled {
		t.Error("Admin.Enabled: should default to false")
	}
	if len(cfg.Admin.AllowedKeys) != 0 {
		t.Errorf("Admin.AllowedKeys: should default to empty, got %v", cfg.Admin.AllowedKeys)
	}
}
