package api

// workflow_md_shape doctor check — flags shipped WORKFLOW.md files
// that are missing structural metadata the dashboard, picker, and
// doctor report rely on. Today the only field checked is
// `description`; the check is structured so future required-field
// additions (e.g. tags, category) plug in without a new check.
//
// Rationale: a workflow without a description renders as a bare
// id in every list view, which forces operators to grep the
// Markdown body to remember what each one does. Each shipped
// workflow carries enough intent in its frontmatter for the doctor
// to mechanically verify; this check makes that contract explicit.

import (
	"fmt"
	"sort"
	"strings"

	"vornik.io/vornik/internal/registry"
)

// checkWorkflowMdShape inspects every loaded workflow and reports
// shape issues. Status is ERROR rather than WARNING because the
// fix is trivial (add one line of YAML) and silently accepting a
// missing description leads to a poor dashboard experience
// indefinitely.
func (h *DoctorHandlers) checkWorkflowMdShape() DoctorCheck {
	name := "workflow_md_shape"
	if h.configDir == "" {
		return DoctorCheck{Name: name, Status: "OK", Message: "no config dir; skipping"}
	}
	workflows, err := registry.LoadWorkflows(h.configDir)
	if err != nil {
		return DoctorCheck{Name: name, Status: "ERROR", Message: fmt.Sprintf("load workflows: %v", err)}
	}
	if len(workflows) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: "no workflows configured"}
	}

	// Stable iteration order — doctor output is consumed as text
	// in CI, surprise reordering between runs is noise.
	ids := make([]string, 0, len(workflows))
	for id := range workflows {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var problems []string
	for _, id := range ids {
		wf := workflows[id]
		if wf == nil {
			continue
		}
		if issue := workflowShapeIssue(wf); issue != "" {
			problems = append(problems, fmt.Sprintf("%s: %s", id, issue))
		}
	}
	if len(problems) == 0 {
		return DoctorCheck{
			Name:    name,
			Status:  "OK",
			Message: fmt.Sprintf("all %d workflow(s) carry the required shape", len(workflows)),
		}
	}
	return DoctorCheck{
		Name:    name,
		Status:  "ERROR",
		Message: fmt.Sprintf("%d workflow(s) missing required frontmatter", len(problems)),
		Items:   problems,
	}
}

// workflowShapeIssue returns the human-readable reason a workflow
// fails the shape check, or "" when it passes. Split out so the
// unit test can hit every branch without spinning up the doctor
// handler.
func workflowShapeIssue(wf *registry.Workflow) string {
	if wf == nil {
		return "workflow is nil"
	}
	if strings.TrimSpace(wf.Description) == "" {
		return "missing `description:` field (add a one-paragraph summary to the frontmatter, ≤1024 chars)"
	}
	if len(wf.Description) > registry.WorkflowDescriptionMaxLen {
		return fmt.Sprintf("description is %d chars; cap is %d", len(wf.Description), registry.WorkflowDescriptionMaxLen)
	}
	return ""
}
