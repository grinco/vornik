package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadFromPath_ParsesValidatesAndIgnoresFlags — the runtime-safe loader
// used by the config hot-reload path. It must parse + validate a config.yaml
// without touching flags/global state, and surface parse/validate errors.
func TestLoadFromPath_ParsesValidatesAndIgnoresFlags(t *testing.T) {
	t.Run("parses memory hot-reloadable keys", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		// auth_enabled:false keeps the config minimally valid without needing
		// api_keys (the default auth-on posture requires them).
		const body = `
api:
  auth_enabled: false
memory:
  prompt_injection_scan: quarantine
  claim_audit_disabled_projects:
    - proj-a
    - proj-b
`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		cfg, err := LoadFromPath(path)
		if err != nil {
			t.Fatalf("LoadFromPath error: %v", err)
		}
		if cfg.Memory.PromptInjectionScan != "quarantine" {
			t.Errorf("PromptInjectionScan = %q, want quarantine", cfg.Memory.PromptInjectionScan)
		}
		if got := cfg.Memory.ClaimAuditDisabledProjects; len(got) != 2 || got[0] != "proj-a" || got[1] != "proj-b" {
			t.Errorf("ClaimAuditDisabledProjects = %v, want [proj-a proj-b]", got)
		}
	})

	// Backlog (batch-2 RAG/memory follow-up): deny_patterns is wired from YAML
	// into the ingest gate. The loader must parse the list so the hot-reload
	// activator can hand it to Pipeline.UpdateGates.
	t.Run("parses memory deny_patterns", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		const body = `
api:
  auth_enabled: false
memory:
  deny_patterns:
    - SECRET-MARKER
    - do-not-store
`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		cfg, err := LoadFromPath(path)
		if err != nil {
			t.Fatalf("LoadFromPath error: %v", err)
		}
		got := cfg.Memory.DenyPatterns
		if len(got) != 2 || got[0] != "SECRET-MARKER" || got[1] != "do-not-store" {
			t.Errorf("DenyPatterns = %v, want [SECRET-MARKER do-not-store]", got)
		}
	})

	t.Run("unparseable YAML is an error", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, []byte("memory: : : not yaml"), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		if _, err := LoadFromPath(path); err == nil {
			t.Error("expected a parse error for malformed YAML, got nil")
		}
	})

	t.Run("invalid prompt_injection_scan fails validation", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, []byte("memory:\n  prompt_injection_scan: bogus\n"), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		if _, err := LoadFromPath(path); err == nil {
			t.Error("expected validation error for an invalid prompt_injection_scan, got nil")
		}
	})

	t.Run("missing file is an error", func(t *testing.T) {
		if _, err := LoadFromPath(filepath.Join(t.TempDir(), "does-not-exist.yaml")); err == nil {
			t.Error("expected a read error for a missing file, got nil")
		}
	})
}

// TestValidateBytes — the in-memory pre-write validator used by the gen-config
// bootstrap. It must accept a valid config and reject one that fails Validate
// (e.g. auth_enabled:true with no api_keys) without reading any file.
func TestValidateBytes(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		good := []byte("api:\n  auth_enabled: true\n  api_keys:\n    - sk-vornik-abc.def\n")
		if err := ValidateBytes(good); err != nil {
			t.Fatalf("ValidateBytes rejected a valid config: %v", err)
		}
	})

	t.Run("auth_enabled without api_keys fails", func(t *testing.T) {
		bad := []byte("api:\n  auth_enabled: true\n  api_keys: []\n")
		if err := ValidateBytes(bad); err == nil {
			t.Fatal("expected validation error for auth_enabled with empty api_keys, got nil")
		}
	})

	t.Run("unparseable yaml fails", func(t *testing.T) {
		if err := ValidateBytes([]byte("api: [unterminated\n")); err == nil {
			t.Fatal("expected a parse error for malformed YAML, got nil")
		}
	})
}
