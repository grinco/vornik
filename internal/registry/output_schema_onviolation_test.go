package registry

import "testing"

// D6.2b: an enum node declares what a violation MEANS. A closed correctness
// contract fails the step; an advisory hint warns and lets the existing
// downstream fallback handle the value. Before this, `Enum` was parsed and
// unused, so every enum meant the same thing: nothing.

func TestOnViolationDefaultsToFail(t *testing.T) {
	// Test 54b — the conservative default is the whole reason `warn` is safe
	// to offer. An enum declared without the field must bind.
	s := &OutputSchema{Type: "string", Enum: []any{"a", "b"}}
	if got := s.ViolationMode(); got != ViolationFail {
		t.Fatalf("an enum with no onViolation resolved to %q, want %q — "+
			"a silent default of warn would make every declared enum advisory", got, ViolationFail)
	}
}

func TestOnViolationWarnIsHonoured(t *testing.T) {
	s := &OutputSchema{Type: "string", Enum: []any{"a"}, OnViolation: "warn"}
	if got := s.ViolationMode(); got != ViolationWarn {
		t.Fatalf("onViolation: warn resolved to %q, want %q", got, ViolationWarn)
	}
}

func TestOnViolationUnknownValueIsRejectedAtLoad(t *testing.T) {
	// A typo must not resolve to the permissive mode. It is a config error:
	// "enforce" is not "warn", and guessing which the operator meant is how a
	// control ends up reporting "examined and clean" while meaning nothing.
	s := &OutputSchema{Type: "string", Enum: []any{"a"}, OnViolation: "enforce"}
	if err := s.ValidateOnViolation(); err == nil {
		t.Fatal("onViolation: enforce was accepted; an unrecognised mode must be a load-time error")
	}
}

func TestOnViolationOnlyMeaningfulWithAnEnum(t *testing.T) {
	// Declaring a violation mode on a node with no enum is a dead statement,
	// and a dead statement in a schema is one the next reader trusts.
	s := &OutputSchema{Type: "string", OnViolation: "warn"}
	if err := s.ValidateOnViolation(); err == nil {
		t.Fatal("onViolation on an enum-less node was accepted; it constrains nothing there")
	}
}

func TestCloneCopiesOnViolation(t *testing.T) {
	// pinCaseIDEnum clones the role's schema per execution. A Clone that drops
	// OnViolation would silently promote a warn node to fail on the pinned path.
	src := &OutputSchema{
		Type: "object",
		Properties: map[string]*OutputSchema{
			"tier": {Type: "string", Enum: []any{"a"}, OnViolation: "warn"},
		},
	}
	got := src.Clone().Properties["tier"]
	if got.OnViolation != "warn" {
		t.Fatalf("Clone dropped OnViolation: got %q, want %q", got.OnViolation, "warn")
	}
	got.OnViolation = "fail"
	if src.Properties["tier"].OnViolation != "warn" {
		t.Fatal("Clone shares the node with the source: mutating the clone changed the role's declaration")
	}
}
