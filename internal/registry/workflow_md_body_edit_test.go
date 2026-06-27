package registry

import (
	"strings"
	"testing"
)

// workflowBodyFixture mirrors swarmBodyFixture but with the
// `## Prompts` section + step-id subheadings that
// WORKFLOW.md uses.
const workflowBodyFixture = `# Test Workflow

Plan → implement → review loop.

## Prompts

### plan

Plan the work before implementing.

### implement

Implement the plan one step at a time.

## Error handling

Plan retries on failure.
`

// TestReplaceWorkflowStepPrompts_ReplacesSingleStep — updating
// one step's body leaves siblings and the `## Error handling`
// docs section verbatim. The load-bearing case for the
// workflow editor: an operator iterating on the plan step's
// prompt shouldn't disturb anything else.
func TestReplaceWorkflowStepPrompts_ReplacesSingleStep(t *testing.T) {
	updates := map[string]string{
		"plan": "Plan very carefully. Surface every assumption.",
	}
	out, err := ReplaceWorkflowStepPrompts([]byte(workflowBodyFixture), updates)
	if err != nil {
		t.Fatalf("ReplaceWorkflowStepPrompts: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "Plan very carefully. Surface every assumption.") {
		t.Errorf("new plan prompt not written. got:\n%s", got)
	}
	if strings.Contains(got, "Plan the work before implementing.") {
		t.Errorf("old plan prompt should have been replaced. got:\n%s", got)
	}
	if !strings.Contains(got, "Implement the plan one step at a time.") {
		t.Errorf("implement prompt should have survived. got:\n%s", got)
	}
	if !strings.Contains(got, "## Error handling") || !strings.Contains(got, "Plan retries on failure.") {
		t.Errorf("Error handling docs section should have survived. got:\n%s", got)
	}
}

// TestReplaceWorkflowStepPrompts_AppendsMissingStepBody — a
// step in the update map without an existing subsection gets
// appended at the end of `## Prompts`. Matches the swarm
// behaviour so the editor pattern is consistent across both
// authoring primitives.
func TestReplaceWorkflowStepPrompts_AppendsMissingStepBody(t *testing.T) {
	updates := map[string]string{
		"review": "Review carefully; reject vague success claims.",
	}
	out, err := ReplaceWorkflowStepPrompts([]byte(workflowBodyFixture), updates)
	if err != nil {
		t.Fatalf("ReplaceWorkflowStepPrompts: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "### review") {
		t.Errorf("review subsection missing. got:\n%s", got)
	}
	if !strings.Contains(got, "Review carefully; reject vague success claims.") {
		t.Errorf("review body missing. got:\n%s", got)
	}
}

// TestReplaceWorkflowStepPrompts_EmptyUpdates_NoOp — verbatim
// output when nothing to update.
func TestReplaceWorkflowStepPrompts_EmptyUpdates_NoOp(t *testing.T) {
	out, err := ReplaceWorkflowStepPrompts([]byte(workflowBodyFixture), nil)
	if err != nil {
		t.Fatalf("ReplaceWorkflowStepPrompts: %v", err)
	}
	if string(out) != workflowBodyFixture {
		t.Errorf("empty updates should produce verbatim output")
	}
}

// TestSplitWorkflowContent_Roundtrip — Split + Join is the
// inverse pair; the joined output must re-parse cleanly as a
// WORKFLOW.md.
func TestSplitWorkflowContent_Roundtrip(t *testing.T) {
	content := []byte(`---
workflowId: t
entrypoint: plan
steps:
  plan:
    type: agent
    role: lead
    on_success: done
    prompt: inline plan
terminals:
  done:
    status: COMPLETED
---

# Body

## Prompts

### plan

inline plan
`)
	fm, body, err := SplitWorkflowContent(content, "t.md")
	if err != nil {
		t.Fatalf("SplitWorkflowContent: %v", err)
	}
	if !strings.Contains(string(fm), "workflowId: t") {
		t.Errorf("frontmatter missing workflowId. got:\n%s", string(fm))
	}
	joined := JoinWorkflowContent(fm, body)
	if _, err := ParseWorkflowMarkdown(joined, "round.md"); err != nil {
		t.Errorf("re-parse failed after Split+Join: %v\nbody:\n%s", err, string(joined))
	}
}

// TestSplitWorkflowContent_RejectsNoFrontmatter — same error
// surface as the swarm split helper.
func TestSplitWorkflowContent_RejectsNoFrontmatter(t *testing.T) {
	_, _, err := SplitWorkflowContent([]byte("# just markdown\n"), "x.md")
	if err == nil || !strings.Contains(err.Error(), "opening frontmatter marker") {
		t.Errorf("err = %v, want missing-frontmatter rejection", err)
	}
}

// TestReplaceSwarmRolePrompts_StillWorks_AfterRefactor —
// pinning that extracting the shared helper didn't change the
// swarm-side semantics. (The swarm-specific test file covers
// every detail; this is a smoke test guarding the refactor.)
func TestReplaceSwarmRolePrompts_StillWorks_AfterRefactor(t *testing.T) {
	out, err := ReplaceSwarmRolePrompts([]byte(swarmBodyFixture), map[string]string{
		"lead": "new lead body",
	})
	if err != nil {
		t.Fatalf("ReplaceSwarmRolePrompts: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "new lead body") {
		t.Errorf("swarm role prompt replacement broken after refactor. got:\n%s", got)
	}
	if strings.Contains(got, "Plan the work, then delegate.") {
		t.Errorf("old swarm lead body should have been replaced. got:\n%s", got)
	}
}
