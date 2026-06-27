package ui

import (
	"bytes"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// TestProjectDetail_HeaderRendersSwarmAndWorkflowLinks — the
// project detail page header must surface direct links to the
// swarm + workflow editors alongside Config / Keys. Buried
// discovery via the config form's Routing section wasn't
// enough — operators couldn't find the editors at all in user
// testing.
func TestProjectDetail_HeaderRendersSwarmAndWorkflowLinks(t *testing.T) {
	s := NewServer()
	data := ProjectDetailData{
		Title:       "Project: demo",
		CurrentPage: "projects",
		Project: &registry.Project{
			ID:                "demo",
			DisplayName:       "Demo",
			SwarmID:           "demo-swarm",
			DefaultWorkflowID: "demo-wf",
		},
		Swarm: &registry.Swarm{ID: "demo-swarm", LeadRole: "lead", Roles: []registry.SwarmRole{
			{Name: "lead", Runtime: registry.SwarmRoleRuntime{Image: "vornik-agent:latest"}},
		}},
	}
	var buf bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buf, "project_detail.html", data); err != nil {
		t.Fatalf("template render failed: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, `href="/ui/swarms/demo-swarm/edit?projectId=demo"`) {
		t.Errorf("header must link to swarm editor with projectId. body excerpt:\n%s", excerptAround(body, "/swarms/", 80))
	}
	if !strings.Contains(body, `href="/ui/workflows/demo-wf/edit?projectId=demo"`) {
		t.Errorf("header must link to workflow editor with projectId. body excerpt:\n%s", excerptAround(body, "/workflows/", 80))
	}
}

// TestProjectDetail_ConfigLinkPointsToFormEditor pins the
// project-detail nav: the "Config" link must route to the
// form-driven editor at /config/form, not the legacy raw-YAML
// editor at /config. The form editor is the friendly default;
// power users still reach the YAML editor via the "Advanced
// YAML" link on the form page. Without this assertion a
// template refactor could silently route operators back to the
// raw textarea — exactly the regression the customer-pain
// report flagged. See web-authoring-ux-design.md Phase 1B.
func TestProjectDetail_ConfigLinkPointsToFormEditor(t *testing.T) {
	s := NewServer()
	data := ProjectDetailData{
		Title:       "Project: demo",
		CurrentPage: "projects",
		Project: &registry.Project{
			ID:          "demo",
			DisplayName: "Demo",
			SwarmID:     "swarm-1",
		},
		Swarm: &registry.Swarm{ID: "swarm-1", LeadRole: "lead", Roles: []registry.SwarmRole{
			{Name: "lead", Runtime: registry.SwarmRoleRuntime{Image: "vornik-agent:latest"}},
		}},
	}
	var buf bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buf, "project_detail.html", data); err != nil {
		t.Fatalf("template render failed: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `href="/ui/projects/demo/config/form"`) {
		t.Errorf("project detail nav must link Config to /config/form. body:\n%s", excerptAround(out, "/config", 80))
	}
}

// excerptAround returns a short window of bytes around the first
// occurrence of needle in haystack so test failures show only
// the relevant chunk of the rendered template rather than the
// whole page.
func excerptAround(haystack, needle string, window int) string {
	i := strings.Index(haystack, needle)
	if i < 0 {
		return "(needle not found in body)"
	}
	start := i - window
	if start < 0 {
		start = 0
	}
	end := i + window
	if end > len(haystack) {
		end = len(haystack)
	}
	return haystack[start:end]
}
