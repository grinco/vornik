package registry

import (
	"strings"
	"testing"
)

func roleWithModalities(model, fallback string, required []string) *Swarm {
	return &Swarm{
		ID:       "s",
		LeadRole: "vision",
		Roles: []SwarmRole{{
			Name:               "vision",
			Model:              model,
			ModelFallback:      fallback,
			RequiredModalities: required,
			Runtime:            SwarmRoleRuntime{Image: "localhost/vornik-agent:latest"},
		}},
	}
}

// A role that declares it needs sight must not be wired to a blind model.
// Without this, a handover to the vision role reproduces the dispatcher's
// own bug one layer down: the task runs, the agent is told an image is
// attached, and the model never receives pixels.
//
// see LLD § https://docs.vornik.io §4.5
func TestSwarmValidate_RejectsBlindModelForVisionRole(t *testing.T) {
	err := roleWithModalities("glm-5.2", "", []string{"vision"}).Validate("test.md")
	if err == nil {
		t.Fatal("a role requiring vision on a text-only model must fail validation")
	}
	if !strings.Contains(err.Error(), "vision") {
		t.Errorf("error should name the missing modality: %v", err)
	}
}

// The fallback is checked too. This is the case that motivated checking
// both: the vision role's chain crosses providers
// (google.gemma-3-27b-it → gemma4:31b), so a model-only check would pass
// while a fallback-time degradation went undetected until an incident.
func TestSwarmValidate_RejectsBlindFallbackForVisionRole(t *testing.T) {
	err := roleWithModalities("google.gemma-3-27b-it", "glm-5.2", []string{"vision"}).Validate("test.md")
	if err == nil {
		t.Fatal("a sighted primary with a blind modelFallback must fail validation")
	}
	if !strings.Contains(err.Error(), "modelFallback") {
		t.Errorf("error should name modelFallback so the operator knows which of the two is wrong: %v", err)
	}
}

// The deployment's own configured pair must pass, or this validation would
// refuse to load the swarm file that ships today.
func TestSwarmValidate_AcceptsConfiguredVisionPair(t *testing.T) {
	if err := roleWithModalities("google.gemma-3-27b-it", "gemma4:31b", []string{"vision"}).Validate("test.md"); err != nil {
		t.Fatalf("the shipped vision role pair must validate: %v", err)
	}
}

// Declaration-driven, not name-driven: a role called "vision" gets no
// special treatment, so an undeclared role is unaffected by this check.
func TestSwarmValidate_UndeclaredRoleUnaffected(t *testing.T) {
	if err := roleWithModalities("glm-5.2", "zai.glm-5", nil).Validate("test.md"); err != nil {
		t.Fatalf("a role with no requiredModalities must not be capability-checked: %v", err)
	}
}

// An empty Model means "inherit the daemon default", which this check
// cannot resolve — the swarm file does not know it. Refusing would make
// the declaration unusable on inheriting roles, so it is skipped and the
// operator keeps the runtime behaviour they have today.
func TestSwarmValidate_InheritedModelSkipsCheck(t *testing.T) {
	if err := roleWithModalities("", "", []string{"vision"}).Validate("test.md"); err != nil {
		t.Fatalf("a role inheriting the daemon model must not fail this check: %v", err)
	}
}

func TestSwarmValidate_RejectsUnknownModality(t *testing.T) {
	err := roleWithModalities("google.gemma-3-27b-it", "", []string{"telepathy"}).Validate("test.md")
	if err == nil {
		t.Fatal("an unknown modality name must fail validation")
	}
	if !strings.Contains(err.Error(), "requiredModalities") {
		t.Errorf("error should name the field: %v", err)
	}
}
