package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/registry"
)

type uiSessionBackend struct {
	projects []string
}

func (b uiSessionBackend) Name() string { return "session" }
func (b uiSessionBackend) Authenticate(context.Context, auth.Credential) (*auth.Identity, error) {
	return &auth.Identity{
		Projects: b.projects,
		Extra: map[string]any{
			auth.ExtraSessionRole: "user",
		},
	}, nil
}

func sessionUserUIRequest(method, target string, projects []string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "session"})
	// Simulate a real same-origin browser request. Audit A3 made the
	// CSRF gate fail closed for cookie-authed mutating requests that
	// carry no Sec-Fetch-Site / Origin; browsers always send
	// Sec-Fetch-Site on a same-origin form POST, so without this the
	// helper's request would be CSRF-blocked before reaching the
	// handler under test (which is testing authz, not CSRF).
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	var captured *http.Request
	api.AuthMiddleware(api.AuthConfig{
		Enabled:        true,
		SessionBackend: uiSessionBackend{projects: projects},
	})(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = r
	})).ServeHTTP(httptest.NewRecorder(), req)
	return captured
}

// projectsRig writes a minimal projects/swarms/workflows tree with
// two distinct projects so the Projects handler can render them
// deterministically. Registry.Load() cross-validates all three
// resource kinds, so the fixture has to be complete.
func projectsRig(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workflows"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "alpha.yaml"), []byte(`projectId: alpha
displayName: "Alpha Project"
swarmId: dev-swarm
defaultWorkflowId: w1
defaultPriority: 50
maxConcurrentTasks: 1
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "beta.yaml"), []byte(`projectId: beta
displayName: "Beta Project"
swarmId: dev-swarm
defaultWorkflowId: w1
defaultPriority: 50
maxConcurrentTasks: 1
`), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "dev-swarm.md"), []byte(`---
swarmId: dev-swarm
displayName: "Dev Swarm"
leadRole: lead
roles:
  - name: "lead"
    description: "Plans"
    model: "test"
    runtime:
      image: "vornik-agent:latest"
---
`), 0o600))

	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "w1.md"), []byte(`---
workflowId: w1
entrypoint: build
steps:
  build:
    type: agent
    prompt: "do work"
    role: lead
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`), 0o644))

	reg := registry.New()
	require.NoError(t, reg.Load(root))
	return NewServer(WithProjectRegistry(reg))
}

func scopedUIRequest(method, target string, projects []string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	req.Header.Set("Authorization", "Bearer scoped-key")
	var captured *http.Request
	api.AuthMiddleware(api.AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"scoped-key": projects},
	})(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = r
	})).ServeHTTP(httptest.NewRecorder(), req)
	if captured == nil {
		return req
	}
	return captured
}

func TestProjects_NoRegistryRendersEmpty(t *testing.T) {
	srv := NewServer() // no registry
	req := httptest.NewRequest(http.MethodGet, "/ui/projects", nil)
	rec := httptest.NewRecorder()
	srv.Projects(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	// Page header should still render — operator sees the list page
	// shape even when no projects exist.
	assert.Contains(t, rec.Body.String(), "Projects")
}

func TestProjects_RendersProjectList(t *testing.T) {
	srv := projectsRig(t)
	req := httptest.NewRequest(http.MethodGet, "/ui/projects", nil)
	rec := httptest.NewRecorder()
	srv.Projects(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Alpha Project", "DisplayName should render")
	assert.Contains(t, body, "Beta Project")
	// IDs should appear so the operator can click through.
	assert.Contains(t, body, "alpha")
	assert.Contains(t, body, "beta")
}

func TestProjects_FiltersByScopedAPIKey(t *testing.T) {
	srv := projectsRig(t)
	req := scopedUIRequest(http.MethodGet, "/ui/projects", []string{"alpha"})
	rec := httptest.NewRecorder()
	srv.Projects(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Alpha Project")
	assert.NotContains(t, body, "Beta Project")
}

func TestMemory_FiltersBySessionProjects(t *testing.T) {
	srv := projectsRig(t)
	req := sessionUserUIRequest(http.MethodGet, "/ui/memory", []string{"alpha"})
	rec := httptest.NewRecorder()
	srv.Memory(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Alpha Project")
	assert.NotContains(t, rec.Body.String(), "Beta Project")
}

func TestDashboard_SessionUserRedirectsToTasks(t *testing.T) {
	// A project-scoped (RoleUser) session lands on Tasks — their
	// day-to-day surface — not the operator dashboard (whose
	// instance-wide aggregates are gated away) nor the Projects list.
	srv := projectsRig(t)
	req := sessionUserUIRequest(http.MethodGet, "/ui/", []string{"alpha"})
	rec := httptest.NewRecorder()
	srv.Dashboard(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "/ui/tasks", rec.Header().Get("Location"))
}

func TestHandler_SessionUserCannotReachGlobalAuthoring(t *testing.T) {
	srv := projectsRig(t)
	for _, path := range []string{"/swarms/new", "/workflows/w1/edit", "/projects/new", "/assistant/draft", "/audit", "/mcp"} {
		req := sessionUserUIRequest(http.MethodGet, path, []string{"alpha"})
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		require.Equal(t, http.StatusForbidden, rec.Code, "path=%s", path)
	}
}
