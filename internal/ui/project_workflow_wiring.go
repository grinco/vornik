package ui

import (
	"strings"

	"vornik.io/vornik/internal/registry"
)

// ProjectWorkflowWiring is the project-detail view of every workflow a project
// is wired to and what triggers each. Built by buildProjectWorkflowWiring from
// the project config; the template renders Triggers as a routing legend and
// Workflows as per-workflow graph panels. See
// https://docs.vornik.io
type ProjectWorkflowWiring struct {
	Triggers  []TriggerWiring
	Workflows []WiredWorkflow
}

// TriggerWiring is one entry-point → workflow edge in the routing legend.
type TriggerWiring struct {
	Label      string
	WorkflowID string
	Unresolved bool // the referenced workflow id does not resolve in the registry
}

// WiredWorkflow is one DISTINCT workflow the project can run, with every
// trigger that routes to it.
type WiredWorkflow struct {
	Workflow      *registry.Workflow // nil when Unresolved
	WorkflowID    string
	TriggerLabels []string
	EditURL       string
	Unresolved    bool
	DefaultOpen   bool // expanded in the UI when the project has <= 3 distinct workflows
}

// defaultOpenWorkflowThreshold is the max distinct-workflow count at which every
// per-workflow graph panel starts expanded; beyond it they start collapsed to
// keep dense projects manageable.
const defaultOpenWorkflowThreshold = 3

// buildProjectWorkflowWiring derives the trigger→workflow map and the distinct
// wired-workflow set from a project's config. getWorkflow resolves a workflow id
// to its definition (nil = unresolved → flagged, not hidden). Pure over its
// inputs; deterministic order (triggers in entry-point precedence, workflows
// first-seen).
func buildProjectWorkflowWiring(p *registry.Project, getWorkflow func(string) *registry.Workflow) ProjectWorkflowWiring {
	var w ProjectWorkflowWiring
	if p == nil {
		return w
	}

	// Accumulate distinct workflows (first-seen order) with their trigger
	// labels, adding a legend entry per (trigger → workflow) edge.
	index := map[string]int{} // workflow id → position in w.Workflows
	add := func(label, workflowID string) {
		workflowID = strings.TrimSpace(workflowID)
		if workflowID == "" {
			return
		}
		wf := getWorkflow(workflowID)
		unresolved := wf == nil
		w.Triggers = append(w.Triggers, TriggerWiring{Label: label, WorkflowID: workflowID, Unresolved: unresolved})
		if i, ok := index[workflowID]; ok {
			w.Workflows[i].TriggerLabels = append(w.Workflows[i].TriggerLabels, label)
			return
		}
		index[workflowID] = len(w.Workflows)
		w.Workflows = append(w.Workflows, WiredWorkflow{
			Workflow:      wf,
			WorkflowID:    workflowID,
			TriggerLabels: []string{label},
			EditURL:       "/ui/workflows/" + workflowID + "/edit?projectId=" + p.ID,
			Unresolved:    unresolved,
		})
	}

	// Entry-point precedence: webhook sources, GitHub-App channel, autonomy,
	// adaptive candidates, then the default/manual path.
	for _, src := range p.Webhooks.Sources {
		name := strings.TrimSpace(src.Name)
		issueLabel := "issue labeled"
		prLabel := "PR / change-request opened"
		if name != "" && len(p.Webhooks.Sources) > 1 {
			issueLabel += " (" + name + ")"
			prLabel += " (" + name + ")"
		}
		add(issueLabel, src.WorkflowID)
		add(prLabel, src.ChangeRequestWorkflowID)
	}
	add("issue-comment reply", p.GitHubApp.ReplyWorkflowID)
	add("PR review", p.GitHubApp.PRReviewWorkflowID)

	autonomyLabel := "autonomy tick"
	if mode := strings.TrimSpace(p.Autonomy.Mode); mode != "" {
		autonomyLabel += " (" + mode + ")"
	}
	add(autonomyLabel, p.Autonomy.WorkflowID)

	for _, id := range p.AdaptiveCandidateWorkflows {
		add("adaptive pick", id)
	}
	add("default / manual submit", p.DefaultWorkflowID)

	open := len(w.Workflows) <= defaultOpenWorkflowThreshold
	for i := range w.Workflows {
		w.Workflows[i].DefaultOpen = open
	}
	return w
}
