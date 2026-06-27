package config

import "testing"

// TestIsAdminKey_ConstantTimeCorrectness guards the constant-time rewrite
// of IsAdminKey (the timing-unsafe `k == key` was replaced by SHA-256 +
// subtle.ConstantTimeCompare scanning every entry without an early return).
// The behavioural contract must be preserved: a key matching ANY allowed
// entry — including a non-first one, which the no-early-return scan must
// still reach — is accepted; near-misses and the disabled/empty guards
// reject. (Audit 2026-06-03: timing-side-channel admin-key recovery.)
func TestIsAdminKey_ConstantTimeCorrectness(t *testing.T) {
	cfg := AdminConfig{Enabled: true, AllowedKeys: []string{"first-key", "second-key", "third-key"}}

	// Every allowed entry matches, not just the first — proves the scan
	// does not early-return on the first comparison.
	for _, k := range []string{"first-key", "second-key", "third-key"} {
		if !cfg.IsAdminKey(k) {
			t.Errorf("IsAdminKey(%q) = false, want true", k)
		}
	}

	// Near-misses (length off, trailing space, case, prefix) reject.
	for _, k := range []string{"", "first-ke", "first-key ", "FIRST-KEY", "second", "unknown"} {
		if cfg.IsAdminKey(k) {
			t.Errorf("IsAdminKey(%q) = true, want false", k)
		}
	}

	// Disabled config never matches, even a correct key.
	if (AdminConfig{Enabled: false, AllowedKeys: []string{"first-key"}}).IsAdminKey("first-key") {
		t.Error("IsAdminKey on a disabled AdminConfig must return false")
	}
	// No allowed keys configured → nothing matches.
	if (AdminConfig{Enabled: true}).IsAdminKey("anything") {
		t.Error("IsAdminKey with no AllowedKeys must return false")
	}
}
