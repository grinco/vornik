package api

// Coverage tests for the companion MCP tool methods left thin by
// companion_mcp_test.go:
//   - companionToolList (0% → list happy path, source/age filtering,
//     invalid args, no-repo, repo error, status filter)
//   - companionToolStatus / Result / Cancel error branches
//     (missing task_id, not-found, lookup error)
//   - companionInlineArtifacts + readArtifactBytes (disk-read fallback)
//
// Reuses newCompanionMCPServer / seedCompanionKey / withCompanionBearer
// / mcpRequest / decodeJSONRPC / decodeToolText from companion_mcp_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// cmcpCallTool issues a tools/call for the named tool with the given
// arguments and returns the decoded tool text + IsError flag.
func cmcpCallTool(t *testing.T, srv *Server, raw, name string, args map[string]any) (string, bool) {
	t.Helper()
	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)
	return decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
}

// --- list ----------------------------------------------------------

func TestCompanionMCP_List_FiltersBySourceAndAge(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)

	wf := "wf-alpha"
	now := time.Now()
	taskRepo.ListFunc = func(_ context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
		require.NotNil(t, filter.ProjectID)
		assert.Equal(t, "alpha", *filter.ProjectID)
		return []*persistence.Task{
			// Companion-originated + recent → kept.
			{ID: "keep", ProjectID: "alpha", WorkflowID: &wf,
				Status: persistence.TaskStatusRunning, CreatedAt: now,
				CreationSource: persistence.TaskCreationSourceCompanion},
			// Non-companion source → dropped.
			{ID: "other-src", ProjectID: "alpha",
				Status: persistence.TaskStatusRunning, CreatedAt: now,
				CreationSource: persistence.TaskCreationSourceUser},
			// Companion but too old → dropped.
			{ID: "too-old", ProjectID: "alpha",
				Status:         persistence.TaskStatusCompleted,
				CreatedAt:      now.Add(-30 * 24 * time.Hour),
				CreationSource: persistence.TaskCreationSourceCompanion},
			nil, // nil entry → skipped without panic
		}, nil
	}

	text, isErr := cmcpCallTool(t, srv, raw, "list", map[string]any{})
	require.False(t, isErr, "list: %s", text)
	var out struct {
		Project string `json:"project"`
		Total   int    `json:"total"`
		Tasks   []struct {
			TaskID   string `json:"task_id"`
			Workflow string `json:"workflow"`
		} `json:"tasks"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, "alpha", out.Project)
	require.Equal(t, 1, out.Total, "only the recent companion-sourced task survives")
	assert.Equal(t, "keep", out.Tasks[0].TaskID)
	assert.Equal(t, "wf-alpha", out.Tasks[0].Workflow)
}

func TestCompanionMCP_List_StatusFilterThreaded(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)

	var sawStatus *persistence.TaskStatus
	taskRepo.ListFunc = func(_ context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
		sawStatus = filter.Status
		return nil, nil
	}
	// lowercase status must be upcased before reaching the filter.
	text, isErr := cmcpCallTool(t, srv, raw, "list", map[string]any{"status": "running", "limit": 5})
	require.False(t, isErr, "list: %s", text)
	require.NotNil(t, sawStatus, "status filter must be threaded to TaskFilter")
	assert.Equal(t, persistence.TaskStatus("RUNNING"), *sawStatus)
}

func TestCompanionMCP_List_RepoError(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	taskRepo.ListFunc = func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
		return nil, errors.New("list boom")
	}
	text, isErr := cmcpCallTool(t, srv, raw, "list", map[string]any{})
	require.True(t, isErr)
	assert.Contains(t, text, "list boom")
}

func TestCompanionMCP_List_InvalidArgs(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	// Pass a non-object for arguments (string) so json.Unmarshal into
	// listArgs fails.
	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "list",
		"arguments": "not-an-object",
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)
	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.True(t, isErr)
	assert.Contains(t, text, "invalid arguments")
}

// --- status / result / cancel error branches ----------------------

func TestCompanionMCP_Status_MissingTaskID(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	text, isErr := cmcpCallTool(t, srv, raw, "status", map[string]any{"task_id": "   "})
	require.True(t, isErr)
	assert.Contains(t, text, "task_id is required")
}

func TestCompanionMCP_Status_NotFound(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	taskRepo.GetFunc = func(_ context.Context, _ string) (*persistence.Task, error) {
		return nil, persistence.ErrNotFound
	}
	text, isErr := cmcpCallTool(t, srv, raw, "status", map[string]any{"task_id": "ghost"})
	require.True(t, isErr)
	assert.Contains(t, text, "not found")
}

func TestCompanionMCP_Status_LookupError(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	taskRepo.GetFunc = func(_ context.Context, _ string) (*persistence.Task, error) {
		return nil, errors.New("db gone")
	}
	text, isErr := cmcpCallTool(t, srv, raw, "status", map[string]any{"task_id": "t1"})
	require.True(t, isErr)
	assert.Contains(t, text, "lookup")
}

// Status happy path with a last_error populated — covers the success
// projection + the LastError branch.
func TestCompanionMCP_Status_HappyPath_WithLastError(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	lastErr := "step 2 failed"
	wf := "wf-alpha"
	taskRepo.GetFunc = func(_ context.Context, _ string) (*persistence.Task, error) {
		return &persistence.Task{
			ID: "t1", ProjectID: "alpha", WorkflowID: &wf,
			Status: persistence.TaskStatusFailed, Attempt: 2,
			LastError: &lastErr, CreatedAt: time.Now(),
		}, nil
	}
	text, isErr := cmcpCallTool(t, srv, raw, "status", map[string]any{"task_id": "t1"})
	require.False(t, isErr, "status: %s", text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, "FAILED", out["status"])
	assert.Equal(t, "wf-alpha", out["workflow"])
	assert.Equal(t, "step 2 failed", out["last_error"])
}

func TestCompanionMCP_Result_NotFound(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	taskRepo.GetFunc = func(_ context.Context, _ string) (*persistence.Task, error) {
		return nil, persistence.ErrNotFound
	}
	text, isErr := cmcpCallTool(t, srv, raw, "result", map[string]any{"task_id": "ghost"})
	require.True(t, isErr)
	assert.Contains(t, text, "not found")
}

func TestCompanionMCP_Result_CrossProjectBlocked(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	taskRepo.GetFunc = func(_ context.Context, _ string) (*persistence.Task, error) {
		return &persistence.Task{ID: "x", ProjectID: "beta", Status: persistence.TaskStatusCompleted}, nil
	}
	text, isErr := cmcpCallTool(t, srv, raw, "result", map[string]any{"task_id": "x"})
	require.True(t, isErr)
	assert.Contains(t, text, "not found")
}

// Result terminal path with an inlined OUTPUT artifact read from disk.
// Covers companionInlineArtifacts (kept/skipped paths) + the disk-read
// fallback in readArtifactBytes.
func TestCompanionMCP_Result_Terminal_InlinesArtifactFromDisk(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)

	wf := "wf-alpha"
	taskRepo.GetFunc = func(_ context.Context, _ string) (*persistence.Task, error) {
		return &persistence.Task{
			ID: "t1", ProjectID: "alpha", WorkflowID: &wf,
			Status: persistence.TaskStatusCompleted, UpdatedAt: time.Now(),
		}, nil
	}

	dir := t.TempDir()
	outPath := filepath.Join(dir, "verdict.md")
	require.NoError(t, os.WriteFile(outPath, []byte("APPROVED: ship it"), 0o600))
	size := int64(len("APPROVED: ship it"))

	artRepo := &mocks.MockArtifactRepository{}
	artRepo.ListFunc = func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
		return []*persistence.Artifact{
			{ID: "a1", Name: "verdict.md", ArtifactClass: persistence.ArtifactClassOutput,
				StoragePath: outPath, SizeBytes: &size},
			// A non-OUTPUT artifact (LOG) is skipped.
			{ID: "a2", Name: "run.log", ArtifactClass: persistence.ArtifactClassLog, StoragePath: outPath},
		}, nil
	}
	srv.artifactRepo = artRepo

	text, isErr := cmcpCallTool(t, srv, raw, "result", map[string]any{"task_id": "t1"})
	require.False(t, isErr, "result: %s", text)
	var out struct {
		Complete  bool `json:"complete"`
		Artifacts []struct {
			ArtifactID string `json:"artifact_id"`
			Name       string `json:"name"`
			Content    string `json:"content"`
		} `json:"artifacts"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.True(t, out.Complete)
	require.Len(t, out.Artifacts, 1, "only the OUTPUT artifact is inlined")
	assert.Equal(t, "verdict.md", out.Artifacts[0].Name)
	assert.Equal(t, "APPROVED: ship it", out.Artifacts[0].Content)
}

