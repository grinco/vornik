package swarmenv

import (
	"strings"
	"testing"
)

// A faithful fixture: two roles, each with runtime.envVars (matching the real
// SWARM.md shape — comments under envVars, quoted values, 12-space key indent).
const fixture = `swarmId: "test-swarm"
roles:
    - name: "researcher"
      runtime:
        image: "vornik-agent:latest"
        envVars:
            # keep this comment
            VORNIK_MAX_TOOL_ITERATIONS: "25"
      permissions:
        allowedTools: ["file_read"]
    - name: "writer"
      runtime:
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "50"
`

// Updating an existing key rewrites only that role's value, preserving comments
// and leaving sibling roles untouched — the core correctness the whole Phase-2
// apply path depends on (a bug here corrupts a live swarm config).
func TestSetRoleEnvUpdatesExistingKeyScopedToRole(t *testing.T) {
	out, err := SetRoleEnv(fixture, "researcher", "VORNIK_MAX_TOOL_ITERATIONS", "40")
	if err != nil {
		t.Fatalf("SetRoleEnv: %v", err)
	}
	if !strings.Contains(out, `            VORNIK_MAX_TOOL_ITERATIONS: "40"`) {
		t.Errorf("researcher value not updated to 40:\n%s", out)
	}
	if !strings.Contains(out, "# keep this comment") {
		t.Errorf("comment not preserved")
	}
	// writer's identical key must be UNTOUCHED (still 50)
	if strings.Count(out, `VORNIK_MAX_TOOL_ITERATIONS: "50"`) != 1 {
		t.Errorf("writer's key was affected:\n%s", out)
	}
	if strings.Count(out, `VORNIK_MAX_TOOL_ITERATIONS: "40"`) != 1 {
		t.Errorf("expected exactly one updated key, got:\n%s", out)
	}
}

// Setting an absent key inserts it into that role's envVars (at the existing key
// indent), preserving the existing key + comment.
func TestSetRoleEnvInsertsAbsentKey(t *testing.T) {
	out, err := SetRoleEnv(fixture, "researcher", "VORNIK_STEP_PROMPT_TOKEN_BUDGET", "900000")
	if err != nil {
		t.Fatalf("SetRoleEnv: %v", err)
	}
	if !strings.Contains(out, `            VORNIK_STEP_PROMPT_TOKEN_BUDGET: "900000"`) {
		t.Errorf("new key not inserted at 12-space indent:\n%s", out)
	}
	if !strings.Contains(out, `            VORNIK_MAX_TOOL_ITERATIONS: "25"`) {
		t.Errorf("existing key clobbered")
	}
	// inserted under researcher, not writer: budget key appears once, before writer
	if i, j := strings.Index(out, "VORNIK_STEP_PROMPT_TOKEN_BUDGET"), strings.Index(out, `- name: "writer"`); i < 0 || i > j {
		t.Errorf("new key not placed under researcher (before writer)")
	}
}

func TestSetRoleEnvErrorsOnUnknownRole(t *testing.T) {
	if _, err := SetRoleEnv(fixture, "ghost", "K", "v"); err == nil {
		t.Errorf("expected error for unknown role")
	}
}
