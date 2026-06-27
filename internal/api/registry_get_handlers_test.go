// Package api: hermetic tests for the singleton-fetch registry
// endpoints (GetSwarm / GetWorkflow). The existing
// registry_handlers_test.go covers list + project paths; this file
// fills in the per-resource Get paths the backlog flagged as 0%.
package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/registry"
)

// buildPopulatedRegistry mirrors the fixture used by the existing
// list-projects test so the Get tests have something real to hit.
func buildPopulatedRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workflows"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "swarm.md"), []byte(`---
swarmId: swarm-1
displayName: First Swarm
leadRole: worker
roles:
  - name: worker
    runtime:
      image: fake-agent
  - name: reviewer
    runtime:
      image: fake-agent
---
`), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "wf.md"), []byte(`---
workflowId: wf-1
displayName: First Workflow
entrypoint: run
steps:
  run:
    type: agent
    prompt: "do work"
    role: worker
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "project-1.yaml"), []byte(`
projectId: project-1
displayName: Project One
swarmId: swarm-1
defaultWorkflowId: wf-1
defaultPriority: 42
`), 0o644))

	reg := registry.New()
	require.NoError(t, reg.Load(root))
	return reg
}

// --- GetSwarm --------------------------------------------------------

func TestGetSwarm_MethodNotAllowed(t *testing.T) {
	srv := NewServer(WithProjectRegistry(buildPopulatedRegistry(t)))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/swarms/swarm-1", nil)
	rec := httptest.NewRecorder()
	srv.GetSwarm(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d, want 405", rec.Code)
	}
}

func TestGetSwarm_MissingID(t *testing.T) {
	srv := NewServer(WithProjectRegistry(buildPopulatedRegistry(t)))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/swarms/", nil)
	rec := httptest.NewRecorder()
	srv.GetSwarm(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}

func TestGetSwarm_NilRegistry(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/swarms/swarm-1", nil)
	rec := httptest.NewRecorder()
	srv.GetSwarm(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", rec.Code)
	}
}

func TestGetSwarm_NotFound(t *testing.T) {
	srv := NewServer(WithProjectRegistry(buildPopulatedRegistry(t)))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/swarms/missing", nil)
	rec := httptest.NewRecorder()
	srv.GetSwarm(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
}

func TestGetSwarm_Success(t *testing.T) {
	srv := NewServer(WithProjectRegistry(buildPopulatedRegistry(t)))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/swarms/swarm-1", nil)
	rec := httptest.NewRecorder()
	srv.GetSwarm(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "swarm-1") || !strings.Contains(body, "worker") {
		t.Errorf("response missing swarm body: %s", body)
	}
}

// --- GetWorkflow -----------------------------------------------------

func TestGetWorkflow_MethodNotAllowed(t *testing.T) {
	srv := NewServer(WithProjectRegistry(buildPopulatedRegistry(t)))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workflows/wf-1", nil)
	rec := httptest.NewRecorder()
	srv.GetWorkflow(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d, want 405", rec.Code)
	}
}

func TestGetWorkflow_MissingID(t *testing.T) {
	srv := NewServer(WithProjectRegistry(buildPopulatedRegistry(t)))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workflows/", nil)
	rec := httptest.NewRecorder()
	srv.GetWorkflow(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}

func TestGetWorkflow_NilRegistry(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workflows/wf-1", nil)
	rec := httptest.NewRecorder()
	srv.GetWorkflow(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", rec.Code)
	}
}

func TestGetWorkflow_NotFound(t *testing.T) {
	srv := NewServer(WithProjectRegistry(buildPopulatedRegistry(t)))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workflows/missing", nil)
	rec := httptest.NewRecorder()
	srv.GetWorkflow(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
}

func TestGetWorkflow_Success(t *testing.T) {
	srv := NewServer(WithProjectRegistry(buildPopulatedRegistry(t)))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workflows/wf-1", nil)
	rec := httptest.NewRecorder()
	srv.GetWorkflow(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "wf-1") {
		t.Errorf("response missing wf-1: %s", rec.Body.String())
	}
}
