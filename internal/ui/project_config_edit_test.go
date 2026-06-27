// Package ui: tests for ProjectConfigEdit. Uses a temp directory
// + real registry to exercise the read-file branch of
// projectConfigData (the render path); ProjectConfigSave already
// has its own tests in project_config_test.go.
package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProjectConfigEdit_RendersExistingProject(t *testing.T) {
	reg := buildPopulatedUIRegistry(t)
	srv := NewServer(WithProjectRegistry(reg))
	req := httptest.NewRequest(http.MethodGet, "/ui/projects/project-1/config", nil)
	rec := httptest.NewRecorder()
	srv.ProjectConfigEdit(rec, req, "project-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "project-1") {
		t.Errorf("body missing project id: %s", rec.Body.String())
	}
}

func TestProjectConfigEdit_NotFound(t *testing.T) {
	reg := buildPopulatedUIRegistry(t)
	srv := NewServer(WithProjectRegistry(reg))
	req := httptest.NewRequest(http.MethodGet, "/ui/projects/missing-project/config", nil)
	rec := httptest.NewRecorder()
	srv.ProjectConfigEdit(rec, req, "missing-project")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
}

func TestProjectConfigEdit_InvalidID(t *testing.T) {
	srv := NewServer(WithProjectRegistry(buildPopulatedUIRegistry(t)))
	cases := []string{"", "../escape", "with/slash"}
	for _, id := range cases {
		t.Run(id, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/ui/projects/"+id+"/config", nil)
			rec := httptest.NewRecorder()
			srv.ProjectConfigEdit(rec, req, id)
			if rec.Code != http.StatusNotFound {
				t.Errorf("id=%q: status got %d, want 404", id, rec.Code)
			}
		})
	}
}

func TestProjectConfigEdit_NoRegistry(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/projects/p1/config", nil)
	rec := httptest.NewRecorder()
	srv.ProjectConfigEdit(rec, req, "p1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d", rec.Code)
	}
}
