package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGateway_EnvOverridesAndSecretFile(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "gateway.token")
	if err := os.WriteFile(tokenFile, []byte("  s3cr3t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{}
	cfg.Gateway = GatewayConfig{Enabled: true, TokenFile: tokenFile}

	t.Setenv("VORNIK_GATEWAY_ADDRESS", "http://127.0.0.1:8010")
	applyEnvOverrides(cfg)
	if cfg.Gateway.Address != "http://127.0.0.1:8010" {
		t.Fatalf("address = %q, want env value", cfg.Gateway.Address)
	}

	if err := resolveGatewaySecret(cfg); err != nil {
		t.Fatalf("resolveGatewaySecret: %v", err)
	}
	if cfg.Gateway.Token != "s3cr3t" {
		t.Errorf("token = %q, want resolved+trimmed secret", cfg.Gateway.Token)
	}
	if cfg.Gateway.TokenFile != "" {
		t.Errorf("token_file should be cleared after resolution, got %q", cfg.Gateway.TokenFile)
	}
}

// TestGateway_AgentWritesEnvOverride — VORNIK_GATEWAY_AGENT_WRITES routes into
// GatewayConfig.AgentWrites so the documented env override actually takes
// effect, and an INVALID env value is a startup error via the same
// normalize+validate path (never a silent off). Guards the review-caught gap
// where the doc promised an env override that wasn't wired.
func TestGateway_AgentWritesEnvOverride(t *testing.T) {
	cfg := &Config{}
	t.Setenv("VORNIK_GATEWAY_AGENT_WRITES", "user")
	applyEnvOverrides(cfg)
	if cfg.Gateway.AgentWrites != "user" {
		t.Fatalf("AgentWrites = %q, want env value 'user'", cfg.Gateway.AgentWrites)
	}
	if mode, err := cfg.Gateway.AgentWritesMode(); err != nil || mode != "user" {
		t.Fatalf("AgentWritesMode() = %q, %v; want user, nil", mode, err)
	}

	bad := &Config{}
	t.Setenv("VORNIK_GATEWAY_AGENT_WRITES", "true")
	applyEnvOverrides(bad)
	if _, err := bad.Gateway.AgentWritesMode(); err == nil {
		t.Fatal("an invalid env value must fail validation, not silently fall through to off")
	}
}

func TestGateway_ValidateRequiresTokenWhenEnabled(t *testing.T) {
	cfg := &Config{}
	cfg.Gateway = GatewayConfig{Enabled: true} // no token, no token_file
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error when gateway enabled without a token")
	}
}

// gatewayValidBase returns the minimal config that passes every Validate rule
// preceding the gateway.address loopback guard, with the gateway enabled and a
// token present so the guard under test is the one that fires.
func gatewayValidBase() *Config {
	c := &Config{}
	c.Server.Address = ":8080"
	c.Database = DatabaseConfig{Driver: "postgres", Host: "localhost", Port: 5432, Name: "vornik", User: "vornik"}
	c.Gateway = GatewayConfig{Enabled: true, Token: "tok"}
	return c
}

// review B1/F1: a gateway.address that is not loopback is misconfiguration and
// would defeat the SSRF host-pin (design §5, C2). Validate must reject it.
func TestGateway_ValidateRejectsNonLoopbackAddress(t *testing.T) {
	for _, addr := range []string{"http://example.com:8010", "http://169.254.169.254"} {
		c := gatewayValidBase()
		c.Gateway.Address = addr
		if err := c.Validate(); err == nil {
			t.Errorf("address %q: expected loopback validation error, got nil", addr)
		}
	}
}

func TestGateway_ValidateAcceptsLoopbackAddress(t *testing.T) {
	for _, addr := range []string{"http://127.0.0.1:8010", "http://localhost:8010", "http://[::1]:8010"} {
		c := gatewayValidBase()
		c.Gateway.Address = addr
		if err := c.Validate(); err != nil {
			t.Errorf("loopback address %q: unexpected error: %v", addr, err)
		}
	}
}

