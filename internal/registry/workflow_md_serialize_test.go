package registry

import (
	"strings"
	"testing"
)

func TestMarshalWorkflowMarkdown_RoundTrip(t *testing.T) {
	wf := &Workflow{
		ID:          "research",
		DisplayName: "Research",
		Description: "Find things.",
		Version:     "1.0",
		Entrypoint:  "research",
		Steps: map[string]WorkflowStep{
			"research": {
				Type:      "agent",
				Role:      "researcher",
				Prompt:    "Find stuff.",
				OnSuccess: "done",
				OnFail:    "failed",
			},
		},
		Terminals: map[string]WorkflowTerminal{
			"done":   {Status: "COMPLETED"},
			"failed": {Status: "FAILED"},
		},
	}
	out, err := MarshalWorkflowMarkdown(wf)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	parsed, err := ParseWorkflowMarkdown(out, "rt.md")
	if err != nil {
		t.Fatalf("parse: %v\noutput:\n%s", err, out)
	}
	if parsed.ID != wf.ID || parsed.Entrypoint != wf.Entrypoint {
		t.Errorf("structural mismatch: got %#v", parsed)
	}
	if parsed.Steps["research"].Prompt != "Find stuff." {
		t.Errorf("prompt round-trip lost: %q", parsed.Steps["research"].Prompt)
	}
}

func TestMarshalWorkflowMarkdown_RequiresID(t *testing.T) {
	_, err := MarshalWorkflowMarkdown(&Workflow{Entrypoint: "s"})
	if err == nil || !strings.Contains(err.Error(), "workflowId") {
		t.Errorf("want workflowId required error, got %v", err)
	}
}

func TestMarshalWorkflowMarkdown_NilRejected(t *testing.T) {
	_, err := MarshalWorkflowMarkdown(nil)
	if err == nil || !strings.Contains(err.Error(), "nil") {
		t.Errorf("want nil error, got %v", err)
	}
}
