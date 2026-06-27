package registry

import "testing"

// TestParseWorkflow_IngestInputArtifacts_SteplessValidates — the
// deterministic bulk-ingest workflow has no agent step: its entrypoint
// routes straight to a terminal and ingest_input_artifacts tells the
// executor to deposit the staged input artifacts itself. Parsing must
// round-trip the flag, and Validate must accept the step-less body
// (the "at least one step is required" rule is waived when the flag is
// set).
func TestParseWorkflow_IngestInputArtifacts_SteplessValidates(t *testing.T) {
	md := `---
workflowId: "companion-rag-ingest"
entrypoint: "done"
require_input_artifacts: true
ingest_input_artifacts: true
terminals:
  done:
    status: "COMPLETED"
---
`
	wf, err := ParseWorkflowMarkdown([]byte(md), "companion-rag-ingest.md")
	if err != nil {
		t.Fatalf("ParseWorkflowMarkdown: %v", err)
	}
	if !wf.IngestInputArtifacts {
		t.Fatal("ingest_input_artifacts did not round-trip to Workflow.IngestInputArtifacts")
	}
	if err := wf.Validate("companion-rag-ingest.md"); err != nil {
		t.Fatalf("step-less ingest workflow must validate, got: %v", err)
	}
}

// TestWorkflowValidate_SteplessStillRejectedWithoutFlag — without the
// ingest_input_artifacts opt-in, a step-less workflow is still an error
// (guards against the relaxed check leaking to ordinary workflows).
func TestWorkflowValidate_SteplessStillRejectedWithoutFlag(t *testing.T) {
	wf := Workflow{
		ID:         "x",
		Entrypoint: "done",
		Terminals:  map[string]WorkflowTerminal{"done": {Status: "COMPLETED"}},
	}
	if err := wf.Validate("x.md"); err == nil {
		t.Fatal("a step-less workflow without ingest_input_artifacts must fail validation")
	}
}
