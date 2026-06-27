package executor

import (
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/verifier"
)

// TestBuildAgentInput_RecoveryContextRendered confirms that when a
// recovery execution forwards a RecoveryContext to a plain agent step,
// the failure signal is rendered into the prompt the model reads (not
// only stashed under context.recovery for programmatic consumers).
// see https://docs.vornik.io §2
func TestBuildAgentInput_RecoveryContextRendered(t *testing.T) {
	task := &persistence.Task{ID: "task_x", ProjectID: "proj", Payload: []byte(`{"context":{"prompt":"do the thing"}}`)}
	opts := &agentInputOpts{
		RecoveryContext: &RecoveryContext{
			FailedStep:    "research",
			FailureClass:  "verifier_block",
			FailureReason: "reuters returned http_403 (auth_required)",
			BlockedURLs: []verifier.BlockedURL{
				{URL: "https://reuters.com/x", Reason: "auth_required"},
			},
		},
	}
	raw := buildAgentInput(task, "exec_1", "wf", "swarm", "recover", "lead", "Propose alternatives", opts)

	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal task.json: %v", err)
	}
	ctx, ok := parsed["context"].(map[string]any)
	if !ok {
		t.Fatalf("context block missing")
	}

	// The rendered prompt the model reads must carry the recovery block.
	prompt, _ := ctx["prompt"].(string)
	if !strings.Contains(prompt, "## RECOVERY_CONTEXT") {
		t.Errorf("prompt missing RECOVERY_CONTEXT block; got:\n%s", prompt)
	}
	for _, want := range []string{"research", "verifier_block", "http_403", "reuters.com"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("recovery block missing %q; prompt:\n%s", want, prompt)
		}
	}

	// The structured copy still reaches programmatic consumers.
	if _, ok := ctx["recovery"]; !ok {
		t.Errorf("context.recovery should still be present for programmatic consumers")
	}
}

// TestBuildAgentInput_NoRecoveryBlockOnNormalExecution confirms a
// normal (non-recovery) execution omits the recovery block entirely.
func TestBuildAgentInput_NoRecoveryBlockOnNormalExecution(t *testing.T) {
	task := &persistence.Task{ID: "task_y", ProjectID: "proj", Payload: []byte(`{"context":{"prompt":"do the thing"}}`)}
	raw := buildAgentInput(task, "exec_1", "wf", "swarm", "step_a", "lead", "Work", &agentInputOpts{})

	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ctx := parsed["context"].(map[string]any)
	prompt, _ := ctx["prompt"].(string)
	if strings.Contains(prompt, "RECOVERY_CONTEXT") {
		t.Errorf("normal execution should not render a recovery block; prompt:\n%s", prompt)
	}
	if _, ok := ctx["recovery"]; ok {
		t.Errorf("context.recovery should be absent on a normal execution")
	}
}

// TestBuildRecoveryContextBlock_OmitsEmptyFields pins that optional
// fields (blocked_urls, failure_reason) are skipped when empty so a
// non-verifier failure class renders a clean block.
func TestBuildRecoveryContextBlock_OmitsEmptyFields(t *testing.T) {
	got := buildRecoveryContextBlock(&RecoveryContext{
		FailedStep:   "write",
		FailureClass: "pandoc_error",
	})
	if !strings.Contains(got, "failed_step: write") {
		t.Errorf("missing failed_step; got:\n%s", got)
	}
	if !strings.Contains(got, "failure_class: pandoc_error") {
		t.Errorf("missing failure_class; got:\n%s", got)
	}
	if strings.Contains(got, "blocked_urls") {
		t.Errorf("blocked_urls should be omitted when empty; got:\n%s", got)
	}
	if strings.Contains(got, "failure_reason") {
		t.Errorf("failure_reason should be omitted when empty; got:\n%s", got)
	}
}
