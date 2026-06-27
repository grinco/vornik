package registry

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDevPipelineRecoveryCheckpointWiring is the regression guard for the
// graceful-checkpoint recovery design
// (https://docs.vornik.io §8).
//
// A hard error on a per-subtask-loop step (implement / test / review) must
// route to the recover-checkpoint step rather than dead-ending the whole
// feature at the failed terminal. recover-checkpoint is an analyst agent
// step that parks the stuck subtask and terminates as the existing
// COMPLETED-partial checkpoint. analyze / report / checkpoint-report stay on
// hard-fail by deliberate design (see spec §3), so this test also pins that
// they are NOT rerouted.
func TestDevPipelineRecoveryCheckpointWiring(t *testing.T) {
	root := repoRootFromRegistryTest(t)
	path := filepath.Join(root, "configs", "workflows", "dev-pipeline.md")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	wf, err := ParseWorkflowMarkdown(content, path)
	if err != nil {
		t.Fatalf("ParseWorkflowMarkdown(dev-pipeline): %v", err)
	}

	// The per-subtask-loop steps reroute hard failures to recover-checkpoint.
	for _, stepID := range []string{"implement", "test", "review"} {
		step, ok := wf.Steps[stepID]
		if !ok {
			t.Fatalf("dev-pipeline missing expected step %q", stepID)
		}
		if step.OnFail != "recover-checkpoint" {
			t.Errorf("step %q on_fail = %q; want %q", stepID, step.OnFail, "recover-checkpoint")
		}
	}

	// The recover-checkpoint step itself: an analyst agent that parks the
	// stuck subtask and exits via the checkpoint terminal; if the analyst
	// itself errors, it gives up to the failed terminal.
	rc, ok := wf.Steps["recover-checkpoint"]
	if !ok {
		t.Fatalf("dev-pipeline missing recover-checkpoint step")
	}
	if rc.Type != "agent" {
		t.Errorf("recover-checkpoint type = %q; want %q", rc.Type, "agent")
	}
	if rc.Role != "analyst" {
		t.Errorf("recover-checkpoint role = %q; want %q", rc.Role, "analyst")
	}
	if rc.OnSuccess != "checkpoint" {
		t.Errorf("recover-checkpoint on_success = %q; want %q", rc.OnSuccess, "checkpoint")
	}
	if rc.OnFail != "failed" {
		t.Errorf("recover-checkpoint on_fail = %q; want %q", rc.OnFail, "failed")
	}

	// Deliberate exclusions (spec §3): these keep hard-fail semantics.
	for _, stepID := range []string{"analyze", "report", "checkpoint-report"} {
		step, ok := wf.Steps[stepID]
		if !ok {
			t.Fatalf("dev-pipeline missing expected step %q", stepID)
		}
		if step.OnFail != "failed" {
			t.Errorf("excluded step %q on_fail = %q; want %q (must stay hard-fail)", stepID, step.OnFail, "failed")
		}
	}
}
