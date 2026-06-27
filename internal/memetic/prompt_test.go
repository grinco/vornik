package memetic

import (
	"strings"
	"testing"
)

// TestDefaultSystemPrompt_InstructsKind guards the architect-`kind`
// output contract. The parse → validate → kill-switch machinery for
// `kind` is wired (architect.go), but it is inert unless the prompt
// actually tells the model to emit `kind` — otherwise every proposal
// silently lands `unspecified` (the audit residual this closes). A
// future prompt edit that drops the instruction fails here.
func TestDefaultSystemPrompt_InstructsKind(t *testing.T) {
	if !strings.Contains(defaultSystemPrompt, `"kind"`) {
		t.Fatal("default system prompt must instruct the model to emit a `kind` field")
	}
	// The classes the architect is allowed to propose. change_role_assignment
	// is intentionally NOT offered (rule 2 forbids role changes); unspecified
	// is the fallback, not an offered choice.
	for _, k := range []string{
		"add_step", "remove_step", "change_transition",
		"change_timeout", "change_retry_policy", "reorder_steps",
	} {
		if !strings.Contains(defaultSystemPrompt, k) {
			t.Errorf("default system prompt should enumerate kind %q", k)
		}
	}
}
