package executor

import (
	"testing"

	"vornik.io/vornik/internal/registry"
)

// TestIsStrictRouteStep pins the generalization that lets a resume_after_children
// workflow (e.g. github-router) delegate from its entrypoint, while keeping the
// built-in `adaptive` workflow's behavior unchanged and confining custom
// workflows to their entrypoint (so a later publish/review step never delegates).
func TestIsStrictRouteStep(t *testing.T) {
	adaptive := &registry.Workflow{ID: "adaptive", Entrypoint: "route"}
	router := &registry.Workflow{ID: "github-router", Entrypoint: "intake", ResumeAfterChildren: true}
	plain := &registry.Workflow{ID: "dev-pipeline", Entrypoint: "analyze"}

	cases := []struct {
		name   string
		wf     *registry.Workflow
		stepID string
		want   bool
	}{
		{"nil workflow", nil, "x", false},
		{"adaptive any step", adaptive, "route", true},
		{"adaptive non-entrypoint still true (only has route)", adaptive, "anything", true},
		{"resume_after_children entrypoint", router, "intake", true},
		{"resume_after_children non-entrypoint (publish) does NOT delegate", router, "publish", false},
		{"plain workflow never delegates", plain, "analyze", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isStrictRouteStep(tc.wf, tc.stepID); got != tc.want {
				t.Errorf("isStrictRouteStep(%v, %q) = %v, want %v", tc.wf, tc.stepID, got, tc.want)
			}
		})
	}
}

// TestIsDelegatorStep pins the router/delegator discriminator introduced for
// incident task_20260712143854_429a3500d692d23c: a step that pins
// delegated_workflow contractually delegates via delegatedTasks and must be
// excluded from both selected_workflow spawn paths.
func TestIsDelegatorStep(t *testing.T) {
	cases := []struct {
		name string
		step registry.WorkflowStep
		want bool
	}{
		{"pinned delegated_workflow", registry.WorkflowStep{DelegatedWorkflow: "research-subtask"}, true},
		{"whitespace-only pin is no pin", registry.WorkflowStep{DelegatedWorkflow: "  "}, false},
		{"no pin", registry.WorkflowStep{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDelegatorStep(tc.step); got != tc.want {
				t.Errorf("isDelegatorStep(%+v) = %v, want %v", tc.step, got, tc.want)
			}
		})
	}
}
