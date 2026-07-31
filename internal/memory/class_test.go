package memory

import (
	"testing"
	"time"
)

func TestClassifyByRole_KnownRoles(t *testing.T) {
	cases := map[string]ContentClass{
		"researcher":  ClassResearch,
		"scout":       ClassResearch,
		"writer":      ClassResearch,
		"analyst":     ClassSpec,
		"coder":       ClassCommitMsg,
		"developer":   ClassCommitMsg,
		"engineer":    ClassCommitMsg,
		"implementer": ClassCommitMsg,
		"reviewer":    ClassDecision,
		"architect":   ClassDecision,
		"tester":      ClassDiagnostic,
		"qa":          ClassDiagnostic,
		"verifier":    ClassDiagnostic,
		// Workflow / multi-modal / trading roles added after the
		// assistant project's reclassify run reported 224 chunks
		// stuck on unknown roles.
		"lead":         ClassSpec,
		"feasibility":  ClassDecision,
		"vision":       ClassExternalFetch,
		"strategist":   ClassSpec,
		"risk-officer": ClassDecision,
		"executor":     ClassCommitMsg,
	}
	for role, want := range cases {
		got, pol := ClassifyByRole(role)
		if got != want {
			t.Errorf("ClassifyByRole(%q) class=%q, want %q", role, got, want)
		}
		wantPol := DefaultClassPolicies[want]
		if pol != wantPol {
			t.Errorf("ClassifyByRole(%q) policy=%+v, want %+v", role, pol, wantPol)
		}
	}
}

func TestClassifyByRole_UnknownFallsBackToUnclassified(t *testing.T) {
	got, pol := ClassifyByRole("totally-not-a-role")
	if got != ClassUnclassified {
		t.Fatalf("class=%q want %q", got, ClassUnclassified)
	}
	if pol != DefaultClassPolicies[ClassUnclassified] {
		t.Fatalf("policy mismatch: %+v", pol)
	}
}

func TestIsValidClass(t *testing.T) {
	builtins := []ContentClass{
		ClassResearch, ClassSpec, ClassDecision, ClassCommitMsg,
		ClassDiagnostic, ClassExternalFetch, ClassSummary, ClassUnclassified,
		ClassCompanionNote,
	}
	for _, c := range builtins {
		if !IsValidClass(string(c)) {
			t.Errorf("IsValidClass(%q) = false, want true", c)
		}
	}
	for _, bad := range []string{"", "made-up", "RESEARCH"} {
		if IsValidClass(bad) {
			t.Errorf("IsValidClass(%q) = true, want false", bad)
		}
	}
}

// TestCompanionNoteClassPolicy — LLD 22 sets a deliberate confidence
// floor (0.3) and 30-day TTL distinct from the existing classes.
// The values matter: bumping default confidence would let companion
// content masquerade as agent-validated output; dropping the TTL to 0
// would let stale chat notes accumulate forever. Pin them.
// TestChatMemoryClassRegistered — chat memory-write slice 5, §3.
// ClassChatMemory must be a valid class (else ClassKnownGate silently
// downgrades a chat deposit to unclassified, gates.go), carry the
// operator-signed-off policy (90d TTL, 0.3 confidence, never
// role-of-record), and NOT be role-of-record.
func TestChatMemoryClassRegistered(t *testing.T) {
	if !IsValidClass(string(ClassChatMemory)) {
		t.Fatalf("ClassChatMemory %q is not a valid class — ClassKnownGate would downgrade it to unclassified", ClassChatMemory)
	}
	if string(ClassChatMemory) != "chat_memory" {
		t.Errorf("ClassChatMemory = %q, want chat_memory", ClassChatMemory)
	}
	pol, ok := DefaultClassPolicies[ClassChatMemory]
	if !ok {
		t.Fatal("ClassChatMemory missing from DefaultClassPolicies")
	}
	if pol.DefaultConfidence != 0.3 {
		t.Errorf("DefaultConfidence=%v, want 0.3", pol.DefaultConfidence)
	}
	if pol.TTL != 90*24*time.Hour {
		t.Errorf("TTL=%v, want 90 days", pol.TTL)
	}
	if pol.RoleOfRecordEligible {
		t.Error("chat memory must not be role-of-record")
	}
}

func TestCompanionNoteClassPolicy(t *testing.T) {
	pol, ok := DefaultClassPolicies[ClassCompanionNote]
	if !ok {
		t.Fatal("ClassCompanionNote missing from DefaultClassPolicies")
	}
	if pol.DefaultConfidence != 0.3 {
		t.Errorf("DefaultConfidence=%v, want 0.3", pol.DefaultConfidence)
	}
	if pol.TTL == 0 || pol.TTL > 31*24*time.Hour {
		t.Errorf("TTL=%v, want ~30 days", pol.TTL)
	}
	if pol.RoleOfRecordEligible {
		t.Error("companion notes must not be role-of-record")
	}
}
