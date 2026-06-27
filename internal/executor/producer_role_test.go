package executor

import (
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestProducerRoleForExecution_FallbackIsEmpty — the canonical
// regression for the 2026-05-14 assistant-project incident: the
// fallback string used to be "executor", which collided with the
// ibkr-trader swarm's real role of the same name and caused 224
// chunks to be misclassified as commit_msg. Empty string is the
// truthful answer when no producer role can be derived.
func TestProducerRoleForExecution_FallbackIsEmpty(t *testing.T) {
	if got := producerRoleForExecution(nil, nil); got != "" {
		t.Fatalf("nil execution: got %q, want \"\"", got)
	}
	// Empty completed steps → no producer to derive → fallback.
	if got := producerRoleForExecution(&persistence.Execution{}, nil); got != "" {
		t.Fatalf("empty steps: got %q, want \"\"", got)
	}
	// Steps with non-matching naming convention AND no workflow
	// available for fallback → empty. This was the pre-Measure-2
	// hot path for the assistant project (semantic step names like
	// "research" or "route" never matched stepIDToRole), which is
	// exactly what the new wf-lookup branch fixes — see
	// TestProducerRoleForExecution_WorkflowLookupFallback.
	exec := &persistence.Execution{
		CompletedSteps: []string{"setup", "teardown", "weird-id"},
	}
	if got := producerRoleForExecution(exec, nil); got != "" {
		t.Fatalf("non-conforming, no wf: got %q, want \"\"", got)
	}
}

// TestProducerRoleForExecution_PicksLastDevPipelineRole pins the
// dev-pipeline `plan_<n>_<role>` lookup that survives independent of
// the workflow argument — used by dynamic plans whose step IDs are
// generated at runtime, not declared in the workflow definition.
func TestProducerRoleForExecution_PicksLastDevPipelineRole(t *testing.T) {
	exec := &persistence.Execution{
		CompletedSteps: []string{
			"plan_0_lead",
			"plan_1_researcher",
			"plan_2_writer",
		},
	}
	if got := producerRoleForExecution(exec, nil); got != "writer" {
		t.Fatalf("expected last role (writer), got %q", got)
	}
}

// TestProducerRoleForExecution_WorkflowLookupFallback (Measure 2,
// 2026-05-15 incident) — when no completed step matches the
// dev-pipeline `plan_<n>_<role>` convention, fall back to looking
// the step ID up in the workflow definition and returning
// step.Role. This is the fix for the 79-chunks-with-empty-role
// regression on the assistant project, where workflows like
// research / plan-and-write / adaptive use semantic step IDs that
// stepIDToRole could never recognise.
func TestProducerRoleForExecution_WorkflowLookupFallback(t *testing.T) {
	wf := &registry.Workflow{
		ID:         "plan-and-write",
		Entrypoint: "research",
		Steps: map[string]registry.WorkflowStep{
			"research": {Role: "researcher"},
			"plan":     {Role: "planner"},
			"write":    {Role: "writer"},
		},
	}
	exec := &persistence.Execution{
		CompletedSteps: []string{"research", "plan", "write"},
	}
	if got := producerRoleForExecution(exec, wf); got != "writer" {
		t.Fatalf("expected writer (last semantic step's role), got %q", got)
	}

	// Single-step adaptive route — the only completed step's role is
	// `lead`. The pre-Measure-2 implementation explicitly skipped
	// `lead`, which produced "" here; the post-fix returns "lead"
	// so the chunk lands with a real role and ClassifyByRole maps
	// to ClassSpec. Routing transcripts themselves are filtered out
	// upstream by isTranscriptArtifact, so the only artifacts that
	// reach this code path with role=lead are LEGITIMATE lead
	// outputs (e.g. autonomy digests) that we want classified.
	adaptiveWF := &registry.Workflow{
		ID:         "adaptive",
		Entrypoint: "route",
		Steps:      map[string]registry.WorkflowStep{"route": {Role: "lead"}},
	}
	exec2 := &persistence.Execution{CompletedSteps: []string{"route"}}
	if got := producerRoleForExecution(exec2, adaptiveWF); got != "lead" {
		t.Fatalf("adaptive route step: got %q, want lead", got)
	}
}

// TestProducerRoleForExecution_RetrySuffixesStripped — a step that
// re-ran via shape_retry / refusal_retry / route_retry / model_fallback
// / infra_retry<N> still resolves to the base step's role. Without
// this stripping, the workflow lookup would miss (the workflow YAML
// has no row for `write_shape_retry`).
func TestProducerRoleForExecution_RetrySuffixesStripped(t *testing.T) {
	wf := &registry.Workflow{
		Steps: map[string]registry.WorkflowStep{
			"write": {Role: "writer"},
		},
	}
	cases := []struct {
		stepID string
		want   string
	}{
		{"write_shape_retry", "writer"},
		{"write_refusal_retry", "writer"},
		{"write_model_fallback", "writer"},
		{"write_infra_retry1", "writer"},
		{"write_infra_retry5", "writer"},
		{"write_route_retry", "writer"},
		{"write", "writer"}, // no suffix still works
	}
	for _, tc := range cases {
		exec := &persistence.Execution{CompletedSteps: []string{tc.stepID}}
		if got := producerRoleForExecution(exec, wf); got != tc.want {
			t.Errorf("stepID=%s: got %q, want %q", tc.stepID, got, tc.want)
		}
	}
}

// TestStripRetryStepSuffix_UnchangedForUnknownSuffix — the helper
// must be a no-op on step IDs that don't carry a known retry suffix,
// otherwise we'd accidentally trim legitimate underscores in
// project-defined step names (e.g. a workflow author writing
// "write_post" as a step ID).
func TestStripRetryStepSuffix_UnchangedForUnknownSuffix(t *testing.T) {
	cases := []string{"write", "research", "write_post", "plan_v2"}
	for _, s := range cases {
		if got := stripRetryStepSuffix(s); got != s {
			t.Errorf("expected %q unchanged, got %q", s, got)
		}
	}
}
