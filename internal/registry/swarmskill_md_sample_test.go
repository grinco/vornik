package registry

import (
	"os"
	"testing"
)

// TestMarshalSwarmSkill_SnapshotForReview emits the sample skill
// to stderr when VORNIK_PRINT_SAMPLE=1. Useful when reviewing the
// on-disk shape during development; no-op in normal CI runs so
// log spam stays under control.
func TestMarshalSwarmSkill_SnapshotForReview(t *testing.T) {
	if os.Getenv("VORNIK_PRINT_SAMPLE") == "" {
		t.Skip("set VORNIK_PRINT_SAMPLE=1 to print the sample bundle")
	}
	skill := makeTestSkill()
	out, err := MarshalSwarmSkill(skill, MarshalSwarmSkillOpts{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	t.Logf("\n%s", out)
	t.Logf("--- standard mode ---")
	stdOut, err := MarshalSwarmSkill(skill, MarshalSwarmSkillOpts{Standard: true})
	if err != nil {
		t.Fatalf("marshal standard: %v", err)
	}
	t.Logf("\n%s", stdOut)
}
