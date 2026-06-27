package ui

import (
	"bytes"
	"strings"
	"testing"
)

// renderConfigForm renders the project_config_form.html template
// with an empty data payload so the assertions below can scan for
// hint-text strings regardless of project state.
func renderConfigForm(t *testing.T) string {
	t.Helper()
	s := NewServer()
	data := ProjectConfigFormData{
		Title:           "Project Config",
		CurrentPage:     "projects",
		ProjectID:       "demo",
		WorkflowOptions: []string{"adaptive"},
		SwarmOptions:    []string{},
		AutonomyModes:   []string{"event", "cron"},
		TimezoneOptions: []string{"UTC", "America/New_York"},
	}
	var buf bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buf, "project_config_form.html", data); err != nil {
		t.Fatalf("template render: %v", err)
	}
	return buf.String()
}

// TestProjectConfigForm_TooltipsPresent — operators kept asking
// what each field meant. Every field below should render plain-
// English hint text below the input so the form is self-documenting.
// Pin the exact phrases so a future refactor can't silently drop them.
func TestProjectConfigForm_TooltipsPresent(t *testing.T) {
	body := renderConfigForm(t)

	cases := []struct {
		name string
		want string
	}{
		{"maxConcurrentTasks", "running in parallel"},
		{"autonomy_enabled", "autonomous loop"},
		{"autonomy_mode", "event-driven"},
		{"autonomy_maxTasksPerHour", "per hour"},
		{"autonomy_pollInterval", "How often the autonomous loop"},
		{"autonomy_requireApproval", "queued for human approval"},
		{"permissions_secrets", "Environment variable names"},
		{"permissions_allowedTools_custom", "Tick common agent runtime tools above"},
		{"rateLimit_tasksPerMinute", "rolling minute window"},
		{"rateLimit_tasksPerHour", "rolling hour window"},
		{"retention_taskLLMUsageDays", "Days before LLM-usage rows"},
		{"retention_tasksDays", "Days before completed tasks"},
		{"retention_artifactsDays", "artifact blobs"},
	}
	for _, c := range cases {
		if !strings.Contains(body, c.want) {
			t.Errorf("%s: tooltip %q not rendered", c.name, c.want)
		}
	}
}
