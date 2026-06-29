package ui

import (
	"bytes"
	"strings"
	"testing"
)

// TestProjectsNewSuccess_LinksToProjectDetail — after a template is
// materialised, the success page must point operators at the new
// project's detail page rather than dumping them on the global
// projects list and hoping they find their freshly-created project.
func TestProjectsNewSuccess_LinksToProjectDetail(t *testing.T) {
	s := NewServer()
	data := ProjectsNewData{
		Title:            "Project created",
		CurrentPage:      "projects",
		CreatedSlug:      "personal-assistant",
		CreatedProjectID: "my-helper",
		CreatedFiles:     []string{"projects/my-helper/project.yaml"},
	}
	var buf bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buf, "projects_new_success.html", data); err != nil {
		t.Fatalf("template render: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, `href="/ui/projects/my-helper"`) {
		t.Errorf("success page must link to the new project's detail page. excerpt:\n%s", excerptAround(body, "my-helper", 120))
	}
	if !strings.Contains(body, "review the project") && !strings.Contains(body, "Review the project") {
		t.Errorf("success page should mention reviewing the project. excerpt:\n%s", excerptAround(body, "review", 120))
	}
}

// TestProjectsNewSuccess_NoProjectIDFallsBackToList — defensive:
// if for some reason the projectId param wasn't captured, the
// success page should still render and link the operator
// somewhere usable (the projects list).
func TestProjectsNewSuccess_NoProjectIDFallsBackToList(t *testing.T) {
	s := NewServer()
	data := ProjectsNewData{
		Title:        "Project created",
		CurrentPage:  "projects",
		CreatedSlug:  "personal-assistant",
		CreatedFiles: []string{"projects/anon/project.yaml"},
		// CreatedProjectID intentionally empty
	}
	var buf bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buf, "projects_new_success.html", data); err != nil {
		t.Fatalf("template render: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, `href="/ui/projects"`) {
		t.Errorf("success page must fall back to projects list when ID unknown")
	}
}
