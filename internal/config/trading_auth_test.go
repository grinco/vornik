package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Backlog: "HMAC/mTLS on /internal/trading-*". These pin the config
// surface for the trading-auth feature: a secret_file is resolved into
// Secret and the path cleared; Enabled=true without a secret fails
// validation (fail-closed config); the feature defaults off.

func baseValidConfig() *Config {
	c := DefaultConfig()
	c.Server.Address = "127.0.0.1:8080"
	c.Database.Driver = "sqlite"
	c.Database.Path = "/tmp/x.db"
	c.API.AuthEnabled = false
	c.API.APIKeys = nil
	return c
}

func TestTradingAuthDefaultsOff(t *testing.T) {
	c := baseValidConfig()
	if c.Trading.Auth.Enabled {
		t.Fatal("trading auth must default to disabled")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("disabled trading auth must validate cleanly: %v", err)
	}
}

func TestTradingAuthEnabledRequiresSecret(t *testing.T) {
	c := baseValidConfig()
	c.Trading.Auth.Enabled = true
	if err := c.Validate(); err == nil {
		t.Fatal("trading.auth.enabled=true without a secret must fail validation")
	}
	c.Trading.Auth.Secret = "a-secret"
	if err := c.Validate(); err != nil {
		t.Fatalf("trading auth with a secret must validate: %v", err)
	}
}

func TestTradingAuthBadClockSkew(t *testing.T) {
	c := baseValidConfig()
	c.Trading.Auth.Enabled = true
	c.Trading.Auth.Secret = "a-secret"
	c.Trading.Auth.ClockSkew = "not-a-duration"
	if err := c.Validate(); err == nil {
		t.Fatal("invalid trading.auth.clock_skew must fail validation")
	}
}

func TestTradingAuthSecretFileResolved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trading.secret")
	if err := os.WriteFile(path, []byte("  file-secret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgYAML := "" +
		"server:\n  address: 127.0.0.1:8080\n" +
		"database:\n  driver: sqlite\n  path: /tmp/x.db\n" +
		"api:\n  auth_enabled: false\n" +
		"trading:\n  auth:\n    enabled: true\n    secret_file: " + path + "\n"
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFromPath(cfgPath)
	if err != nil {
		t.Fatalf("LoadFromPath: %v", err)
	}
	if got := c.Trading.Auth.Secret; got != "file-secret-value" {
		t.Fatalf("secret = %q, want trimmed file contents", got)
	}
	if c.Trading.Auth.SecretFile != "" {
		t.Errorf("secret_file should be cleared after resolution, got %q", c.Trading.Auth.SecretFile)
	}
}

func TestTradingAuthSecretFileMissing(t *testing.T) {
	c := baseValidConfig()
	c.Trading.Auth.SecretFile = filepath.Join(t.TempDir(), "does-not-exist")
	if err := resolveTradingSecret(c); err == nil || !strings.Contains(err.Error(), "trading") {
		t.Fatalf("missing secret file must be a fatal trading error, got %v", err)
	}
}
