package ui

import (
	"net/http"
	"sort"
	"strings"

	"vornik.io/vornik/internal/registry"
)

// WorkflowsData backs the Workflows list page (IA completion). Workflows are
// global registry entities, so all loaded workflows are shown.
type WorkflowsData struct {
	Title       string
	CurrentPage string
	Rows        []WorkflowRow
}

// WorkflowRow is one row in the workflows table.
type WorkflowRow struct {
	ID            string
	Label         string // DisplayName or ID
	Description   string
	StepCount     int
	TerminalCount int
	Entrypoint    string
	UsedBy        []string // project labels with this as DefaultWorkflowID
}

// buildWorkflowsData turns the registry's workflows + project-usage index
// into the sorted view the template renders. Pure.
func buildWorkflowsData(workflows []*registry.Workflow, usage projectUsage) WorkflowsData {
	rows := make([]WorkflowRow, 0, len(workflows))
	for _, wf := range workflows {
		if wf == nil {
			continue
		}
		label := wf.DisplayName
		if label == "" {
			label = wf.ID
		}
		rows = append(rows, WorkflowRow{
			ID:            wf.ID,
			Label:         label,
			Description:   wf.Description,
			StepCount:     len(wf.Steps),
			TerminalCount: len(wf.Terminals),
			Entrypoint:    wf.Entrypoint,
			UsedBy:        usage.byWorkflow[wf.ID],
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return strings.ToLower(rows[i].Label) < strings.ToLower(rows[j].Label)
	})
	return WorkflowsData{Title: "Workflows", CurrentPage: "workflows", Rows: rows}
}

// WorkflowsList renders the Workflows list page.
func (s *Server) WorkflowsList(w http.ResponseWriter, r *http.Request) {
	var workflows []*registry.Workflow
	var usage projectUsage
	if s.projectReg != nil {
		workflows = s.projectReg.ListWorkflows()
		usage = buildProjectUsage(s.projectReg.ListProjects())
	}
	s.render(w, "workflows.html", buildWorkflowsData(workflows, usage))
}
