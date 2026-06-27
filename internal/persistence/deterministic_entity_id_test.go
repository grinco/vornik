package persistence

import (
	"strings"
	"testing"
)

// TestDeterministicEntityID pins the KG entity-ID scheme: a stable hash of
// the identity triple so extraction is idempotent (same entity → same ID).
func TestDeterministicEntityID(t *testing.T) {
	a := DeterministicEntityID("proj-1", "person", "Ada Lovelace")

	// Deterministic: same triple → same ID.
	if b := DeterministicEntityID("proj-1", "person", "Ada Lovelace"); a != b {
		t.Errorf("same triple produced different IDs: %q vs %q", a, b)
	}
	// Prefixed + non-empty.
	if !strings.HasPrefix(a, "kent_") {
		t.Errorf("ID %q missing kent_ prefix", a)
	}

	// Each component matters — changing any one changes the ID.
	for _, other := range []string{
		DeterministicEntityID("proj-2", "person", "Ada Lovelace"), // project
		DeterministicEntityID("proj-1", "org", "Ada Lovelace"),    // type
		DeterministicEntityID("proj-1", "person", "Ada L."),       // name
	} {
		if other == a {
			t.Errorf("distinct triple collided with %q", a)
		}
	}

	// Separator-safety: ("a","bc","d") must not collide with ("ab","c","d").
	if DeterministicEntityID("a", "bc", "d") == DeterministicEntityID("ab", "c", "d") {
		t.Error("triple boundary collision — NUL separator not preventing concat ambiguity")
	}
}
