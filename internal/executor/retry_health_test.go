package executor

import (
	"errors"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/registry"
)

// isModelUnhealthyFailure recognizes both the agent-emitted string marker and
// the typed chat.ModelUnhealthyError (LLD 2026-07-11-model-health §6).
func TestIsModelUnhealthyFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"typed", &chat.ModelUnhealthyError{Route: "bedrock", Model: "m", State: "open"}, true},
		{"wrapped typed", errors.New("agent reported FAILED status: " +
			(&chat.ModelUnhealthyError{Model: "m"}).Error()), true},
		{"string marker", errors.New("agent reported FAILED: MODEL_UNHEALTHY: model x circuit open"), true},
		{"plain provider error", errors.New("PROVIDER_ERROR: upstream provider returned an error"), false},
		{"unrelated", errors.New("schema violation"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isModelUnhealthyFailure(c.err); got != c.want {
				t.Errorf("isModelUnhealthyFailure(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// A MODEL_UNHEALTHY error is NOT infra-classified, so the infra ladder returns
// it immediately (no retry) — the circuit is open, retrying just fast-rejects.
func TestModelUnhealthy_NotInfraClassified(t *testing.T) {
	err := errors.New("MODEL_UNHEALTHY: model zai.glm-5 on route bedrock circuit open")
	if isInfraFailure(err) {
		t.Fatal("MODEL_UNHEALTHY must NOT be infra-classified (would trigger the retry ladder)")
	}
	// But it IS a model-shaped / fail-over trigger.
	if !isModelShapedFailure(err) {
		t.Fatal("MODEL_UNHEALTHY must trigger model fallback")
	}
}

// The same-model-fallback drop: a role whose fallback equals its primary is
// treated as having no fallback (would re-reject on the same circuit).
func TestFallbackModelOverrides_DropsSameModel(t *testing.T) {
	sw := &registry.Swarm{Roles: []registry.SwarmRole{
		{Name: "researcher", Model: "zai.glm-5", ModelFallback: "zai.glm-5"}, // same → dropped
		{Name: "coder", Model: "zai.glm-5", ModelFallback: "minimax.m2.5"},   // genuine → kept
		{Name: "gate", Model: "m", ModelFallback: ""},                        // none
	}}
	got := FallbackModelOverrides(sw)
	if _, ok := got["researcher"]; ok {
		t.Fatal("a fallback equal to the primary must be dropped")
	}
	if got["coder"] != "minimax.m2.5" {
		t.Fatalf("genuine fallback must be kept, got %q", got["coder"])
	}
	if _, ok := got["gate"]; ok {
		t.Fatal("no fallback must stay absent")
	}
}
