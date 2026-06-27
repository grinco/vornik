package dispatcher

import (
	"strings"
	"testing"
)

func TestOperatorLinkOTPStore_RoundTrip(t *testing.T) {
	s := NewOperatorLinkOTPStore()
	code := s.Issue("web:alice")
	if !strings.Contains(code, "-") {
		t.Errorf("issued code should carry the cosmetic dash: %q", code)
	}
	issuer, ok, _ := s.Claim("web:bob", code)
	if !ok {
		t.Fatalf("claim should succeed for freshly issued code")
	}
	if issuer != "web:alice" {
		t.Errorf("issuer: %q", issuer)
	}
}

func TestOperatorLinkOTPStore_ClaimIsSingleUse(t *testing.T) {
	s := NewOperatorLinkOTPStore()
	code := s.Issue("web:alice")
	if _, ok, _ := s.Claim("web:bob", code); !ok {
		t.Fatalf("first claim failed")
	}
	if _, ok, _ := s.Claim("web:bob", code); ok {
		t.Errorf("second claim must fail (single-use)")
	}
}

func TestOperatorLinkOTPStore_DashTolerant(t *testing.T) {
	s := NewOperatorLinkOTPStore()
	code := s.Issue("web:alice")
	// Strip the dash + try lowercase.
	stripped := strings.ToLower(strings.ReplaceAll(code, "-", ""))
	if _, ok, _ := s.Claim("web:bob", stripped); !ok {
		t.Errorf("claim should tolerate case + missing dash: %q", stripped)
	}
}

func TestOperatorLinkOTPStore_UnknownCodeRejected(t *testing.T) {
	s := NewOperatorLinkOTPStore()
	if _, ok, _ := s.Claim("web:bob", "FAKE-CODE"); ok {
		t.Errorf("unknown code should not claim")
	}
}

func TestOperatorLinkOTPStore_MalformedCodeRejected(t *testing.T) {
	s := NewOperatorLinkOTPStore()
	cases := []string{"", "1234", "1234-567", "0123456789ABCDEF"} // too short / too long
	for _, c := range cases {
		if _, ok, _ := s.Claim("web:bob", c); ok {
			t.Errorf("malformed code %q should reject", c)
		}
	}
}

func TestOperatorLinkOTPStore_PendingReflectsState(t *testing.T) {
	s := NewOperatorLinkOTPStore()
	if s.pending() != 0 {
		t.Errorf("empty store should have 0 pending")
	}
	_ = s.Issue("a")
	_ = s.Issue("b")
	if s.pending() != 2 {
		t.Errorf("expected 2 pending, got %d", s.pending())
	}
}

func TestDefaultOperatorLinkOTPStore_Singleton(t *testing.T) {
	a := DefaultOperatorLinkOTPStore()
	b := DefaultOperatorLinkOTPStore()
	if a != b {
		t.Errorf("DefaultOperatorLinkOTPStore should return the same instance")
	}
}

// TestOperatorLinkOTPStore_BruteForceLockout is the security regression
// for online brute force of the 32-bit code space: after
// operatorLinkMaxFailures well-formed wrong guesses, the claimant is
// locked out for the window — even a subsequently CORRECT code is
// refused. A different claimant is unaffected (lockout is per-speaker).
func TestOperatorLinkOTPStore_BruteForceLockout(t *testing.T) {
	s := NewOperatorLinkOTPStore()
	realCode := s.Issue("web:alice")

	// Attacker burns the failure budget with well-formed wrong codes.
	var lastLocked bool
	for i := 0; i < operatorLinkMaxFailures; i++ {
		_, ok, locked := s.Claim("tg:mallory", "DEADBEEF")
		if ok {
			t.Fatalf("wrong code should never claim (iteration %d)", i)
		}
		lastLocked = locked
	}
	if !lastLocked {
		t.Fatalf("claimant should be locked after %d failures", operatorLinkMaxFailures)
	}

	// Even the CORRECT code is now refused for the locked claimant.
	if _, ok, locked := s.Claim("tg:mallory", realCode); ok || !locked {
		t.Errorf("locked claimant must not claim even a correct code; ok=%v locked=%v", ok, locked)
	}

	// A different claimant is NOT locked and can still claim the code.
	issuer, ok, locked := s.Claim("tg:victim", realCode)
	if !ok || locked {
		t.Errorf("unrelated claimant must be unaffected by another's lockout; ok=%v locked=%v", ok, locked)
	}
	if issuer != "web:alice" {
		t.Errorf("issuer = %q, want web:alice", issuer)
	}
}

// TestOperatorLinkOTPStore_SuccessResetsFailures ensures a successful
// claim clears the failure tally so a legitimate operator who fat-
// fingered a few times isn't penalised afterward.
func TestOperatorLinkOTPStore_SuccessResetsFailures(t *testing.T) {
	s := NewOperatorLinkOTPStore()
	for i := 0; i < operatorLinkMaxFailures-1; i++ {
		if _, _, locked := s.Claim("tg:bob", "DEADBEEF"); locked {
			t.Fatalf("should not lock below threshold (iteration %d)", i)
		}
	}
	code := s.Issue("web:bob")
	if _, ok, _ := s.Claim("tg:bob", code); !ok {
		t.Fatal("correct code should claim while under the failure threshold")
	}
	// A fresh run must again take the FULL budget before locking.
	for i := 0; i < operatorLinkMaxFailures-1; i++ {
		if _, _, locked := s.Claim("tg:bob", "DEADBEEF"); locked {
			t.Fatalf("tally was not reset after success (locked at iteration %d)", i)
		}
	}
}

// TestOperatorLinkOTPStore_MalformedDoesNotCountAsFailure ensures
// malformed codes (operator typos) don't burn the brute-force budget.
func TestOperatorLinkOTPStore_MalformedDoesNotCountAsFailure(t *testing.T) {
	s := NewOperatorLinkOTPStore()
	for i := 0; i < operatorLinkMaxFailures*3; i++ {
		if _, _, locked := s.Claim("tg:typo", "1234"); locked {
			t.Fatalf("malformed code must not trip the lockout (iteration %d)", i)
		}
	}
}

func TestNormaliseOperatorLinkCode(t *testing.T) {
	cases := map[string]string{
		"ABCD-1234":     "ABCD1234",
		"abcd-1234":     "ABCD1234",
		"  ABCD-1234  ": "ABCD1234",
		"abcd 1234":     "ABCD1234",
		"abcd":          "", // too short
		"abcd-1234-x":   "", // too long
		"":              "", // empty
	}
	for in, want := range cases {
		if got := normaliseOperatorLinkCode(in); got != want {
			t.Errorf("normaliseOperatorLinkCode(%q) = %q, want %q", in, got, want)
		}
	}
}
