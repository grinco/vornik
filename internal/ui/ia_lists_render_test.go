package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Render smoke tests for the IA-completion list pages: parse is already
// guaranteed by template.Must at Server construction, but these exercise the
// actual render with representative data so a missing field / func / partial
// reference is caught here rather than in the browser.

func renderIA(t *testing.T, name string, data any) string {
	t.Helper()
	s := NewServer()
	var buf bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buf, name, data); err != nil {
		t.Fatalf("render %s failed: %v", name, err)
	}
	return buf.String()
}

func TestRenderSwarms(t *testing.T) {
	usage := projectUsage{bySwarm: map[string][]string{"dev-swarm": {"autocoder"}}}
	data := buildSwarmsData([]*registry.Swarm{
		{ID: "dev-swarm", DisplayName: "Dev Swarm", LeadRole: "lead", Roles: []registry.SwarmRole{{Name: "lead"}}},
	}, usage)
	out := renderIA(t, "swarms.html", data)
	for _, want := range []string{"Dev Swarm", "autocoder", "/ui/swarms/dev-swarm/edit", "New swarm"} {
		if !strings.Contains(out, want) {
			t.Errorf("swarms.html missing %q", want)
		}
	}
}

func TestRenderSwarms_Empty(t *testing.T) {
	out := renderIA(t, "swarms.html", buildSwarmsData(nil, projectUsage{}))
	if !strings.Contains(out, "No swarms defined") {
		t.Error("empty swarms.html should render the empty state")
	}
}

func TestRenderWorkflows(t *testing.T) {
	usage := projectUsage{byWorkflow: map[string][]string{"research": {"janka"}}}
	data := buildWorkflowsData([]*registry.Workflow{
		{ID: "research", DisplayName: "Research", Entrypoint: "research",
			Steps:     map[string]registry.WorkflowStep{"research": {}},
			Terminals: map[string]registry.WorkflowTerminal{"done": {}}},
	}, usage)
	out := renderIA(t, "workflows.html", data)
	for _, want := range []string{"Research", "janka", "/ui/workflows/research/edit", "New workflow"} {
		if !strings.Contains(out, want) {
			t.Errorf("workflows.html missing %q", want)
		}
	}
}

func TestRenderExecutions(t *testing.T) {
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	step := "research"
	data := buildExecutionsData([]*persistence.Execution{
		{ID: "exec-1", TaskID: "task-1", ProjectID: "janka", WorkflowID: "research",
			Status: persistence.ExecutionStatusRunning, CurrentStepID: &step,
			StartedAt: ptrTime(now.Add(-2 * time.Minute))},
	}, now)
	data.Limit = 20
	data.LimitOptions = PageSizeOptions
	data.Palette = buildExecutionPalette(executionStatusOrder, map[persistence.ExecutionStatus]int64{persistence.ExecutionStatusRunning: 1})
	data.TotalCount = 1
	out := renderIA(t, "executions.html", data)
	for _, want := range []string{"exec-1", "/ui/tasks/task-1", "/ui/executions/exec-1", "Running"} {
		if !strings.Contains(out, want) {
			t.Errorf("executions.html missing %q", want)
		}
	}
}

func TestRenderExecutions_Empty(t *testing.T) {
	out := renderIA(t, "executions.html", buildExecutionsData(nil, time.Now()))
	if !strings.Contains(out, "No executions") {
		t.Error("empty executions.html should render the empty state")
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
