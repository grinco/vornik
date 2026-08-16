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
	"vornik.io/vornik/internal/telemetryclient"
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
	// A server with no build version wired reports the fallback — all an
	// unidentifiable build can honestly claim.
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

// Regression, 2026-08-15. GetCapabilities reported the version.Default CONSTANT
// to every client regardless of what the daemon was actually running, so a
// companion plugin gating on server version saw the same ancient number from
// every deployment. The only version on the Server was named telemetryVersion,
// which read as belonging to telemetry rather than being the build version.
func TestGetCapabilities_ReportsTheDaemonsRealBuildVersion(t *testing.T) {
	server := NewServer()
	server.buildVersion = "2026.8.4"

	req := httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
	rec := httptest.NewRecorder()
	server.GetCapabilities(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeCapabilities(t, rec.Body.Bytes())
	assert.Equal(t, "2026.8.4", resp.Version,
		"capabilities must report the running build, not a compile-time constant")
	assert.NotEqual(t, version.Default, resp.Version)
}

// Regression, 2026-08-15. The daemon builds the API server's options BEFORE
// container.SetVersion runs, so WithLifecycleTelemetry's eagerly-evaluated
// c.Version() captured "" and /api/v1/capabilities reported an empty version
// on every real daemon — which is what made the endpoint look like it served a
// hardcoded constant. internal/ui had already hit and solved this with a lazy
// WithVersionFunc; the API side had not.
//
// The test models the real ordering: wire the option first, set the version
// after, then serve.
func TestGetCapabilities_LazyBuildVersionSurvivesSetVersionOrdering(t *testing.T) {
	daemonVersion := "" // not yet known when the server is constructed
	server := NewServer(WithBuildVersionFunc(func() string { return daemonVersion }))

	// ... container.SetVersion happens here, after the server exists.
	daemonVersion = "2026.8.4-6-g4cff7746"

	req := httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
	rec := httptest.NewRecorder()
	server.GetCapabilities(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeCapabilities(t, rec.Body.Bytes())
	assert.Equal(t, "2026.8.4-6-g4cff7746", resp.Version,
		"the version must be read at request time, not at option-build time")
}

// The eager setter still works for callers that genuinely have a version, and
// an empty eager value must not clobber a good lazy one.
func TestServer_BuildVersion_PrefersLazyAndIgnoresEmptyEager(t *testing.T) {
	s := NewServer(
		WithLifecycleTelemetry(telemetryclient.Client{}, ""), // empty eager — the daemon's real case
		WithBuildVersionFunc(func() string { return "2026.8.4" }),
	)
	assert.Equal(t, "2026.8.4", s.BuildVersion())

	// No lazy source wired: fall back to whatever was set eagerly.
	s2 := NewServer(WithLifecycleTelemetry(telemetryclient.Client{}, "2026.7.1"))
	assert.Equal(t, "2026.7.1", s2.BuildVersion())

	// Neither wired: empty, and the caller decides what to report.
	assert.Empty(t, NewServer().BuildVersion())
}
