package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The persistent "restart required" banner must appear on rendered pages
// once a config edit is saved-but-not-applied, and stay absent otherwise.
// See https://docs.vornik.io

const restartBannerMarker = "restart the daemon to apply"

func TestRestartBanner_AbsentWhenNoRestartPending(t *testing.T) {
	srv := NewServer(WithProjectRegistry(buildPopulatedUIRegistry(t)))
	req := httptest.NewRequest(http.MethodGet, "/ui/projects/project-1/config", nil)
	rec := httptest.NewRecorder()
	srv.ProjectConfigEdit(rec, req, "project-1")

	if strings.Contains(rec.Body.String(), restartBannerMarker) {
		t.Fatal("banner should be absent when no restart is pending")
	}
}

func TestRestartBanner_ShownWhenRestartPending(t *testing.T) {
	srv := NewServer(WithProjectRegistry(buildPopulatedUIRegistry(t)))
	srv.markRestartPending("project project-1 config")

	req := httptest.NewRequest(http.MethodGet, "/ui/projects/project-1/config", nil)
	rec := httptest.NewRecorder()
	srv.ProjectConfigEdit(rec, req, "project-1")

	if !strings.Contains(rec.Body.String(), restartBannerMarker) {
		t.Fatalf("banner should appear once a restart is pending; body:\n%s", rec.Body.String())
	}
}
