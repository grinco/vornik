package config

import "testing"

// TestParseEnvBool_RecognisesOperatorSpellings is the regression for
// the 2026-06-04 bug sweep: boolean env overrides used
// `EqualFold(v,"true") || v=="1"`, so any other truthy spelling
// silently became false. Most dangerously VORNIK_STORAGE_S3_FORCE_SSL=yes
// silently disabled TLS enforcement.
func TestParseEnvBool_RecognisesOperatorSpellings(t *testing.T) {
	truthy := []string{"1", "t", "T", "true", "TRUE", "True", "y", "Y", "yes", "YES", "on", "ON", " true ", "  yes"}
	for _, v := range truthy {
		got, ok := parseEnvBool(v)
		if !ok || !got {
			t.Errorf("parseEnvBool(%q) = (%v,%v), want (true,true)", v, got, ok)
		}
	}
	falsy := []string{"0", "f", "false", "FALSE", "n", "no", "off", "OFF", " false "}
	for _, v := range falsy {
		got, ok := parseEnvBool(v)
		if !ok || got {
			t.Errorf("parseEnvBool(%q) = (%v,%v), want (false,true)", v, got, ok)
		}
	}
	// Unrecognised values report ok=false so callers leave the config
	// value unchanged instead of coercing to false.
	for _, v := range []string{"maybe", "2", "enabled", "", "tru"} {
		if _, ok := parseEnvBool(v); ok {
			t.Errorf("parseEnvBool(%q) reported recognised; want ok=false", v)
		}
	}
}

// TestApplyEnvOverrides_ForceSSL_YesEnables pins the exact footgun:
// FORCE_SSL=yes must enable SSL, and an unrecognised value must not
// silently flip it off (the config value is left untouched).
func TestApplyEnvOverrides_ForceSSL_YesEnables(t *testing.T) {
	t.Run("yes enables", func(t *testing.T) {
		t.Setenv("VORNIK_STORAGE_S3_FORCE_SSL", "yes")
		cfg := DefaultConfig()
		applyEnvOverrides(cfg)
		if cfg.Storage.S3.ForceSSL == nil || !*cfg.Storage.S3.ForceSSL {
			t.Fatalf("FORCE_SSL=yes should enable SSL, got %v", cfg.Storage.S3.ForceSSL)
		}
	})

	t.Run("garbage leaves default untouched", func(t *testing.T) {
		t.Setenv("VORNIK_STORAGE_S3_FORCE_SSL", "garbage")
		cfg := DefaultConfig()
		before := cfg.Storage.S3.ForceSSL
		applyEnvOverrides(cfg)
		if cfg.Storage.S3.ForceSSL != before {
			t.Fatalf("unrecognised FORCE_SSL should leave config unchanged: before=%v after=%v", before, cfg.Storage.S3.ForceSSL)
		}
	})

	t.Run("metrics yes enables", func(t *testing.T) {
		t.Setenv("VORNIK_METRICS_ENABLED", "on")
		cfg := DefaultConfig()
		applyEnvOverrides(cfg)
		if !cfg.Metrics.Enabled {
			t.Fatal("VORNIK_METRICS_ENABLED=on should enable metrics")
		}
	})
}
