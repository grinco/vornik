package registry

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/mediakind"
)

// Regression, measured 2026-08-19. validateRoleModalities called
// mediakind.Capabilities(model, nil) — the second argument is the operator's
// declared-capability map, hardcoded nil. So swarm validation NEVER read
// chat.model_capabilities, while its own refusal told the operator to
// "declare the model's modalities in chat.model_capabilities": advice that
// could not work.
//
// The cost was not theoretical. The benchmark host serves one local vLLM and
// disables every outward-facing provider, so its vision role has to point at
// the local model. Declaring the model vision-capable in config had no effect,
// the swarm failed to load, and the daemon fell back to a synthetic workflow
// for every task.
func TestValidateRoleModalities_HonoursDeclaredCapabilities(t *testing.T) {
	role := SwarmRole{
		Name:               "vision",
		Model:              "Qwen/Qwen3.8-27B-FP8",
		ModelFallback:      "Qwen/Qwen3.8-27B-FP8",
		RequiredModalities: []string{"vision"},
	}

	// Without a declaration the model is text-only by pattern, so this must
	// still refuse — the guard's real job.
	if err := validateRoleModalities("swarm.md", 0, role, nil); err == nil {
		t.Error("an undeclared text-only model must still be refused for a vision role")
	}

	declared := map[string][]mediakind.Modality{
		"Qwen/Qwen3.8-27B-FP8": {mediakind.ModalityText, mediakind.ModalityVision},
	}
	if err := validateRoleModalities("swarm.md", 0, role, declared); err != nil {
		t.Errorf("a model the operator declared vision-capable must be accepted: %v", err)
	}
}

// An operator may also declare a pattern-matching model text-only — recording
// "this provider's path does not actually accept images". That declaration must
// win, or the escape hatch is one-way.
func TestValidateRoleModalities_DeclarationCanNarrow(t *testing.T) {
	role := SwarmRole{
		Name: "vision", Model: "google.gemma-3-27b-it",
		RequiredModalities: []string{"vision"},
	}
	if err := validateRoleModalities("swarm.md", 0, role, nil); err != nil {
		t.Fatalf("gemma matches a vision pattern, so it should pass undeclared: %v", err)
	}
	declared := map[string][]mediakind.Modality{
		"google.gemma-3-27b-it": {mediakind.ModalityText},
	}
	err := validateRoleModalities("swarm.md", 0, role, declared)
	if err == nil {
		t.Fatal("an explicit text-only declaration must override the id pattern")
	}
	if !strings.Contains(err.Error(), "vision") {
		t.Errorf("refusal should name the missing modality, got %v", err)
	}
}
