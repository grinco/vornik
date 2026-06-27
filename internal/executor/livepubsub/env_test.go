package livepubsub

import "testing"

// TestEnvInt_UnsetReturnsDefault pins the most common path —
// env var blank → caller's default.
func TestEnvInt_UnsetReturnsDefault(t *testing.T) {
	t.Setenv("VORNIK_LIVE_RING_SIZE_TEST", "")
	if got := envInt("VORNIK_LIVE_RING_SIZE_TEST", 200); got != 200 {
		t.Errorf("envInt blank = %d, want 200", got)
	}
}

// TestEnvInt_SetReturnsParsedValue confirms a valid integer
// from the env overrides the default.
func TestEnvInt_SetReturnsParsedValue(t *testing.T) {
	t.Setenv("VORNIK_LIVE_RING_SIZE_TEST", "42")
	if got := envInt("VORNIK_LIVE_RING_SIZE_TEST", 200); got != 42 {
		t.Errorf("envInt 42 = %d, want 42", got)
	}
}

// TestEnvInt_GarbageFallsBackToDefault: an unparseable value
// (e.g. "abc", "  ", a typo) MUST NOT crash the daemon nor
// silently zero the ring size — it falls back to the default.
func TestEnvInt_GarbageFallsBackToDefault(t *testing.T) {
	t.Setenv("VORNIK_LIVE_RING_SIZE_TEST", "not-an-int")
	if got := envInt("VORNIK_LIVE_RING_SIZE_TEST", 200); got != 200 {
		t.Errorf("envInt garbage = %d, want 200 fallback", got)
	}
}

// TestEnvInt_ZeroFallsBackToDefault — an explicit "0" is
// treated as "use the default". Otherwise a ring of size 0
// would drop every event silently.
func TestEnvInt_ZeroFallsBackToDefault(t *testing.T) {
	t.Setenv("VORNIK_LIVE_RING_SIZE_TEST", "0")
	if got := envInt("VORNIK_LIVE_RING_SIZE_TEST", 200); got != 200 {
		t.Errorf("envInt zero = %d, want 200 fallback", got)
	}
}

// TestEnvInt_NegativeFallsBackToDefault: same defensive
// shape — a negative value would underflow.
func TestEnvInt_NegativeFallsBackToDefault(t *testing.T) {
	t.Setenv("VORNIK_LIVE_RING_SIZE_TEST", "-5")
	if got := envInt("VORNIK_LIVE_RING_SIZE_TEST", 200); got != 200 {
		t.Errorf("envInt negative = %d, want 200 fallback", got)
	}
}
