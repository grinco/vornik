package ui

import (
	"bytes"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// renderProjectDetail is a tiny helper so each wizard-CTA test
// stays focused on its assertion rather than template-render
// scaffolding. Returns the rendered body or t.Fatal on error.
func renderProjectDetail(t *testing.T, data ProjectDetailData) string {
	t.Helper()
	s := NewServer()
	var buf bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buf, "project_detail.html", data); err != nil {
		t.Fatalf("template render failed: %v", err)
	}
	return buf.String()
}

// TestProjectDetail_WizardCTA_Disabled verifies the project detail page
// does not advertise the legacy project configuration wizard. That path is
// intentionally hidden while its LLM-backed generation flow is unreliable.
func TestProjectDetail_WizardCTA_Disabled(t *testing.T) {
	data := ProjectDetailData{
		Title:       "Project: demo",
		CurrentPage: "projects",
		Project: &registry.Project{
			ID:    "demo",
			Brief: &registry.ProjectBrief{Goal: "build a thing"},
			Autonomy: registry.ProjectAutonomy{
				Enabled:         true,
				RequireApproval: false,
			},
		},
	}
	body := renderProjectDetail(t, data)
	if strings.Contains(body, `action="/ui/projects/demo/wizard"`) || strings.Contains(body, "Configuration wizard") {
		t.Errorf("project wizard CTA should be hidden. excerpt:\n%s", excerptAround(body, "wizard", 200))
	}
}

func TestProjectDetail_WizardCTA_BriefMissingStillHidden(t *testing.T) {
	data := ProjectDetailData{
		Title:       "Project: demo",
		CurrentPage: "projects",
		Project: &registry.Project{
			ID:       "demo",
			Brief:    nil, // missing
			Autonomy: registry.ProjectAutonomy{Enabled: true},
		},
	}
	body := renderProjectDetail(t, data)
	if strings.Contains(body, "Configuration wizard") || strings.Contains(body, `action="/ui/projects/demo/wizard"`) {
		t.Errorf("wizard tile should stay hidden when brief is missing")
	}
}

func TestProjectDetail_WizardCTA_AutonomyDisabledStillHidden(t *testing.T) {
	data := ProjectDetailData{
		Title:       "Project: demo",
		CurrentPage: "projects",
		Project: &registry.Project{
			ID:       "demo",
			Brief:    &registry.ProjectBrief{Goal: "build"},
			Autonomy: registry.ProjectAutonomy{Enabled: false},
		},
	}
	body := renderProjectDetail(t, data)
	if strings.Contains(body, "Configuration wizard") || strings.Contains(body, `action="/ui/projects/demo/wizard"`) {
		t.Errorf("wizard tile should stay hidden when autonomy is disabled")
	}
}

func TestProjectDetail_WizardCTA_RequireApprovalStillHidden(t *testing.T) {
	data := ProjectDetailData{
		Title:       "Project: demo",
		CurrentPage: "projects",
		Project: &registry.Project{
			ID:    "demo",
			Brief: &registry.ProjectBrief{Goal: "build"},
			Autonomy: registry.ProjectAutonomy{
				Enabled:         true,
				RequireApproval: true,
			},
		},
	}
	body := renderProjectDetail(t, data)
	if strings.Contains(body, "Configuration wizard") || strings.Contains(body, `action="/ui/projects/demo/wizard"`) {
		t.Errorf("wizard tile should stay hidden when requireApproval is enabled")
	}
}
