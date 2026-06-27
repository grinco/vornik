package registry

import (
	"strings"
	"testing"
)

// TestReplaceWorkflowStepPromptsKeeping prunes body subsections for steps
// no longer in the workflow (the schema editor removes/renames a step →
// its `### id` prompt body must go too, or the parser rejects the orphan).
func TestReplaceWorkflowStepPromptsKeeping(t *testing.T) {
	body := []byte(`## Prompts

### build

Build it.

### gone

Old removed step body.

## Notes

Keep me.
`)

	out, err := ReplaceWorkflowStepPromptsKeeping(body,
		map[string]string{"build": "Build carefully."},
		map[string]bool{"build": true},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "Build carefully.") {
		t.Error("kept step's updated body missing")
	}
	if strings.Contains(got, "### gone") || strings.Contains(got, "Old removed step body.") {
		t.Errorf("removed step's subsection should be pruned, got:\n%s", got)
	}
	if !strings.Contains(got, "## Notes") || !strings.Contains(got, "Keep me.") {
		t.Error("unrelated body section must survive")
	}
}
