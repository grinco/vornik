package api

import (
	"fmt"
	"sort"
	"strings"

	"vornik.io/vornik/internal/registry"
)

// checkWorkflowOnFailMasking flags steps whose on_fail target is a
// terminal that maps to a *successful* status. That pattern silently
// converts agent failures into "task COMPLETED" outcomes — the
// research.yaml `on_fail: done` shape that motivated this check.
//
// Mechanics:
//   - Every step's on_fail target is followed; if it resolves to a
//     terminal whose status is COMPLETED, that's the masking case.
//   - Other terminals (FAILED, CANCELLED) are fine — those propagate
//     the failure as the operator expects.
//   - A COMPLETED terminal marked `recovery: true` is EXEMPT: reaching
//     it via on_fail is an intentional graceful-recovery exit (e.g.
//     dev-pipeline's checkpoint), not masking.
//   - Steps whose on_fail points at another step are followed
//     transitively (cap depth at the step count to avoid cycles).
//
// Severity is WARNING: the workflow may be intentionally lenient
// (e.g. a "best-effort cleanup" step), so don't block reload — but
// surface it so an operator reviewing the output sees the contract.
func (h *DoctorHandlers) checkWorkflowOnFailMasking() DoctorCheck {
	const name = "workflow_onfail_masking"
	if h.configDir == "" {
		return DoctorCheck{Name: name, Status: "OK", Message: "no config directory configured, skipping"}
	}

	reg := registry.New()
	// config_validation surfaces the underlying error; we still run
	// our check on whatever loaded, same pattern as the other
	// workflow doctor checks.
	_ = reg.Load(h.configDir)

	workflows := reg.ListWorkflows()
	var items []string
	for _, wf := range workflows {
		if wf == nil {
			continue
		}
		items = append(items, scanWorkflowOnFail(wf)...)
	}

	if len(items) == 0 {
		return DoctorCheck{
			Name:    name,
			Status:  "OK",
			Message: fmt.Sprintf("no on_fail-masking findings across %d workflow(s)", len(workflows)),
		}
	}
	sort.Strings(items)
	return DoctorCheck{
		Name:    name,
		Status:  "WARNING",
		Message: fmt.Sprintf("%d step(s) have on_fail pointing at a successful terminal — failures will look like COMPLETED", len(items)),
		Items:   items,
	}
}

// scanWorkflowOnFail walks every step in a workflow and reports any
// whose on_fail target eventually lands at a COMPLETED terminal.
func scanWorkflowOnFail(wf *registry.Workflow) []string {
	var problems []string
	for stepID, step := range wf.Steps {
		if step.OnFail == "" {
			continue
		}
		target, masked := resolveOnFailTerminal(wf, step.OnFail, len(wf.Steps)+1)
		if !masked {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"workflow %q step %q on_fail → %q (terminal status COMPLETED) — failures masked as success",
			wf.ID, stepID, target,
		))
	}
	return problems
}

// resolveOnFailTerminal follows a target through up to maxHops chained
// step transitions and reports whether the final terminal has status
// COMPLETED. Returns (terminalID, true) on a masking detection,
// ("", false) otherwise — including cycles, missing references, and
// terminals with non-success statuses.
func resolveOnFailTerminal(wf *registry.Workflow, target string, maxHops int) (string, bool) {
	cur := target
	for i := 0; i < maxHops; i++ {
		if term, ok := wf.Terminals[cur]; ok {
			// A terminal explicitly marked as an intentional recovery
			// exit is not masking — reaching it via on_fail is by design.
			if term.Recovery {
				return "", false
			}
			return cur, strings.EqualFold(term.Status, "COMPLETED")
		}
		next, ok := wf.Steps[cur]
		if !ok {
			return "", false
		}
		// Step's own on_fail decides where THIS step's failures go.
		// Follow on_success here because we're tracking where a
		// failure-routed step *succeeds* into; if its on_success
		// reaches a COMPLETED terminal, the original on_fail still
		// masks. (A step that succeeds normally after being routed
		// here is the masking case.)
		if next.OnSuccess == "" {
			return "", false
		}
		cur = next.OnSuccess
	}
	return "", false
}
