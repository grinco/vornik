package executor

import (
	"strings"

	"vornik.io/vornik/internal/registry"
)

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

// isDelegatorStep reports whether the step contractually delegates its
// children via `delegatedTasks` — it pins a step-level `delegated_workflow`
// (issue-fix's and deep-research's decompose). Such a step is NEVER a
// strict-adaptive router, even when isStrictRouteStep returns true for it
// (a resume_after_children entrypoint): its result carries delegatedTasks,
// not selected_workflow, so the route paths must leave it alone.
//
// isStrictRouteStep itself stays broad on purpose — the RESUME guard keys on
// it and must keep covering delegator entrypoints (re-running issue-fix's
// decompose on resume would re-spawn its subtasks). Only the two
// selected_workflow spawn paths (single-candidate auto-route +
// handleSelectedWorkflowRoute) exclude delegator steps via this check.
//
// Incident task_20260712143854_429a3500d692d23c (2026-07-12): the first
// deep-research run on the assistant project — a project WITH a candidate
// list — had its decompose step hijacked by handleSelectedWorkflowRoute
// (the lead's delegatedTasks plan carries no selected_workflow → corrective
// retry forced a pick → the lead picked "deep-research" → same-workflow
// loop guard failed the task, 3/3 attempts). issue-fix never hit this
// because its projects define no adaptiveCandidateWorkflows.
func isDelegatorStep(step registry.WorkflowStep) bool {
	return strings.TrimSpace(step.DelegatedWorkflow) != ""
}
