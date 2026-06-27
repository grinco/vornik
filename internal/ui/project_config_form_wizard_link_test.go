package ui

import (
	"bytes"
	"strings"
	"testing"
)

// renderConfigFormWith renders project_config_form.html with the
// supplied wizard-prereq flags. Keeps each test below focused on
// one branch of the wizard banner.
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

// TestProjectConfigForm_WizardBanner_ShowsRunWhenReady — when
// every wizard prerequisite is satisfied, the config form must
// surface a prominent "Run wizard" CTA so operators arriving via
// the Config link don't have to navigate back to the project
// detail page to find it. Customer complaint: "i still cant find
// the configuration wizard in the ui. is it under projects > name
// > config?"
func TestProjectConfigForm_WizardBanner_ShowsRunWhenReady(t *testing.T) {
	body := renderConfigFormWith(t, ProjectConfigFormData{
		HasBrief:                true,
		AutonomyEnabled:         true,
		AutonomyRequireApproval: false,
	})
	if !strings.Contains(body, "Configuration wizard") {
		t.Errorf("wizard banner heading missing when prereqs met. excerpt:\n%s", excerptAround(body, "wizard", 200))
	}
	if !strings.Contains(body, `action="/ui/projects/demo/wizard"`) {
		t.Errorf("Run-wizard form action missing when prereqs met")
	}
}

// TestProjectConfigForm_WizardBanner_ShowsPrereqsWhenMissing —
// without a brief, the config form must show a friendly banner
// explaining what's needed instead of silently hiding the wizard.
func TestProjectConfigForm_WizardBanner_ShowsPrereqsWhenMissing(t *testing.T) {
	body := renderConfigFormWith(t, ProjectConfigFormData{
		HasBrief:                false,
		AutonomyEnabled:         false,
		AutonomyRequireApproval: false,
	})
	if !strings.Contains(body, "Configuration wizard") {
		t.Errorf("wizard banner heading missing on prereqs-unmet path. excerpt:\n%s", excerptAround(body, "wizard", 200))
	}
	// Form action must NOT be present — clicking would 400.
	if strings.Contains(body, `action="/ui/projects/demo/wizard"`) {
		t.Errorf("Run-wizard form rendered despite missing brief")
	}
	// Must guide the operator toward fixing the prereqs.
	if !strings.Contains(body, "brief") {
		t.Errorf("prereq guidance doesn't mention 'brief'")
	}
}

// TestProjectConfigForm_WizardBanner_RequireApprovalCallout —
// the requireApproval gate is the most surprising one; make sure
// the banner names it explicitly when it's the blocker.
func TestProjectConfigForm_WizardBanner_RequireApprovalCallout(t *testing.T) {
	body := renderConfigFormWith(t, ProjectConfigFormData{
		HasBrief:                true,
		AutonomyEnabled:         true,
		AutonomyRequireApproval: true,
	})
	if !strings.Contains(body, "Configuration wizard") {
		t.Errorf("wizard banner heading missing on requireApproval path")
	}
	if !strings.Contains(body, "requireApproval") && !strings.Contains(body, "approval") {
		t.Errorf("requireApproval gate not explained on config form banner")
	}
}
