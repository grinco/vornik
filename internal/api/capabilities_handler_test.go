package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/version"
)

// seedRegistry writes a minimal projects/swarms/workflows tree into a
// temp dir and returns a loaded registry. Two projects ("alpha",
// "beta") so scope-filter tests have something to assert against;
// three workflows (wf-alpha, wf-beta, wf-artifacts — the last with
// require_input_artifacts for the delegate-guard tests).
func seedRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workflows"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "swarm.md"), []byte(`---
swarmId: swarm-1
roles:
  - name: worker
    runtime:
      image: fake-agent
---
`), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "wf-alpha.md"), []byte(`---
workflowId: wf-alpha
entrypoint: run
steps:
  run:
    type: agent
    prompt: "alpha work"
    role: worker
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "wf-beta.md"), []byte(`---
workflowId: wf-beta
entrypoint: run
steps:
  run:
    type: agent
    prompt: "beta work"
    role: worker
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "wf-artifacts.md"), []byte(`---
workflowId: wf-artifacts
entrypoint: run
require_input_artifacts: true
steps:
  run:
    type: agent
    prompt: "ingest the staged files"
    role: worker
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "alpha.yaml"), []byte(`
projectId: alpha
displayName: Alpha
swarmId: swarm-1
defaultWorkflowId: wf-alpha
defaultPriority: 50
`), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "beta.yaml"), []byte(`
projectId: beta
displayName: Beta
swarmId: swarm-1
defaultWorkflowId: wf-beta
defaultPriority: 50
`), 0o644))

	reg := registry.New()
	require.NoError(t, reg.Load(root))
	return reg
}

func decodeCapabilities(t *testing.T, body []byte) CapabilitiesResponse {
	t.Helper()
	var resp CapabilitiesResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	return resp
}

func TestGetCapabilities_RejectsNonGET(t *testing.T) {
	server := NewServer()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/capabilities", nil)
	rec := httptest.NewRecorder()

	server.GetCapabilities(rec, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestGetCapabilities_NilRegistry(t *testing.T) {
	server := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
	rec := httptest.NewRecorder()

	server.GetCapabilities(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeCapabilities(t, rec.Body.Bytes())
	assert.Equal(t, version.Default, resp.Version)
	assert.Equal(t, "v1", resp.APIVersion)
	assert.ElementsMatch(t, []string{"http", "sse"}, resp.Transports)
	// AllowedProjects/Workflows are nil (not just empty) when there's
	// no registry — distinguishes "registry not loaded" from "loaded
	// but you can't see anything", which a phase-2 client may want
	// to surface as different error states.
	assert.Nil(t, resp.AllowedProjects)
	assert.Nil(t, resp.AllowedWorkflows)
	// Feature flags always present, companion-v1 starts false until
	// the rest of bundle 1 lands.
	assert.True(t, resp.Features["tasks-v1"])
	assert.True(t, resp.Features["sse-events"])
	assert.False(t, resp.Features["companion-v1"])
	// ServerTime within 5s of test wall clock (cheap clock-skew guard).
	assert.WithinDuration(t, time.Now().UTC(), resp.ServerTime, 5*time.Second)
}

func TestGetCapabilities_AuthDisabled_ReturnsAllProjectsAndWorkflows(t *testing.T) {
	reg := seedRegistry(t)
	server := NewServer(WithProjectRegistry(reg))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
	ctx := context.WithValue(req.Context(), authEnabledKey, false)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	server.GetCapabilities(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeCapabilities(t, rec.Body.Bytes())
	assert.Len(t, resp.AllowedProjects, 2, "auth-disabled key should see every project")
	assert.Len(t, resp.AllowedWorkflows, 3)
}

func TestGetCapabilities_AuthEnabled_ScopedKey_FiltersToOwnedProjectAndItsWorkflow(t *testing.T) {
	reg := seedRegistry(t)
	server := NewServer(WithProjectRegistry(reg))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
	ctx := context.WithValue(req.Context(), authEnabledKey, true)
	ctx = context.WithValue(ctx, projectIDKey, []string{"alpha"})
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	server.GetCapabilities(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeCapabilities(t, rec.Body.Bytes())
	require.Len(t, resp.AllowedProjects, 1, "scoped key should only see its own project")
	assert.Equal(t, "alpha", resp.AllowedProjects[0].ProjectID)

	// visibleWorkflowIDs walks the scoped project's DefaultWorkflowID
	// to project workflow visibility. wf-alpha is alpha's default;
	// wf-beta must NOT leak across the scope boundary.
	require.Len(t, resp.AllowedWorkflows, 1)
	assert.Equal(t, "wf-alpha", resp.AllowedWorkflows[0].WorkflowID)
}

func TestGetCapabilities_FeatureFlagsContractStable(t *testing.T) {
	// Compile-time guard: every flag the plugin will pin behaviour on
	// must be present in the map even when the underlying feature is
	// disabled. Renaming a key here is a contract break — add new
	// keys, never rename.
	server := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
	rec := httptest.NewRecorder()
	server.GetCapabilities(rec, req)
	resp := decodeCapabilities(t, rec.Body.Bytes())

	required := []string{
		"tasks-v1",
		"sse-events",
		"registry-introspection",
		"project-templates",
		"webhooks",
		"companion-v1",
		"companion-mcp",
		"a2a-inbound",
	}
	for _, key := range required {
		_, ok := resp.Features[key]
		assert.Truef(t, ok, "feature flag %q must be present in capabilities response", key)
	}
}
