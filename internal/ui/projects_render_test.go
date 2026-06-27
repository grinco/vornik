// Package ui: render test for the Projects list page. Pure
// template + nil/empty registry path; no DB.
package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProjects_RendersWithoutRegistry(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/projects", nil)
	rec := httptest.NewRecorder()
	srv.Projects(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	// Page renders the title "Projects" — confirms the template
	// found its CurrentPage / Title fields without panicking on
	// the nil slice.
	if !strings.Contains(rec.Body.String(), "Projects") {
		t.Errorf("body missing 'Projects' title: %s", rec.Body.String())
	}
}

func TestProjects_RendersWithRegistry(t *testing.T) {
	reg := buildPopulatedUIRegistry(t)
	srv := NewServer(WithProjectRegistry(reg))
	req := httptest.NewRequest(http.MethodGet, "/ui/projects", nil)
	rec := httptest.NewRecorder()
	srv.Projects(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "project-1") {
		t.Errorf("body missing project-1: %s", body)
	}
}
