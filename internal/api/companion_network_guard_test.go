package api

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/taskcreate"
)

// ---- fixture -------------------------------------------------------
//
// A registry whose swarm carries three roles that make the derivation
// observable end to end:
//
//   - inert-analyst   — exactly the tool set the real companion
//     `analyst` role holds (the 2026-09-11 fixture): all inert.
//   - shell-worker    — carries run_shell, which is NOT on the inert
//     allow-list, so its workflow stays possibly-capable.
//   - open-worker     — declares no allowedTools at all. Empty means
//     UNRESTRICTED in this codebase (buildAgentInput / roleToolAllowlist),
//     so it must read as possibly-capable, never as inert.
func seedNetworkGuardRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"projects", "swarms", "workflows"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, d), 0o755))
	}

	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "swarm.md"), []byte(`---
swarmId: swarm-net
roles:
  - name: inert-analyst
    runtime:
      image: test-image
    permissions:
      allowedTools:
        - "current_time"
        - "file_read"
        - "file_write"
        - "read_many_files"
        - "grep"
        - "glob"
        - "memory_search"
  - name: shell-worker
    runtime:
      image: test-image
    permissions:
      allowedTools:
        - "file_read"
        - "run_shell"
  - name: open-worker
    runtime:
      image: test-image
---
`), 0o644))

	writeWF := func(id, role string) {
		require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", id+".md"), []byte(`---
workflowId: `+id+`
displayName: `+id+`
description: "fixture workflow"
entrypoint: run
steps:
  run:
    type: agent
    prompt: "work"
    role: `+role+`
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`), 0o644))
	}
	writeWF("wf-inert", "inert-analyst")
	writeWF("wf-capable", "shell-worker")
	writeWF("wf-open", "open-worker")

	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "alpha.yaml"), []byte(`
projectId: alpha
displayName: Alpha
swarmId: swarm-net
defaultWorkflowId: wf-inert
defaultPriority: 50
adaptiveCandidateWorkflows:
  - wf-inert
  - wf-capable
`), 0o644))

	reg := registry.New()
	require.NoError(t, reg.Load(root))
	return reg
}

func newNetworkGuardServer(t *testing.T) (*Server, *memAPIKeyRepo, *mocks.MockTaskRepository) {
	t.Helper()
	reg := seedNetworkGuardRegistry(t)
	keyRepo := &memAPIKeyRepo{}
	taskRepo := &mocks.MockTaskRepository{}
	creator := taskcreate.New(
		taskcreate.WithTaskRepository(taskRepo),
		taskcreate.WithProjectRegistry(reg),
	)
	return &Server{
		logger:          zerolog.Nop(),
		apiKeyRepo:      keyRepo,
		taskRepo:        taskRepo,
		taskCreator:     creator,
		projectRegistry: reg,
	}, keyRepo, taskRepo
}

// incidentPrompt is the 2026-09-11 shape, verbatim in intent.
const incidentPrompt = "load https://developer.hashicorp.com/terraform/docs into project RAG"

func callDelegate(t *testing.T, srv *Server, rawKey string, args map[string]any) (string, bool) {
	t.Helper()
	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "delegate",
		"arguments": args,
	})
	req = withCompanionBearer(req, rawKey)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)
	return decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
}

// ---- §4.3 the guard ------------------------------------------------

// TestCompanionMCP_Delegate_URLToNetworkIncapableWorkflow_Refused is the
// regression test for the 2026-09-11 incident: a host delegated a
// URL-bearing "load these docs into RAG" prompt to
// companion-research-gather, whose analyst role holds no fetch tool.
// The task queued, reported plausible, and fetched nothing — the
// failure was indistinguishable from success.
//
// The refusal must be a VISIBLE tool error (not a queued task with a
// warning attached — that is the property that made the 2026-06-05
// require_input_artifacts guard work), and it must name the blessed
// path and the escape argument.
func TestCompanionMCP_Delegate_URLToNetworkIncapableWorkflow_Refused(t *testing.T) {
	srv, keyRepo, taskRepo := newNetworkGuardServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-inert", "wf-capable", "wf-open"})

	text, isErr := callDelegate(t, srv, raw, map[string]any{
		"workflow": "wf-inert",
		"prompt":   incidentPrompt,
	})

	require.True(t, isErr,
		"a URL-bearing prompt to a network-incapable workflow must surface as a tool error; got: %s", text)
	assert.Contains(t, text, "cannot", "the refusal must say the workflow cannot fetch")
	assert.Contains(t, text, "companion-rag-ingest",
		"the refusal must name the blessed path's terminal workflow")
	assert.Contains(t, text, "inputArtifacts",
		"the refusal must name how bytes actually reach a workflow")
	assert.Contains(t, text, "acknowledge_workflow_cannot_fetch",
		"the refusal must name the escape argument")
	assert.Equal(t, 0, taskRepo.CallCount.Create,
		"no task may be created — a queued task is what made the incident look like success")
}

// TestCompanionMCP_Delegate_URLWithAcknowledgement_Succeeds — the
// escape hatch for an incidentally-mentioned URL ("review this diff;
// context: <link>"). The host restates the intent deliberately.
func TestCompanionMCP_Delegate_URLWithAcknowledgement_Succeeds(t *testing.T) {
	srv, keyRepo, taskRepo := newNetworkGuardServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-inert"})

	text, isErr := callDelegate(t, srv, raw, map[string]any{
		"workflow":                          "wf-inert",
		"prompt":                            incidentPrompt,
		"acknowledge_workflow_cannot_fetch": true,
	})
	require.False(t, isErr, "acknowledged delegation must proceed; got: %s", text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.NotEmpty(t, out["task_id"])
	assert.Equal(t, 1, taskRepo.CallCount.Create)
}

// TestCompanionMCP_Delegate_URLToNetworkCapableWorkflow_Unaffected —
// the guard fires on the workflow's tools, not on the presence of a
// URL. A workflow whose role holds a non-inert tool is left alone.
func TestCompanionMCP_Delegate_URLToNetworkCapableWorkflow_Unaffected(t *testing.T) {
	srv, keyRepo, taskRepo := newNetworkGuardServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-capable"})

	text, isErr := callDelegate(t, srv, raw, map[string]any{
		"workflow": "wf-capable",
		"prompt":   incidentPrompt,
	})
	require.False(t, isErr, "network-capable workflow must be unaffected; got: %s", text)
	assert.Equal(t, 1, taskRepo.CallCount.Create)
}

// TestCompanionMCP_Delegate_NoURL_Unaffected — a prompt with no URL is
// never refused, however inert the workflow.
func TestCompanionMCP_Delegate_NoURL_Unaffected(t *testing.T) {
	srv, keyRepo, taskRepo := newNetworkGuardServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-inert"})

	text, isErr := callDelegate(t, srv, raw, map[string]any{
		"workflow": "wf-inert",
		"prompt":   "summarise what we already know about terraform module layout",
	})
	require.False(t, isErr, "URL-free prompt must be unaffected; got: %s", text)
	assert.Equal(t, 1, taskRepo.CallCount.Create)
}

// TestCompanionMCP_Delegate_UnknownWorkflow_NotBlocked mirrors the
// 2026-06-05 guard's own carve-out: an ID the catalogue does not know
// is not this guard's business, and the task creator's validator is
// what rejects it.
func TestCompanionMCP_Delegate_UnknownWorkflow_NotBlocked(t *testing.T) {
	srv, keyRepo, _ := newNetworkGuardServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)

	text, _ := callDelegate(t, srv, raw, map[string]any{
		"workflow": "wf-does-not-exist",
		"prompt":   incidentPrompt,
	})
	assert.NotContains(t, text, "acknowledge_workflow_cannot_fetch",
		"an unknown workflow must not be refused by the network guard")
}

// ---- §4.3 catalogue --------------------------------------------------

func TestCompanionMCP_Catalog_ReportsDerivedNetworkCapability(t *testing.T) {
	srv, keyRepo, _ := newNetworkGuardServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-inert", "wf-capable", "wf-open"})

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "catalog",
		"arguments": map[string]any{},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)
	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "catalog must succeed; got: %s", text)

	var out struct {
		Workflows []map[string]any `json:"workflows"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	byID := map[string]map[string]any{}
	for _, e := range out.Workflows {
		byID[e["id"].(string)] = e
	}
	require.Contains(t, byID, "wf-inert")
	require.Contains(t, byID, "wf-capable")
	require.Contains(t, byID, "wf-open")

	assert.Equal(t, "none", byID["wf-inert"]["network_access"],
		"an all-inert workflow must be published as network_access=none, next to its description")
	assert.Equal(t, "possible", byID["wf-capable"]["network_access"])
	assert.Equal(t, "possible", byID["wf-open"]["network_access"],
		"a role that declares no allowedTools is UNRESTRICTED, never inert")
}
