package registry

import (
	"strings"
	"testing"
)

func TestGuardYAMLComplexity_RejectsAliasBomb(t *testing.T) {
	// 60 aliases > the 50 cap → rejected.
	var b strings.Builder
	b.WriteString("anchor: &x \"v\"\nlist:\n")
	for i := 0; i < 60; i++ {
		b.WriteString("  - *x\n")
	}
	if err := guardYAMLComplexity([]byte(b.String()), "bomb.md"); err == nil {
		t.Fatal("alias-heavy frontmatter must be rejected")
	}
}

func TestGuardYAMLComplexity_AllowsNormal(t *testing.T) {
	fm := "name: research\ndescription: find facts\nversion: 1.0.0\n"
	if err := guardYAMLComplexity([]byte(fm), "ok.md"); err != nil {
		t.Fatalf("normal frontmatter rejected: %v", err)
	}
}

func TestParseSwarmSkill_RejectsOversized(t *testing.T) {
	huge := make([]byte, MaxSwarmSkillBytes+1)
	for i := range huge {
		huge[i] = 'a'
	}
	if _, err := ParseSwarmSkill(huge, "huge.md"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized input must be rejected, got %v", err)
	}
}

func TestValidateSwarmSkillMarkdown_RejectsOversized(t *testing.T) {
	huge := make([]byte, MaxSwarmSkillBytes+1)
	rep := ValidateSwarmSkillMarkdown(huge, "huge.md")
	if !rep.HasErrors() {
		t.Fatal("oversized input must produce a validation error")
	}
}
