package registry

import (
	"strings"
	"testing"
)

// TestReplaceSwarmRolePromptsKeeping prunes body subsections for roles no
// longer in the swarm (the schema editor removes a role → its `### name`
// prompt body must go too, or the parser rejects the orphan). Kept roles
// are updated/preserved and unrelated body sections survive.
func TestReplaceSwarmRolePromptsKeeping(t *testing.T) {
	body := []byte(`# Swarm

## Role prompts

### lead

Plan it.

### coder

Old coder body.

## Notes

Keep me.
`)

	out, err := ReplaceSwarmRolePromptsKeeping(body,
		map[string]string{"lead": "Plan carefully."},
		map[string]bool{"lead": true},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := string(out)

	if !strings.Contains(got, "Plan carefully.") {
		t.Error("kept role's updated body missing")
	}
	if strings.Contains(got, "### coder") || strings.Contains(got, "Old coder body.") {
		t.Errorf("removed role's subsection should be pruned, got:\n%s", got)
	}
	if !strings.Contains(got, "## Notes") || !strings.Contains(got, "Keep me.") {
		t.Error("unrelated body section must survive")
	}
}

// TestReplaceSwarmRolePrompts_NilKeepKeepsAll — the original behaviour
// (no pruning) is preserved when keep is not supplied.
func TestReplaceSwarmRolePrompts_NilKeepKeepsAll(t *testing.T) {
	body := []byte("## Role prompts\n\n### a\n\nA.\n\n### b\n\nB.\n")
	out, err := ReplaceSwarmRolePrompts(body, map[string]string{"a": "A2."})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "### b") || !strings.Contains(got, "B.") {
		t.Errorf("nil keep must preserve every subsection, got:\n%s", got)
	}
}
