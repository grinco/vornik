// Tests for the 2026.7.0 F9 retention panel. Anchors the resolver
// precedence (project-override > daemon-default > hardcoded
// fallback), the IsOverride flag's semantics, and the template's
// rendering of override badges.

package ui

import (
	"bytes"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// TestBuildRetentionPanel_AllDefaults — when neither the project
// nor the daemon defaults set any value, the panel falls back to
// the hardcoded 90/30/60/60/60 days and every IsOverride flag is
// false.
func TestBuildRetentionPanel_AllDefaults(t *testing.T) {
	project := &registry.Project{ID: "p"}
	got := buildRetentionPanel(project, registry.ProjectRetention{})
	if got.TaskLLMUsageDays != 90 {
		t.Errorf("TaskLLMUsageDays = %d, want 90 fallback", got.TaskLLMUsageDays)
	}
	if got.ToolAuditDays != 30 {
		t.Errorf("ToolAuditDays = %d, want 30 fallback", got.ToolAuditDays)
	}
	if got.TasksDays != 60 || got.ExecutionsDays != 60 || got.ArtifactsDays != 60 {
		t.Errorf("Tasks/Executions/Artifacts: got %d/%d/%d, want 60/60/60",
			got.TasksDays, got.ExecutionsDays, got.ArtifactsDays)
	}
	for _, isOverride := range []bool{
		got.TaskLLMUsageIsOverride, got.ToolAuditIsOverride,
		got.TasksIsOverride, got.ExecutionsIsOverride, got.ArtifactsIsOverride,
	} {
		if isOverride {
			t.Error("hardcoded fallback values must NOT be flagged as overrides")
		}
	}
}

// TestBuildRetentionPanel_DaemonDefaultsApply — daemon-wide
// defaults flow through when the project YAML doesn't override.
// IsOverride stays false because the project YAML didn't carry
// the value.
func TestBuildRetentionPanel_DaemonDefaultsApply(t *testing.T) {
	project := &registry.Project{ID: "p"}
	defaults := registry.ProjectRetention{
		TaskLLMUsageDays: 120,
		ToolAuditDays:    7,
		TasksDays:        45,
		ExecutionsDays:   45,
		ArtifactsDays:    45,
	}
	got := buildRetentionPanel(project, defaults)
	if got.TaskLLMUsageDays != 120 {
		t.Errorf("TaskLLMUsageDays = %d, want 120 (daemon default)", got.TaskLLMUsageDays)
	}
	if got.TaskLLMUsageIsOverride {
		t.Error("daemon-default value must NOT be flagged as override")
	}
}

// TestBuildRetentionPanel_ProjectOverridesWin — project YAML wins
// over daemon defaults, AND the IsOverride flag fires.
func TestBuildRetentionPanel_ProjectOverridesWin(t *testing.T) {
	project := &registry.Project{
		ID: "p",
		Retention: registry.ProjectRetention{
			TaskLLMUsageDays: 365,
			ArtifactsDays:    14,
		},
	}
	defaults := registry.ProjectRetention{
		TaskLLMUsageDays: 120,
		ArtifactsDays:    45,
	}
	got := buildRetentionPanel(project, defaults)
	if got.TaskLLMUsageDays != 365 {
		t.Errorf("TaskLLMUsageDays = %d, want 365 (project override)", got.TaskLLMUsageDays)
	}
	if !got.TaskLLMUsageIsOverride {
		t.Error("project-override value MUST be flagged as override")
	}
	if got.ArtifactsDays != 14 {
		t.Errorf("ArtifactsDays = %d, want 14 (project override)", got.ArtifactsDays)
	}
	if !got.ArtifactsIsOverride {
		t.Error("ArtifactsDays project-override must be flagged")
	}
	// Unset fields fall through to daemon defaults, NOT flagged.
	if got.ToolAuditIsOverride {
		t.Error("ToolAuditDays wasn't set in project YAML — must not be flagged")
	}
}

// TestBuildRetentionPanel_NilProjectIsSafe — defensive: nil
// project pointer returns hardcoded fallbacks rather than
// panicking.
func TestBuildRetentionPanel_NilProjectIsSafe(t *testing.T) {
	got := buildRetentionPanel(nil, registry.ProjectRetention{})
	if got.TaskLLMUsageDays != 90 {
		t.Errorf("nil project: got TaskLLMUsageDays=%d, want 90 fallback", got.TaskLLMUsageDays)
	}
}

// TestProjectDetail_RenderRetentionPanelOverrideBadge — the
// template renders an "override" badge on fields the operator
// has tuned, and omits the badge on inherited defaults.
func TestProjectDetail_RenderRetentionPanelOverrideBadge(t *testing.T) {
	s := NewServer()
	data := ProjectDetailData{
		Title:       "Project: demo",
		CurrentPage: "projects",
		Project:     &registry.Project{ID: "demo", SwarmID: "swarm-1"},
		Swarm: &registry.Swarm{ID: "swarm-1", LeadRole: "lead", Roles: []registry.SwarmRole{
			{Name: "lead", Runtime: registry.SwarmRoleRuntime{Image: "vornik-agent:latest"}},
		}},
		Retention: RetentionPanel{
			TaskLLMUsageDays:       365,
			TaskLLMUsageIsOverride: true,
			ToolAuditDays:          30,
			ToolAuditIsOverride:    false,
			TasksDays:              60,
			ExecutionsDays:         60,
			ArtifactsDays:          14,
			ArtifactsIsOverride:    true,
		},
	}
	var buf bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buf, "project_detail.html", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	// Panel renders.
	if !strings.Contains(out, "Retention") {
		t.Fatal("retention panel header missing")
	}
	for _, want := range []string{"365d", "30d", "60d", "14d"} {
		if !strings.Contains(out, want) {
			t.Errorf("retention value %q missing", want)
		}
	}
	// Override badge count: TaskLLMUsage + Artifacts = 2.
	if got := strings.Count(out, ">override<"); got != 2 {
		t.Errorf("expected 2 override badges (LLM cost + Artifacts), got %d", got)
	}
}

// TestWithRetentionDefaults_OptionPlumbedThrough — locks in the
// server-option wiring: WithRetentionDefaults stores the value on
// the server so ProjectDetail can pass it to buildRetentionPanel.
// Without this test the option compiles but a silent typo (wrong
// field assignment) would surface only on a UI smoke test.
func TestWithRetentionDefaults_OptionPlumbedThrough(t *testing.T) {
	defaults := registry.ProjectRetention{TaskLLMUsageDays: 365}
	s := NewServer(WithRetentionDefaults(defaults))
	if s.retentionDefaults.TaskLLMUsageDays != 365 {
		t.Errorf("retentionDefaults.TaskLLMUsageDays = %d, want 365",
			s.retentionDefaults.TaskLLMUsageDays)
	}
}

// TestProjectDetail_RetentionPanelHiddenWhenZero — defensive: if
// the panel struct's TaskLLMUsageDays is zero (no defaults wired,
// no project override, no fallback ran), the template skips the
// whole panel rather than rendering a row of "0d" fields.
func TestProjectDetail_RetentionPanelHiddenWhenZero(t *testing.T) {
	s := NewServer()
	data := ProjectDetailData{
		Title:       "Project: demo",
		CurrentPage: "projects",
		Project:     &registry.Project{ID: "demo", SwarmID: "swarm-1"},
		Swarm: &registry.Swarm{ID: "swarm-1", LeadRole: "lead", Roles: []registry.SwarmRole{
			{Name: "lead", Runtime: registry.SwarmRoleRuntime{Image: "vornik-agent:latest"}},
		}},
		// Retention left at zero value.
	}
	var buf bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buf, "project_detail.html", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(buf.String(), "Retention") {
		t.Error("retention panel must be hidden when zero — operator sees no sad-empty card")
	}
}
