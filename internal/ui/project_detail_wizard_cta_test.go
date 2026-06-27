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

// TestProjectDetail_WizardCTA_AllPrereqsMet — happy path: brief
// is set, autonomy is on, requireApproval is off. The page must
// render the "Generate" button so a single click runs the
// swarm+workflow generation wizard.
func TestProjectDetail_WizardCTA_AllPrereqsMet(t *testing.T) {
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
	if !strings.Contains(body, `action="/ui/projects/demo/wizard"`) {
		t.Errorf("Generate form missing when all prereqs met. excerpt:\n%s", excerptAround(body, "wizard", 200))
	}
	if !strings.Contains(body, "Generate") {
		t.Errorf("Generate button label missing")
	}
}

// TestProjectDetail_WizardCTA_BriefMissing — the wizard tile must
// still render when the brief is missing, but as a guided
// prerequisites view that points to the brief editor.
// Customer-pain: operators couldn't find the wizard until they
// had already done several other things; the tile would just
// disappear silently.
func TestProjectDetail_WizardCTA_BriefMissing(t *testing.T) {
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
	if !strings.Contains(body, "Configuration wizard") {
		t.Errorf("wizard tile heading missing when prereqs unmet. excerpt:\n%s", excerptAround(body, "wizard", 200))
	}
	if !strings.Contains(body, `href="/ui/projects/demo/brief"`) {
		t.Errorf("missing-brief tile must link to the brief editor. excerpt:\n%s", excerptAround(body, "brief", 200))
	}
	// Must NOT render the Generate submit form when prereqs are
	// missing — that would 400/403 on click.
	if strings.Contains(body, `action="/ui/projects/demo/wizard"`) {
		t.Errorf("Generate form present despite missing brief — would fail on submit")
	}
}

// TestProjectDetail_WizardCTA_AutonomyDisabled — same shape but
// for the autonomy gate. Tile must explain the prereq + link to
// the config editor's autonomy section.
func TestProjectDetail_WizardCTA_AutonomyDisabled(t *testing.T) {
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
	if !strings.Contains(body, "Configuration wizard") {
		t.Errorf("wizard tile heading missing")
	}
	if !strings.Contains(body, "autonomy") && !strings.Contains(body, "Autonomy") {
		t.Errorf("missing-autonomy tile should mention the prereq. excerpt:\n%s", excerptAround(body, "wizard", 200))
	}
	if !strings.Contains(body, `href="/ui/projects/demo/config/form"`) {
		t.Errorf("missing-autonomy tile should link to the config form")
	}
}

// TestProjectDetail_WizardCTA_RequireApproval — the third gate.
// Same shape — but the prerequisite text should specifically
// call out that `requireApproval` must be `false`.
func TestProjectDetail_WizardCTA_RequireApproval(t *testing.T) {
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
	if !strings.Contains(body, "Configuration wizard") {
		t.Errorf("wizard tile heading missing")
	}
	if !strings.Contains(body, "requireApproval") && !strings.Contains(body, "approval") {
		t.Errorf("requireApproval gate not explained. excerpt:\n%s", excerptAround(body, "approval", 200))
	}
}
