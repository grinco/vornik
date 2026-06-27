package ui

import (
	"bytes"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// TestProjectDetail_ShowsPerRoleTools locks in the "display matches
// enforcement" fix: the project detail page renders each role's actual
// allowedTools (the list the agent container gates on at dispatch time)
// and does NOT render the informational project-level allowedTools
// block, which was previously shown but never enforced.
func TestProjectDetail_ShowsPerRoleTools(t *testing.T) {
	s := NewServer()
	data := ProjectDetailData{
		Title:       "Project: test",
		CurrentPage: "projects",
		Project: &registry.Project{
			ID:      "test",
			SwarmID: "dev-swarm",
			Permissions: registry.ProjectPermissions{
				// Informational — should NOT show up in output
				AllowedTools: []string{"informational_only_tool"},
			},
		},
		Swarm: &registry.Swarm{
			ID:       "dev-swarm",
			LeadRole: "lead",
			Roles: []registry.SwarmRole{
				{
					Name:    "lead",
					Runtime: registry.SwarmRoleRuntime{Image: "vornik-agent:latest"},
					Permissions: registry.SwarmRolePermissions{
						AllowedTools: []string{"file_read", "run_shell"},
					},
				},
				{
					Name:    "coder",
					Runtime: registry.SwarmRoleRuntime{Image: "vornik-agent:latest"},
					Permissions: registry.SwarmRolePermissions{
						AllowedTools: []string{"file_read", "file_write", "file_edit", "git_diff", "test_run"},
					},
				},
				{
					Name:    "no_tools_role",
					Runtime: registry.SwarmRoleRuntime{Image: "vornik-agent:latest"},
					// No AllowedTools — should render the fallback notice
				},
			},
		},
	}

	var buf bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buf, "project_detail.html", data); err != nil {
		t.Fatalf("template render failed: %v", err)
	}
	out := buf.String()

	// Positive — every tool from every role's list shows up
	expected := []string{
		"file_read", "file_write", "file_edit", "git_diff", "test_run",
		"Tools (2)", // lead has 2
		"Tools (5)", // coder has 5
		"No tools configured",
	}
	for _, want := range expected {
		if !strings.Contains(out, want) {
			t.Errorf("expected rendered output to contain %q (not found)", want)
		}
	}

	// Negative — the misleading tool from the project-level block must be absent
	if strings.Contains(out, "informational_only_tool") {
		t.Error("project-level Permissions.AllowedTools leaked into output — " +
			"the display-vs-enforcement drift fix requires that block to be removed")
	}
}