// Result terminal path where the artifact body can't be read (no
// opener + empty storage path) → content_error, not a hard failure.
func TestCompanionMCP_Result_Terminal_ArtifactReadError(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	taskRepo.GetFunc = func(_ context.Context, _ string) (*persistence.Task, error) {
		return &persistence.Task{ID: "t1", ProjectID: "alpha",
			Status: persistence.TaskStatusCompleted, UpdatedAt: time.Now()}, nil
	}
	artRepo := &mocks.MockArtifactRepository{}
	artRepo.ListFunc = func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
		return []*persistence.Artifact{
			{ID: "a1", Name: "out.txt", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: ""},
		}, nil
	}
	srv.artifactRepo = artRepo

	text, isErr := cmcpCallTool(t, srv, raw, "result", map[string]any{"task_id": "t1"})
	require.False(t, isErr, "result: %s", text)
	assert.Contains(t, text, "content_error")
}

func TestCompanionMCP_Cancel_MissingTaskID(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	text, isErr := cmcpCallTool(t, srv, raw, "cancel", map[string]any{})
	require.True(t, isErr)
	assert.Contains(t, text, "task_id is required")
}

func TestCompanionMCP_Cancel_HappyPath_Transitions(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	taskRepo.GetFunc = func(_ context.Context, _ string) (*persistence.Task, error) {
		return &persistence.Task{ID: "t1", ProjectID: "alpha", Status: persistence.TaskStatusRunning}, nil
	}
	taskRepo.TransitionToCancelledFunc = func(_ context.Context, _ string) (bool, error) {
		return true, nil
	}
	text, isErr := cmcpCallTool(t, srv, raw, "cancel", map[string]any{"task_id": "t1", "reason": "stop"})
	require.False(t, isErr, "cancel: %s", text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, true, out["cancelled"])
	assert.Equal(t, "stop", out["reason_given"])
	assert.NotContains(t, text, "no-op")
}

