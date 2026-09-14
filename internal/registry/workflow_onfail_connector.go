package registry

import (
	"fmt"
	"sort"
	"strings"
)

// A RECOVERY STEP THAT DEPENDS ON THE THING THAT MAY HAVE BROKEN.
//
// A step named as an `on_fail` target can itself call connector-backed tools.
// If the connector that failed the primary step is the one the recovery step
// needs — or another connector that is also down — the recovery fails
// circularly and routes to its own `on_fail`. That behaviour is CORRECT; the
// gap this closes is that nothing said so at authoring time.
//
// The rule was written down twice — the connector-auth-failure-visibility
// design §7 and the workflow guide — and enforced nowhere. A documented
// behaviour nothing implements is worse than an absent one: it stops the next
// person looking. BACKLOG 2026-08-26, filed out of review-20260825-838a
// finding B.

// onFailConnectorRule is quoted VERBATIM from
// https://docs.vornik.io §7.
//
// Verbatim on purpose. The whole failure mode here is a rule living in prose
// that the code never learned; paraphrasing it into a warning would start the
// same drift one layer down, and an author who reads the warning and then the
// design must find the same sentence.
const onFailConnectorRule = "A step named as an `on_fail` target should not depend on connector-backed " +
	"tools to report failure. The daemon-side operator alert is not an MCP tool " +
	"and is not subject to any connector's auth state."

// connectorToolPrefix marks a tool served by an MCP connector — the class whose
// auth state can be revoked, expire, or 401 independently of the daemon.
const connectorToolPrefix = "mcp__"

// appendOnFailConnectorFindings warns when a step reachable ONLY as an
// `on_fail` target requires a connector-backed tool.
//
// WARNING, never an error. The pattern is legal and sometimes deliberate — a
// recovery step that files a Jira ticket depends on a connector on purpose, and
// the daemon-side operator alert reports the outage regardless of what the step
// manages to do. This is an authoring-ergonomics signal, not a gate.
//
// SCOPED TO STEPS REACHABLE ONLY AS A RECOVERY TARGET. A step that is also on
// the success path is not primarily a reporter, and warning there would fire on
// ordinary re-use of a step — which teaches authors to ignore the code, and a
// warning nobody reads is worse than no warning at all.
func appendOnFailConnectorFindings(report *WorkflowMDValidationReport, wf *Workflow) {
	if wf == nil || len(wf.Steps) == 0 {
		return
	}

	recoveryOnly := recoveryOnlyTargets(wf)
	if len(recoveryOnly) == 0 {
		return
	}

	ids := make([]string, 0, len(recoveryOnly))
	for id := range recoveryOnly {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		step := wf.Steps[id]
		var connectorTools []string
		for _, tool := range step.RequireTools {
			if strings.HasPrefix(strings.TrimSpace(tool), connectorToolPrefix) {
				connectorTools = append(connectorTools, strings.TrimSpace(tool))
			}
		}
		if len(connectorTools) == 0 {
			continue
		}
		sort.Strings(connectorTools)
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityWarning,
			Code:     "on_fail_connector_dependency",
			Field:    fmt.Sprintf("steps.%s.require_tools", id),
			Message: fmt.Sprintf(
				"step %q is reachable only as an `on_fail` recovery target and requires connector-backed tool(s) %s. "+
					"If the connector that failed the primary step is this one — or another that is also down — the "+
					"recovery fails circularly and routes to its own `on_fail`. %s",
				id, strings.Join(connectorTools, ", "), onFailConnectorRule),
			Hint: "Report failure with built-in tools, or rely on the daemon-side operator alert, which reaches the operator whatever this step manages to do.",
		})
	}
}

// recoveryOnlyTargets returns the steps reachable as an `on_fail` target and by
// no other transition — not the entrypoint, no `on_success`, no `on_outcome`.
//
// The distinction is the whole scope of the rule: "also reachable normally"
// means the step is part of the work, and its connector use is not a recovery
// hazard.
func recoveryOnlyTargets(wf *Workflow) map[string]struct{} {
	onFail := map[string]struct{}{}
	otherwise := map[string]struct{}{}
	if wf.Entrypoint != "" {
		otherwise[wf.Entrypoint] = struct{}{}
	}
	for _, step := range wf.Steps {
		if step.OnFail != "" {
			onFail[step.OnFail] = struct{}{}
		}
		if step.OnSuccess != "" {
			otherwise[step.OnSuccess] = struct{}{}
		}
		for _, target := range step.OnOutcome {
			if target != "" {
				otherwise[target] = struct{}{}
			}
		}
	}
	// An entrypoint-less fixture (this validator accepts partial files by
	// design) would make the FIRST step look recovery-only. Steps are a map, so
	// there is no first step to exempt — and a workflow with no entrypoint
	// fails its own validation elsewhere, loudly, which is the right place.
	for id := range onFail {
		if _, ok := otherwise[id]; ok {
			delete(onFail, id)
		}
	}
	return onFail
}
