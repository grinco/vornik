package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/registry"
)

func buildTwoProjectRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	root := t.TempDir()
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, sub), 0o755))
	}
	for _, id := range []string{"a", "b"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "swarm-"+id+".md"), []byte(`---
swarmId: swarm-`+id+`
roles:
  - name: worker
    runtime:
      image: fake-agent
---

## Role prompts

### worker

secret prompt `+id+`
`), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "wf-"+id+".md"), []byte(`---
workflowId: wf-`+id+`
entrypoint: run
steps:
  run:
    type: agent
    prompt: "do `+id+`"
    role: worker
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "project-"+id+".yaml"), []byte(`
projectId: project-`+id+`
swarmId: swarm-`+id+`
defaultWorkflowId: wf-`+id+`
`), 0o644))
	}
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	return reg
}

func scopedRegistryReq(method, path string, projects ...string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	ctx := context.WithValue(req.Context(), authEnabledKey, true)
	ctx = context.WithValue(ctx, projectIDKey, projects)
	return req.WithContext(ctx)
}

func TestRegistryEndpoints_FilterScopedProjectKey(t *testing.T) {
	srv := NewServer(WithProjectRegistry(buildTwoProjectRegistry(t)))

	rec := httptest.NewRecorder()
	srv.ListProjects(rec, scopedRegistryReq(http.MethodGet, "/api/v1/projects", "project-a"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "project-a") || strings.Contains(rec.Body.String(), "project-b") {
		t.Fatalf("ListProjects leaked foreign project: status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	srv.GetSwarm(rec, scopedRegistryReq(http.MethodGet, "/api/v1/swarms/swarm-b", "project-a"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GetSwarm status=%d, want 403; body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	srv.GetWorkflow(rec, scopedRegistryReq(http.MethodGet, "/api/v1/workflows/wf-b", "project-a"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GetWorkflow status=%d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}
