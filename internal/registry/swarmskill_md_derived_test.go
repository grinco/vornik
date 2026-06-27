package registry

import (
	"strings"
	"testing"
)

// TestMarshalSwarmSkill_StripsDerivedFieldsForLoaderIdempotency
// pins down the regression that surfaced in the first end-to-end
// smoke: the live daemon's swarm read returns roles with BOTH
// outputSchema AND the derived RequiredOutputKeys /
// PlausibilityRules populated (the loader fills the derived view
// during Validate). A naïve re-marshal then emitted both, and the
// next load refused the file with "must be empty when outputSchema
// is set". The marshal seam now clears the derived view; the
// schema stays the single source of truth on disk.
func TestMarshalSwarmSkill_StripsDerivedFieldsForLoaderIdempotency(t *testing.T) {
	skill := makeTestSkill()
	skill.Roles[0].OutputSchema = &OutputSchema{
		Type: "object",
		Properties: map[string]*OutputSchema{
			"produced_files": {Type: "array"},
		},
		Required: []string{"produced_files"},
	}
	// Pre-populate the derived view, mirroring what the daemon's
	// loader does after parsing the role from YAML.
	skill.Roles[0].RequiredOutputKeys = []string{"produced_files"}
	skill.Roles[0].PlausibilityRules = []PlausibilityRule{
		{Name: "must-have-files", Require: []string{"produced_files"}},
	}

	out, err := MarshalSwarmSkill(skill, MarshalSwarmSkillOpts{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "requiredOutputKeys:") {
		t.Errorf("requiredOutputKeys must be stripped when outputSchema is set:\n%s", s)
	}
	if strings.Contains(s, "plausibilityRules:") {
		t.Errorf("plausibilityRules must be stripped when outputSchema is set:\n%s", s)
	}
	if !strings.Contains(s, "outputSchema:") {
		t.Errorf("outputSchema should remain in output:\n%s", s)
	}
}

// TestMarshalSwarmMarkdown_StripsDerivedFieldsForLoaderIdempotency
// pins the same idempotency property on the SWARM.md serializer
// used during skill import to write the merged target swarm.
func TestMarshalSwarmMarkdown_StripsDerivedFieldsForLoaderIdempotency(t *testing.T) {
	sw := &Swarm{
		ID: "demo-swarm",
		Roles: []SwarmRole{
			{
				Name:               "researcher",
				SystemPrompt:       "You research.",
				OutputSchema:       &OutputSchema{Type: "object", Properties: map[string]*OutputSchema{"ok": {Type: "boolean"}}, Required: []string{"ok"}},
				RequiredOutputKeys: []string{"ok"}, // pre-populated derived view
			},
		},
	}
	out, err := MarshalSwarmMarkdown(sw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "requiredOutputKeys:") {
		t.Errorf("requiredOutputKeys must be stripped:\n%s", out)
	}
}
