package api

import (
	"fmt"
	"sort"
	"strings"

	"vornik.io/vornik/internal/registry"
)

// checkWorkflowSwarmCompat reports projects whose swarm cannot satisfy
// the roles of the workflow they'll actually run.
//
// The check is deliberately scoped to each project's DefaultWorkflowID:
//
//   - autonomy create_task now falls back to that default when the LLM
//     omits a workflow_id (see commit 0e7241d), so it's the only
//     workflow a project ever actually dispatches in practice.
//   - An earlier version of this check iterated every project × every
//     registered workflow. On a deployment with 5 workflows and 6
//     projects, that produced 14 WARNING findings for combinations no
//     one had ever attempted to run — pure noise that trained operators
//     to ignore the check entirely.
//
// Severity stays WARNING: a wrong default blocks the project at the
// first tick rather than at request time, but it's still preferable
// to an ERROR that would refuse reload over a latent mismatch.
func (h *DoctorHandlers) checkWorkflowSwarmCompat() DoctorCheck {
	const name = "workflow_swarm_compat"
	if h.configDir == "" {
		return DoctorCheck{Name: name, Status: "OK", Message: "no config directory configured, skipping"}
	}

	reg := registry.New()
	// Same rationale as role_prompt_sanity: config_validation surfaces
	// the underlying error; we still run our check on whatever loaded.
	_ = reg.Load(h.configDir)

	projects := reg.ListProjects()
	swarms := reg.ListSwarms()

	// Build swarm roles map once.
	swarmRoles := make(map[string]map[string]bool, len(swarms))
	for _, sw := range swarms {
		if sw == nil {
			continue
		}
		roles := make(map[string]bool, len(sw.Roles))
		for _, r := range sw.Roles {
			roles[r.Name] = true
		}
		swarmRoles[sw.ID] = roles
	}

	var items []string
	for _, p := range projects {
		if p == nil {
			continue
		}
		if p.DefaultWorkflowID == "" {
			// Baseline registry validator rejects projects with no
			// DefaultWorkflowID at load, so this should never trip.
			// If it does, config_validation reports it; skip here to
			// avoid double-reporting.
			continue
		}
		roles, ok := swarmRoles[p.SwarmID]
		if !ok {
			// Project references a missing swarm. Baseline validator
			// catches this; skip.
			continue
		}
		wf := reg.GetWorkflow(p.DefaultWorkflowID)
		if wf == nil {
			// Missing default workflow — same baseline guard applies.
			continue
		}
		missing := missingRolesForWorkflow(wf, roles)
		if len(missing) == 0 {
			continue
		}
		items = append(items, fmt.Sprintf(
			"project %q (swarm %q) cannot run its default workflow %q: missing role(s) %s",
			p.ID, p.SwarmID, wf.ID, strings.Join(missing, ", "),
		))
	}

	if len(items) == 0 {
		return DoctorCheck{
			Name:    name,
			Status:  "OK",
			Message: fmt.Sprintf("%d project(s) default workflow compatible with configured swarm", len(projects)),
		}
	}
	sort.Strings(items)
	return DoctorCheck{
		Name:    name,
		Status:  "WARNING",
		Message: fmt.Sprintf("%d project(s) cannot run their default workflow in the configured swarm", len(items)),
		Items:   items,
	}
}

// missingRolesForWorkflow returns the set of roles a workflow's agent /
// plan steps require that the swarm's role list doesn't cover. Ordered
// so report lines are stable between doctor runs.
func missingRolesForWorkflow(wf *registry.Workflow, roles map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, step := range wf.Steps {
		if step.Type != "agent" && step.Type != "plan" {
			continue
		}
		if step.Role == "" {
			continue
		}
		if roles[step.Role] {
			continue
		}
		if seen[step.Role] {
			continue
		}
		seen[step.Role] = true
		out = append(out, step.Role)
	}
	sort.Strings(out)
	return out
}
