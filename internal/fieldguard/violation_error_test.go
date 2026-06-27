package fieldguard

import "testing"

// Covers the nil/empty branch of Violation.Error (the populated branch is
// exercised elsewhere).
func TestViolation_Error_NilAndEmpty(t *testing.T) {
	const want = "fieldguard: no violation"
	var nilV *Violation
	if got := nilV.Error(); got != want {
		t.Errorf("nil Violation.Error() = %q, want %q", got, want)
	}
	if got := (&Violation{}).Error(); got != want {
		t.Errorf("empty Violation.Error() = %q, want %q", got, want)
	}
	// Sanity: a populated violation still names the fields.
	if got := (&Violation{Fields: []string{"status", "epoch"}}).Error(); got == want {
		t.Errorf("populated Violation.Error() should list fields, got %q", got)
	}
}
