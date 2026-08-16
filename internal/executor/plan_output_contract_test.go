package executor

import (
	"encoding/json"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// Regression, 2026-08-16. The daemon enforces require_output_glob AFTER the
// container exits, and the agent was never told the contract existed — so it
// finished, logged "completed successfully", and only then had the step failed
// for a file nothing had asked it to write.
//
// dp-02-parser-hardening failed this way on 3 of 3 benchmark runs. The tool-free
// schema finalization makes it unrecoverable once entered, because no tools are
// offered there, so the agent must learn about the contract while it can still
// act on it.
func TestBuildAgentInput_CarriesTheOutputContract(t *testing.T) {
	task := &persistence.Task{ID: "task-1", ProjectID: "proj-1"}

	withGlob := buildAgentInput(task, "exec-1", "wf-1", "swarm-1", "step-1", "analyst", "do the thing",
		&agentInputOpts{RequireOutputGlob: "artifacts/out/CHANGELOG-partial.md"})
	var got map[string]any
	if err := json.Unmarshal(withGlob, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wf, _ := got["workflow"].(map[string]any)
	if wf["requireOutputGlob"] != "artifacts/out/CHANGELOG-partial.md" {
		t.Errorf("requireOutputGlob = %v, want the declared glob", wf["requireOutputGlob"])
	}

	// Additive: a step with no contract must produce the payload it always did,
	// so an older agent image sees nothing new and nothing changes for it.
	without := buildAgentInput(task, "exec-1", "wf-1", "swarm-1", "step-1", "analyst", "do the thing",
		&agentInputOpts{})
	var bare map[string]any
	if err := json.Unmarshal(without, &bare); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	bareWf, _ := bare["workflow"].(map[string]any)
	if _, present := bareWf["requireOutputGlob"]; present {
		t.Error("a step with no contract must not gain the field")
	}
}
