package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseWorkflowMarkdown_DescriptionRoundTrip — the new
// `description:` frontmatter field round-trips into Workflow.Description
// so the workflow_md_shape doctor check and the dashboard can read it
// without re-parsing the source file.
func TestParseWorkflowMarkdown_DescriptionRoundTrip(t *testing.T) {
	md := `---
workflowId: "x"
description: "Short summary surfaced in dashboards."
entrypoint: "plan"
steps:
  plan:
    type: "agent"
    role: "lead"
    prompt: "p"
---
`
	wf, err := ParseWorkflowMarkdown([]byte(md), "desc.md")
	if err != nil {
		t.Fatalf("ParseWorkflowMarkdown: %v", err)
	}
	if wf.Description != "Short summary surfaced in dashboards." {
		t.Errorf("Description = %q, want round-tripped value", wf.Description)
	}
}

// TestWorkflowValidate_DescriptionOverLimitRejected — anything past
// WorkflowDescriptionMaxLen surfaces at load time so a runaway paste
// never reaches the dashboard.
func TestWorkflowValidate_DescriptionOverLimitRejected(t *testing.T) {
	wf := Workflow{
		ID:          "x",
		Entrypoint:  "plan",
		Description: strings.Repeat("a", WorkflowDescriptionMaxLen+1),
		Steps: map[string]WorkflowStep{
			"plan": {Type: "agent", Role: "lead", Prompt: "p"},
		},
	}
	err := wf.Validate("over-limit.md")
	if err == nil {
		t.Fatal("expected validation error for over-long description")
	}
	if !strings.Contains(err.Error(), "description") {
		t.Errorf("err = %v, want mention of description field", err)
	}
}

// TestWorkflowValidate_DescriptionAtLimitAccepted — exactly at the
// limit is fine; the rejection is strict-greater-than.
func TestWorkflowValidate_DescriptionAtLimitAccepted(t *testing.T) {
	wf := Workflow{
		ID:          "x",
		Entrypoint:  "plan",
		Description: strings.Repeat("a", WorkflowDescriptionMaxLen),
		Steps: map[string]WorkflowStep{
			"plan": {Type: "agent", Role: "lead", Prompt: "p", OnSuccess: "done"},
		},
		Terminals: map[string]WorkflowTerminal{
			"done": {Status: "COMPLETED"},
		},
	}
	if err := wf.Validate("at-limit.md"); err != nil {
		t.Errorf("Validate at exact cap should pass, got: %v", err)
	}
}

// TestWorkflowValidate_DescriptionEmptyAllowed — empty description
// is allowed by Validate (backward compat); the workflow_md_shape
// doctor check is what surfaces missing descriptions to operators.
// Splitting validation from the doctor lint keeps existing YAML
// loadable.
func TestWorkflowValidate_DescriptionEmptyAllowed(t *testing.T) {
	wf := Workflow{
		ID:         "x",
		Entrypoint: "plan",
		Steps: map[string]WorkflowStep{
			"plan": {Type: "agent", Role: "lead", Prompt: "p", OnSuccess: "done"},
		},
		Terminals: map[string]WorkflowTerminal{
			"done": {Status: "COMPLETED"},
		},
	}
	if err := wf.Validate("no-desc.md"); err != nil {
		t.Errorf("empty description should pass Validate; got: %v", err)
	}
}

// TestBundledWorkflowsCarryDescription — every shipped WORKFLOW.md
// must carry a non-empty `description:` so the doctor check stays
// green on a fresh clone. Catches future drift where a new
// workflow lands without filling in the field.
func TestBundledWorkflowsCarryDescription(t *testing.T) {
	dir := "../../configs/workflows"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		wf, err := ParseWorkflowMarkdown(data, name)
		if err != nil {
			t.Errorf("%s parse: %v", name, err)
			continue
		}
		if strings.TrimSpace(wf.Description) == "" {
			t.Errorf("%s: missing `description:` frontmatter field — workflow_md_shape doctor check will ERR", name)
		}
		if len(wf.Description) > WorkflowDescriptionMaxLen {
			t.Errorf("%s: description is %d chars; cap is %d", name, len(wf.Description), WorkflowDescriptionMaxLen)
		}
	}
}
