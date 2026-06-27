package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/registry"
)

func TestServer_ListProjects_Success(t *testing.T) {
	// Setup test data with temp directory and files
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

	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "wf.md"), []byte(`---
workflowId: wf-1
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

	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "project-2.yaml"), []byte(`
projectId: project-2
displayName: Project Two
swarmId: swarm-1
defaultWorkflowId: wf-1
defaultPriority: 42
autonomy:
  enabled: true
`), 0o644))

	reg := registry.New()
	require.NoError(t, reg.Load(root))

	server := NewServer(WithProjectRegistry(reg))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	rec := httptest.NewRecorder()

	server.ListProjects(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "project-1")
	assert.Contains(t, body, "project-2")
	assert.Contains(t, body, "Project One")
	assert.Contains(t, body, `"autonomyEnabled":true`)
	assert.Contains(t, body, `"total":2`)
}

func TestServer_ListProjects_NilRegistry(t *testing.T) {
	server := NewServer()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	rec := httptest.NewRecorder()

	server.ListProjects(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"projects":[]`)
	assert.Contains(t, rec.Body.String(), `"total":0`)
}

func TestServer_GetProjectConfig_Success(t *testing.T) {
	// Setup test data with temp directory and files
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

	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "wf.md"), []byte(`---
workflowId: wf-1
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
defaultPriority: 50
maxConcurrentTasks: 10
budget:
  dailySoftUSD: 100
autonomy:
  enabled: false
`), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "project-autonomy.yaml"), []byte(`
projectId: project-autonomy
displayName: Autonomy Project
swarmId: swarm-1
defaultWorkflowId: wf-1
defaultPriority: 50
maxConcurrentTasks: 5
autonomy:
  enabled: true
`), 0o644))

	reg := registry.New()
	require.NoError(t, reg.Load(root))

	server := NewServer(WithProjectRegistry(reg))

	// Test first project
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-1/config", nil)
	rec := httptest.NewRecorder()

	server.GetProjectConfig(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "project-1")
	assert.Contains(t, body, `"defaultPriority":50`)
	assert.Contains(t, body, `"maxConcurrentTasks":10`)
	assert.Contains(t, body, `"autonomyEnabled":false`)

	// Test autonomy project
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-autonomy/config", nil)
	rec2 := httptest.NewRecorder()

	server.GetProjectConfig(rec2, req2)

	assert.Equal(t, http.StatusOK, rec2.Code)
	body2 := rec2.Body.String()
	assert.Contains(t, body2, "project-autonomy")
	assert.Contains(t, body2, `"defaultPriority":50`)
	assert.Contains(t, body2, `"maxConcurrentTasks":5`)
	assert.Contains(t, body2, `"autonomyEnabled":true`)
}

func TestServer_GetProjectConfig_MissingID(t *testing.T) {
	server := NewServer(WithProjectRegistry(registry.New()))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects//config", nil)
	rec := httptest.NewRecorder()

	server.GetProjectConfig(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "VALIDATION_ERROR")
}

func TestServer_GetProjectConfig_NilRegistry(t *testing.T) {
	server := NewServer()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-1/config", nil)
	rec := httptest.NewRecorder()

	server.GetProjectConfig(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "REGISTRY_UNAVAILABLE")
}

func TestServer_GetProjectConfig_NotFound(t *testing.T) {
	server := NewServer(WithProjectRegistry(registry.New()))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/nonexistent/config", nil)
	rec := httptest.NewRecorder()

	server.GetProjectConfig(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestServer_ListSwarms_Success(t *testing.T) {
	// Setup test data
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "swarms"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "swarm-1.md"), []byte(`---
swarmId: swarm-1
displayName: Swarm One
leadRole: lead
roles:
  - name: lead
    runtime:
      image: fake-agent
  - name: worker
    runtime:
      image: fake-agent
---
`), 0o644))

	reg := registry.New()
	require.NoError(t, reg.Load(root))

	server := NewServer(WithProjectRegistry(reg))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/swarms", nil)
	rec := httptest.NewRecorder()

	server.ListSwarms(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "swarm-1")
	assert.Contains(t, body, "Swarm One")
	assert.Contains(t, body, `"leadRole":"lead"`)
	assert.Contains(t, body, `"roles":["lead","worker"]`)
	assert.Contains(t, body, `"total":1`)
}

