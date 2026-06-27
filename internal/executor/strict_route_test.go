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
