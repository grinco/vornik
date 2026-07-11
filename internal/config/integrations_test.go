package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestIntegrationsConfig_YAMLUnmarshal locks the YAML shape (task 5.4,
// design §6): a top-level `integrations.allowed_hosts` list populates
// Config.Integrations.AllowedHosts.
func TestIntegrationsConfig_YAMLUnmarshal(t *testing.T) {
	const sample = `
integrations:
  allowed_hosts:
    - imap.internal.example.com
    - mcp.lan
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(sample), &cfg); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	want := []string{"imap.internal.example.com", "mcp.lan"}
	got := cfg.Integrations.AllowedHosts
	if len(got) != len(want) {
		t.Fatalf("AllowedHosts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("AllowedHosts[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestIntegrationsConfig_DefaultsToEmptyAllowlist — an omitted
// `integrations:` block must leave AllowedHosts nil/empty, the secure
// default the DialGuard relies on to block every private/loopback/
// link-local destination outright (design §6).
func TestIntegrationsConfig_DefaultsToEmptyAllowlist(t *testing.T) {
	const sample = `server:
  address: ":8080"`
	var cfg Config
	if err := yaml.Unmarshal([]byte(sample), &cfg); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	if len(cfg.Integrations.AllowedHosts) != 0 {
		t.Errorf("AllowedHosts should default empty, got %v", cfg.Integrations.AllowedHosts)
	}
}

// TestIntegrationsConfig_DefaultConfigLeavesEmptyAllowlist — the
// programmatic DefaultConfig() (used when no YAML is loaded at all) must
// carry the same secure default as an omitted YAML block.
func TestIntegrationsConfig_DefaultConfigLeavesEmptyAllowlist(t *testing.T) {
	cfg := DefaultConfig()
	if len(cfg.Integrations.AllowedHosts) != 0 {
		t.Errorf("DefaultConfig().Integrations.AllowedHosts should default empty, got %v", cfg.Integrations.AllowedHosts)
	}
}