func TestServer_ListSwarms_NilRegistry(t *testing.T) {
	server := NewServer()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/swarms", nil)
	rec := httptest.NewRecorder()

	server.ListSwarms(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"swarms":[]`)
	assert.Contains(t, rec.Body.String(), `"total":0`)
}

func TestServer_ListWorkflows_Success(t *testing.T) {
	// Setup test data
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workflows"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "swarm-1.md"), []byte(`---
swarmId: swarm-1
displayName: Swarm One
roles:
  - name: worker
    runtime:
      image: fake-agent
---
`), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "wf-1.md"), []byte(`---
workflowId: wf-1
displayName: Workflow One
swarmId: swarm-1
entrypoint: step1
steps:
  step1:
    type: agent
    prompt: "do work"
    role: worker
    on_success: step2
  step2:
    type: agent
    prompt: "do work"
    role: worker
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`), 0o644))

	reg := registry.New()
	require.NoError(t, reg.Load(root))

	server := NewServer(WithProjectRegistry(reg))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workflows", nil)
	rec := httptest.NewRecorder()

	server.ListWorkflows(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "wf-1")
	assert.Contains(t, body, "Workflow One")
	assert.Contains(t, body, `"steps":["step1","step2"]`)
	assert.Contains(t, rec.Body.String(), `"total":1`)
}

func TestServer_ListWorkflows_NilRegistry(t *testing.T) {
	server := NewServer()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workflows", nil)
	rec := httptest.NewRecorder()

	server.ListWorkflows(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"workflows":[]`)
	assert.Contains(t, rec.Body.String(), `"total":0`)
}

func TestProjectSummaryFrom(t *testing.T) {
	p := &registry.Project{
		ID:                 "proj-1",
		DisplayName:        "Test Project",
		SwarmID:            "swarm-1",
		DefaultWorkflowID:  "wf-1",
		Autonomy:           registry.ProjectAutonomy{Enabled: true},
		DefaultPriority:    100,
		MaxConcurrentTasks: 5,
		Budget:             registry.ProjectBudget{DailyHardUSD: 50},
	}

	summary := projectSummaryFrom(p)

	assert.Equal(t, "proj-1", summary.ProjectID)
	assert.Equal(t, "Test Project", summary.DisplayName)
	assert.Equal(t, "swarm-1", summary.SwarmID)
	assert.Equal(t, "wf-1", summary.DefaultWorkflowID)
	assert.True(t, summary.AutonomyEnabled)
}

func TestProjectDetailFrom(t *testing.T) {
	p := &registry.Project{
		ID:                 "proj-1",
		DisplayName:        "Test Project",
		SwarmID:            "swarm-1",
		DefaultWorkflowID:  "wf-1",
		Autonomy:           registry.ProjectAutonomy{Enabled: true},
		DefaultPriority:    100,
		MaxConcurrentTasks: 5,
		Budget:             registry.ProjectBudget{DailyHardUSD: 50},
	}

	detail := projectDetailFrom(p)

	assert.Equal(t, "proj-1", detail.ProjectID)
	assert.Equal(t, 100, detail.DefaultPriority)
	assert.Equal(t, 5, detail.MaxConcurrentTasks)
	assert.Equal(t, float64(50), detail.Budget.DailyHardUSD)
	assert.True(t, detail.Autonomy.Enabled)
}

func TestSwarmSummaryFrom(t *testing.T) {
	sw := &registry.Swarm{
		ID:          "swarm-1",
		DisplayName: "Test Swarm",
		LeadRole:    "lead",
		Roles: []registry.SwarmRole{
			{Name: "lead"},
			{Name: "worker"},
			{Name: "reviewer"},
		},
	}

	summary := swarmSummaryFrom(sw)

	assert.Equal(t, "swarm-1", summary.SwarmID)
	assert.Equal(t, "Test Swarm", summary.DisplayName)
	assert.Equal(t, "lead", summary.LeadRole)
	require.Len(t, summary.Roles, 3)
	assert.Equal(t, "lead", summary.Roles[0])
	assert.Equal(t, "worker", summary.Roles[1])
	assert.Equal(t, "reviewer", summary.Roles[2])
}

func TestWorkflowSummaryFrom(t *testing.T) {
	wf := &registry.Workflow{
		ID:          "wf-1",
		DisplayName: "Test Workflow",
		Steps: map[string]registry.WorkflowStep{
			"setup":   {Type: "agent", Role: "worker"},
			"run":     {Type: "agent", Role: "worker"},
			"review":  {Type: "agent", Role: "lead"},
			"cleanup": {Type: "agent", Role: "worker"},
		},
	}

	summary := workflowSummaryFrom(wf)

	assert.Equal(t, "wf-1", summary.WorkflowID)
	assert.Equal(t, "Test Workflow", summary.DisplayName)
	require.Len(t, summary.Steps, 4)
	// Steps should be in some order; just check they're all present
	stepSet := make(map[string]bool)
	for _, step := range summary.Steps {
		stepSet[step] = true
	}
	assert.True(t, stepSet["setup"])
	assert.True(t, stepSet["run"])
	assert.True(t, stepSet["review"])
	assert.True(t, stepSet["cleanup"])
}
