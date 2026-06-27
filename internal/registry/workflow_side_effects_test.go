package registry

import "testing"

// TestWorkflowStep_HasExternalSideEffects pins the classifier the
// retry-from-step containment guard keys on: only step types that mutate
// state OUTSIDE the execution's own workspace/state machine (and therefore
// are NOT replayed on a retry-from-step) count as side-effecting.
func TestWorkflowStep_HasExternalSideEffects(t *testing.T) {
	cases := []struct {
		typ  string
		want bool
	}{
		{"system", true},        // forge.post_review / rag.index — external writes
		{"call_project", true},  // spawns/awaits a callee-project task
		{"agent", false},        // may call tools, but no per-step idempotency decl to key on
		{"gate", false},         // pure control flow
		{"approval", false},     // pure control flow
		{"plan", false},         // pure control flow
		{"", false},             // unset
		{"unknown_type", false}, // forward-compat: unrecognized → not flagged
	}
	for _, tc := range cases {
		t.Run(tc.typ, func(t *testing.T) {
			got := WorkflowStep{Type: tc.typ}.HasExternalSideEffects()
			if got != tc.want {
				t.Errorf("WorkflowStep{Type:%q}.HasExternalSideEffects() = %v, want %v", tc.typ, got, tc.want)
			}
		})
	}
}