func TestCompanionMCP_Cancel_TransitionError(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	taskRepo.GetFunc = func(_ context.Context, _ string) (*persistence.Task, error) {
		return &persistence.Task{ID: "t1", ProjectID: "alpha", Status: persistence.TaskStatusRunning}, nil
	}
	taskRepo.TransitionToCancelledFunc = func(_ context.Context, _ string) (bool, error) {
		return false, errors.New("txn aborted")
	}
	text, isErr := cmcpCallTool(t, srv, raw, "cancel", map[string]any{"task_id": "t1"})
	require.True(t, isErr)
	assert.Contains(t, text, "txn aborted")
}

// --- catalog: adaptive-candidate path (no allowlist on the key) ----

func TestCompanionMCP_Catalog_AdaptiveCandidatesWhenNoAllowlist(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	// nil allowlist → catalog must fall back to the project's adaptive
	// candidate workflows + default workflow.
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)

	text, isErr := cmcpCallTool(t, srv, raw, "catalog", map[string]any{})
	require.False(t, isErr, "catalog: %s", text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, "alpha", out["project"])
	// memory caps always present (stable schema).
	_, hasRead := out["memory_read"]
	_, hasWrite := out["memory_write"]
	assert.True(t, hasRead)
	assert.True(t, hasWrite)
	// delegate_input_schema is echoed for client convenience.
	assert.NotNil(t, out["delegate_input_schema"])
}
