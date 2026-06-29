package ui

import (
	"bytes"
	"strings"
	"testing"
)

// renderConfigFormWith renders project_config_form.html with the supplied
// flags. Keeps each test below focused on the disabled wizard affordance.
func renderConfigFormWith(t *testing.T, data ProjectConfigFormData) string {
	t.Helper()
	s := NewServer()
	// Form needs these to render the broader dropdowns; the test
	// only cares about the wizard banner.
	if data.AutonomyModes == nil {
		data.AutonomyModes = []string{"llm"}
	}
	if data.WorkflowOptions == nil {
		data.WorkflowOptions = []string{"adaptive"}
	}
	if data.TimezoneOptions == nil {
		data.TimezoneOptions = []string{"UTC"}
	}
	data.ProjectID = "demo"
	var buf bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buf, "project_config_form.html", data); err != nil {
		t.Fatalf("template render: %v", err)
	}
	return buf.String()
}

func TestProjectConfigForm_WizardBanner_HiddenWhenReady(t *testing.T) {
	body := renderConfigFormWith(t, ProjectConfigFormData{
		HasBrief:                true,
		AutonomyEnabled:         true,
		AutonomyRequireApproval: false,
	})
	if strings.Contains(body, "Configuration wizard") || strings.Contains(body, `action="/ui/projects/demo/wizard"`) {
		t.Errorf("wizard banner should be hidden while wizard is disabled")
	}
}

func TestProjectConfigForm_WizardBanner_HiddenWhenMissingPrereqs(t *testing.T) {
	body := renderConfigFormWith(t, ProjectConfigFormData{
		HasBrief:                false,
		AutonomyEnabled:         false,
		AutonomyRequireApproval: false,
	})
	if strings.Contains(body, "Configuration wizard") || strings.Contains(body, `action="/ui/projects/demo/wizard"`) {
		t.Errorf("wizard banner should stay hidden when prereqs are missing")
	}
}

func TestProjectConfigForm_WizardBanner_HiddenWhenRequireApproval(t *testing.T) {
	body := renderConfigFormWith(t, ProjectConfigFormData{
		HasBrief:                true,
		AutonomyEnabled:         true,
		AutonomyRequireApproval: true,
	})
	if strings.Contains(body, "Configuration wizard") || strings.Contains(body, `action="/ui/projects/demo/wizard"`) {
		t.Errorf("wizard banner should stay hidden when requireApproval is enabled")
	}
}
