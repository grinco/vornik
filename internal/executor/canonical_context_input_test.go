package executor

import (
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// TestBuildAgentInput_CanonicalContextSurfaced confirms the
// canonical-context fields flow into task.json under the
// context shape the agent prompt expects. Pins the contract
// the LLD §3.2 advertises to LLM agents.
func TestBuildAgentInput_CanonicalContextSurfaced(t *testing.T) {
	task := &persistence.Task{ID: "task_x", ProjectID: "proj", Payload: []byte(`{"context":{"prompt":"hello"}}`)}
	opts := &agentInputOpts{
		CanonicalContext: CanonicalContext{
			ProjectContext: "# Mission\n\nLaunch Q3.",
			UserGuidance:   "Never publish without review.",
			Source:         "dot_autonomy",
			Truncated:      nil,
		},
	}
	raw := buildAgentInput(task, "exec_1", "wf", "swarm", "step_a", "lead", "Create a plan", opts)

	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal task.json: %v", err)
	}
	ctx, ok := parsed["context"].(map[string]any)
	if !ok {
		t.Fatalf("context block missing")
	}
	if got, _ := ctx["projectContext"].(string); !strings.Contains(got, "Launch Q3") {
		t.Errorf("projectContext field missing or wrong: %q", got)
	}
	if got, _ := ctx["userGuidance"].(string); !strings.Contains(got, "Never publish") {
		t.Errorf("userGuidance field missing or wrong: %q", got)
	}
	if got, _ := ctx["projectContextSource"].(string); got != "dot_autonomy" {
		t.Errorf("projectContextSource = %q, want dot_autonomy", got)
	}
	if _, ok := ctx["projectContextTruncated"]; ok {
		t.Errorf("projectContextTruncated should NOT appear when nothing was truncated")
	}
	// System-prompt patch lands when the canonical context is
	// populated, even with no role-level SystemPrompt.
	if got, _ := ctx["systemPrompt"].(string); !strings.Contains(got, "context.projectContext") {
		t.Errorf("systemPrompt should reference context.projectContext when canonical context is loaded; got %q", got)
	}
}

// TestBuildAgentInput_NoCanonicalContextKeys confirms the
// task.json stays clean for projects that don't use the
// convention — no spurious keys.
func TestBuildAgentInput_NoCanonicalContextKeys(t *testing.T) {
	task := &persistence.Task{ID: "task_x", ProjectID: "proj", Payload: []byte(`{"context":{"prompt":"hello"}}`)}
	raw := buildAgentInput(task, "exec_1", "wf", "swarm", "step_a", "lead", "Create a plan", &agentInputOpts{})
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ctx, ok := parsed["context"].(map[string]any)
	if !ok {
		t.Fatalf("context block missing")
	}
	for _, k := range []string{"projectContext", "userGuidance", "projectContextSource", "projectContextTruncated"} {
		if _, ok := ctx[k]; ok {
			t.Errorf("context[%q] should be absent on projects without the convention", k)
		}
	}
	if _, ok := ctx["systemPrompt"]; ok {
		t.Errorf("systemPrompt should be absent when neither role prompt nor canonical context populated")
	}
}

// TestBuildAgentInput_CanonicalContextTruncatedSurfaced pins
// that the per-file truncated marker reaches the agent.
func TestBuildAgentInput_CanonicalContextTruncatedSurfaced(t *testing.T) {
	task := &persistence.Task{ID: "task_x", ProjectID: "proj"}
	opts := &agentInputOpts{
		CanonicalContext: CanonicalContext{
			ProjectContext: "small",
			Truncated:      []string{"user"},
		},
	}
	raw := buildAgentInput(task, "exec_1", "wf", "swarm", "step_a", "lead", "p", opts)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	ctx := parsed["context"].(map[string]any)
	trunc, ok := ctx["projectContextTruncated"].([]any)
	if !ok {
		t.Fatalf("projectContextTruncated should be present; got %#v", ctx["projectContextTruncated"])
	}
	if len(trunc) != 1 || trunc[0] != "user" {
		t.Errorf("projectContextTruncated = %v, want [user]", trunc)
	}
}
