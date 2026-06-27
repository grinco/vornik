package ui

import (
	"reflect"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// buildWorkflowsData powers the Workflows list page (IA completion): global
// registry workflows with step/terminal counts + project usage. Pure builder.

func TestBuildWorkflowsData_RowsSortedWithCountsAndUsage(t *testing.T) {
	workflows := []*registry.Workflow{
		{
			ID:          "research",
			DisplayName: "Research",
			Entrypoint:  "research",
			Steps:       map[string]registry.WorkflowStep{"research": {}, "write": {}, "recover": {}},
			Terminals:   map[string]registry.WorkflowTerminal{"done": {}, "failed": {}},
		},
		{
			ID:         "dev-pipeline", // no DisplayName → label = ID
			Entrypoint: "analyze",
			Steps:      map[string]registry.WorkflowStep{"analyze": {}},
			Terminals:  map[string]registry.WorkflowTerminal{"done": {}},
		},
	}
	usage := projectUsage{byWorkflow: map[string][]string{
		"research":     {"assistant", "janka"},
		"dev-pipeline": {"autocoder"},
	}}

	data := buildWorkflowsData(workflows, usage)

	if data.CurrentPage != "workflows" {
		t.Errorf("CurrentPage = %q; want workflows", data.CurrentPage)
	}
	if len(data.Rows) != 2 {
		t.Fatalf("rows = %d; want 2", len(data.Rows))
	}
	// Case-insensitive label sort: "dev-pipeline" < "Research".
	if data.Rows[0].ID != "dev-pipeline" {
		t.Errorf("first row = %q; want dev-pipeline", data.Rows[0].ID)
	}

	research := data.Rows[1]
	if research.Label != "Research" || research.StepCount != 3 || research.TerminalCount != 2 || research.Entrypoint != "research" {
		t.Errorf("research row = %+v; want label Research, 3 steps, 2 terminals, entry research", research)
	}
	if !reflect.DeepEqual(research.UsedBy, []string{"assistant", "janka"}) {
		t.Errorf("research usedby = %v; want [assistant janka]", research.UsedBy)
	}
}