// TestProviderConfig_ExamplesUnmarshal covers query_api provider-discovery
// design §4.1: a gateway.providers.<name>.examples YAML list unmarshals
// into ProviderConfig.Examples.
func TestProviderConfig_ExamplesUnmarshal(t *testing.T) {
	src := `
gateway:
  enabled: true
  providers:
    maps:
      base_path: "/maps"
      allowed_methods: ["GET", "HEAD"]
      description: "Google Maps API (geocoding, places, directions) — read-only."
    headmatch-ats:
      base_path: "/ats"
      allowed_methods: ["GET"]
      description: "Internal ATS candidate search."
      examples:
        - "/candidates/search?q= — search candidates by keyword"
        - "/candidates/{id} — fetch one candidate"
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}

	maps, ok := cfg.Gateway.Providers["maps"]
	if !ok {
		t.Fatal("expected provider \"maps\" to be present")
	}
	if maps.Examples != nil {
		t.Errorf("maps.Examples = %#v, want nil (examples omitted)", maps.Examples)
	}

	ats, ok := cfg.Gateway.Providers["headmatch-ats"]
	if !ok {
		t.Fatal("expected provider \"headmatch-ats\" to be present")
	}
	want := []string{
		"/candidates/search?q= — search candidates by keyword",
		"/candidates/{id} — fetch one candidate",
	}
	if len(ats.Examples) != len(want) {
		t.Fatalf("headmatch-ats.Examples = %v, want %v", ats.Examples, want)
	}
	for i, v := range want {
		if ats.Examples[i] != v {
			t.Errorf("headmatch-ats.Examples[%d] = %q, want %q", i, ats.Examples[i], v)
		}
	}
}

// --- publication disclosure (G6 finding B, 2026-07-29) ---
//
// A gateway provider can be a human-facing publication surface: the `moltbook`
// provider autonomously posts to a public social platform every 6h/24h. That
// path never touches dispatcher/channel_receiver.go, so the Art 50(1) channel
// chokepoint does not cover it, and disclosure was carried only by a prompt
// instruction in a knowledge skill. Design:
// https://docs.vornik.io §5

func TestProviderDisclosure_RequiredWithNoContentFieldsIsAConfigError(t *testing.T) {
	cfg := &Config{}
	cfg.Gateway = GatewayConfig{Enabled: true, Providers: map[string]ProviderConfig{
		"moltbook": {
			BasePath:      "/moltbook",
			WritesEnabled: true,
			Disclosure:    ProviderDisclosureConfig{Required: true},
		},
	}}
	err := cfg.Gateway.ValidateDisclosure()
	if err == nil {
		t.Fatal("disclosure.required with no content_fields must be a config error, " +
			"not a silent gate that inspects nothing")
	}
	if !strings.Contains(err.Error(), "content_fields") {
		t.Errorf("error should name content_fields, got %v", err)
	}
}

func TestProviderDisclosure_RequiredWithContentFieldsIsValid(t *testing.T) {
	cfg := &Config{}
	cfg.Gateway = GatewayConfig{Enabled: true, Providers: map[string]ProviderConfig{
		"moltbook": {
			BasePath:      "/moltbook",
			WritesEnabled: true,
			Disclosure:    ProviderDisclosureConfig{Required: true, ContentFields: []string{"content"}},
		},
	}}
	if err := cfg.Gateway.ValidateDisclosure(); err != nil {
		t.Fatalf("valid disclosure config rejected: %v", err)
	}
}

// A provider that is not a publication surface needs no disclosure block, and
// must not acquire one by default — every existing provider stays unaffected.
func TestProviderDisclosure_NotRequiredByDefault(t *testing.T) {
	cfg := &Config{}
	cfg.Gateway = GatewayConfig{Enabled: true, Providers: map[string]ProviderConfig{
		"google-maps": {BasePath: "/maps"},
	}}
	if err := cfg.Gateway.ValidateDisclosure(); err != nil {
		t.Fatalf("provider without a disclosure block must be valid: %v", err)
	}
	if cfg.Gateway.Providers["google-maps"].Disclosure.Required {
		t.Error("disclosure must not default to required")
	}
}

// Whitespace-only field names are a misconfiguration that would silently check
// nothing — the same class of hole as a blank disclosure text.
func TestProviderDisclosure_BlankContentFieldNameIsAConfigError(t *testing.T) {
	cfg := &Config{}
	cfg.Gateway = GatewayConfig{Enabled: true, Providers: map[string]ProviderConfig{
		"moltbook": {Disclosure: ProviderDisclosureConfig{Required: true, ContentFields: []string{" "}}},
	}}
	if err := cfg.Gateway.ValidateDisclosure(); err == nil {
		t.Fatal("a whitespace-only content field name must be a config error")
	}
}
