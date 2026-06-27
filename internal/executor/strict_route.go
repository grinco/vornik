package executor

import "vornik.io/vornik/internal/registry"

// isStrictRouteStep reports whether the current step is a strict-adaptive
// routing step — one that may auto-route / delegate to a child workflow from the
// project's AdaptiveCandidateWorkflows.
//
// Two cases qualify:
//   - the built-in `adaptive` workflow (any step — it has only the route step), and
//   - the ENTRYPOINT of any workflow that opts in via `resume_after_children`
//     (e.g. github-router's `intake`, which delegates dev-pipeline then resumes
//     to its deterministic publish step).
//
// Confining the custom-workflow case to the entrypoint is what keeps a later
// step (a publish or review step) in a resume_after_children workflow from being
// misread as a routing step — only the first step delegates. For ID=="adaptive"
// the result is unchanged from the historical `wf.ID == "adaptive"` guard.
func isStrictRouteStep(wf *registry.Workflow, stepID string) bool {
	if wf == nil {
		return false
	}
	if wf.ID == "adaptive" {
		return true
	}
	return wf.ResumeAfterChildren && stepID == wf.Entrypoint
}
