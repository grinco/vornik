package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestIdentityConfig_DefaultsAndValidate(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Identity.Enabled {
		t.Fatal("identity must default OFF (byte-identical to the pre-2026-09-13 daemon)")
	}
	if cfg.Identity.EffectiveChannelCompat() != IdentityCompatStrict {
		t.Fatalf("new installs default strict, got %q", cfg.Identity.EffectiveChannelCompat())
	}
	if cfg.Identity.EffectiveLinkCodeTTL() != 10*time.Minute {
		t.Fatalf("link code ttl default 10m, got %s", cfg.Identity.EffectiveLinkCodeTTL())
	}
	if cfg.Identity.LinkCodeRatePerHour != IdentityDefaultLinkCodeRatePerHour {
		t.Fatalf("rate default, got %d", cfg.Identity.LinkCodeRatePerHour)
	}
	if err := cfg.Identity.Validate(); err != nil {
		t.Fatalf("default identity block must validate: %v", err)
	}

	bad := IdentityConfig{ChannelCompat: "permissive"}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "channel_compat") {
		t.Fatalf("unknown compat mode must be refused, got %v", err)
	}
	bad = IdentityConfig{LinkCodeTTL: "soon"}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "link_code_ttl") {
		t.Fatalf("bad ttl must be refused, got %v", err)
	}
	legacy := IdentityConfig{ChannelCompat: " Legacy "}
	if !legacy.LegacyCompat() {
		t.Fatal("compat mode is case/space-insensitive")
	}
}

func TestIdentityConfig_YAMLRoundTrip(t *testing.T) {
	src := "api:\n  auth_enabled: false\nidentity:\n  enabled: true\n  channel_compat: legacy\n  link_code_ttl: 5m\n"
	cfg := DefaultConfig()
	if err := yaml.Unmarshal([]byte(src), cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Identity.Enabled || !cfg.Identity.LegacyCompat() || cfg.Identity.EffectiveLinkCodeTTL() != 5*time.Minute {
		t.Fatalf("yaml did not land: %+v", cfg.Identity)
	}
	if err := ValidateBytes([]byte(src)); err != nil {
		t.Fatalf("ValidateBytes: %v", err)
	}
	// The doctor gate key must resolve against the struct.
	if _, ok := LookupByPath(cfg, "identity.enabled"); !ok {
		t.Fatal("identity.enabled must resolve via LookupByPath")
	}
}
