package executor

// Bridge from internal/counterfactual.Payload to the executor's
// per-step option struct. Phase C v1 wrote model/prompt overrides
// onto the new task's payload but the executor didn't read them;
// these helpers close that gap so `vornikctl blackbox replay
// --variable {model,prompt}` actually affects the new run.
//
// Both helpers are no-ops for non-counterfactual tasks
// (ExtractPayload returns the zero value on payloads
// without a `context.counterfactual` block), so call sites read
// unconditionally.

import (
	"vornik.io/vornik/internal/counterfactual"
	"vornik.io/vornik/internal/persistence"
)

// applyCounterfactualPromptOverride replaces opts.SystemPrompt
// when the task carries a per-role prompt override. Called after
// the role config's SystemPrompt has been written so the override
// wins.
func applyCounterfactualPromptOverride(opts *agentInputOpts, task *persistence.Task, role string) {
	if opts == nil || task == nil {
		return
	}
	if v := counterfactual.ExtractPayload(task.Payload).ResolvePrompt(role); v != "" {
		opts.SystemPrompt = v
	}
}
