package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The full happy-path render test isn't in this file because the
// Server's repo fields are typed against the full persistence
// interfaces (~80 methods total). Exercising those in a unit test
// is too much fake-stub surface; the Builder's behaviour is
// already covered by internal/replay/builder_test.go which uses
// narrow interfaces. This file covers the handler boundary
// behaviour (auth/format error paths) only.

// TestExecutionReplay_503WhenReposMissing checks the unwired
// deployment path renders a clear 503 rather than panicking.
func TestExecutionReplay_503WhenReposMissing(t *testing.T) {
	srv := NewServer() // no repos wired
	req := httptest.NewRequest("GET", "/ui/executions/exec_1/replay", nil)
	rr := httptest.NewRecorder()
	srv.ExecutionReplay(rr, req, "exec_1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "missing repos") {
		t.Errorf("expected 'missing repos' detail in body: %s", rr.Body.String())
	}
}

func TestExecutionReplay_MethodNotAllowed(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest("POST", "/ui/executions/exec_1/replay", nil)
	rr := httptest.NewRecorder()
	srv.ExecutionReplay(rr, req, "exec_1")
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rr.Code)
	}
}

func TestExecutionReplay_EmptyIDIs400(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest("GET", "/ui/executions//replay", nil)
	rr := httptest.NewRecorder()
	srv.ExecutionReplay(rr, req, "")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rr.Code)
	}
}

func TestReplayBuilder_PartiallyWiredReportsMissing(t *testing.T) {
	srv := NewServer()
	_, err := srv.replayBuilder()
	if err == nil {
		t.Fatal("expected error when no repos wired")
	}
	if !strings.Contains(err.Error(), "execution") {
		t.Errorf("expected execution in error: %v", err)
	}
}
