package api

// Integration test for Phase C v2 VariableMemoryChunkExcluded:
// when a counterfactual task carries excluded_chunks in its
// payload, the companion recall handler filters hits whose
// ChunkID matches before serialising the response.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// TestCompanionMCP_Recall_CounterfactualExcludesChunks — task
// payload carries excluded_chunks ["c1","c3"]; the fake memory
// adapter returns c1/c2/c3/c4; the response must contain c2 + c4
// only. Pins the wiring of X-Task-ID → taskRepo lookup →
// PayloadOverrides.IsChunkExcluded → hit filter.
func TestCompanionMCP_Recall_CounterfactualExcludesChunks(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", []string{"wf-alpha"}, true, false)

	srv.memoryCompanion = &fakeMemoryCompanion{
		recallReturn: []MemorySearchResult{
			{ChunkID: "c1", ProjectID: "alpha", Score: 0.9, Content: "drop me"},
			{ChunkID: "c2", ProjectID: "alpha", Score: 0.8, Content: "keep me"},
			{ChunkID: "c3", ProjectID: "alpha", Score: 0.7, Content: "drop me too"},
			{ChunkID: "c4", ProjectID: "alpha", Score: 0.6, Content: "keep me too"},
		},
	}

	// Task carries the counterfactual block with excluded chunks.
	cfTaskID := "task-cf-1"
	cfTask := &persistence.Task{
		ID:        cfTaskID,
		ProjectID: "alpha",
		Payload: json.RawMessage(
			`{"context":{"counterfactual":{"excluded_chunks":["c1","c3"]}}}`,
		),
	}
	taskRepo.GetFunc = func(_ context.Context, id string) (*persistence.Task, error) {
		if id == cfTaskID {
			return cfTask, nil
		}
		return nil, persistence.ErrNotFound
	}

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "recall",
		"arguments": map[string]any{"query": "hi"},
	})
	req = withCompanionBearer(req, raw)
	req.Header.Set("X-Task-ID", cfTaskID)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "recall should succeed")

	var resp struct {
		Hits []struct {
			ChunkID string `json:"chunk_id"`
		} `json:"hits"`
		Returned int `json:"returned"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	require.Len(t, resp.Hits, 2, "two chunks should remain after exclusion")
	got := []string{resp.Hits[0].ChunkID, resp.Hits[1].ChunkID}
	assert.ElementsMatch(t, []string{"c2", "c4"}, got)
	assert.Equal(t, 2, resp.Returned)
}

// TestCompanionMCP_Recall_NonCounterfactualUnchanged — same setup
// without the X-Task-ID header (or with a task that has no
// counterfactual block) returns ALL hits, proving the filter is
// gated cleanly on the counterfactual marker.
func TestCompanionMCP_Recall_NonCounterfactualUnchanged(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", []string{"wf-alpha"}, true, false)

	srv.memoryCompanion = &fakeMemoryCompanion{
		recallReturn: []MemorySearchResult{
			{ChunkID: "c1", ProjectID: "alpha", Score: 0.9},
			{ChunkID: "c2", ProjectID: "alpha", Score: 0.8},
		},
	}

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "recall",
		"arguments": map[string]any{"query": "hi"},
	})
	req = withCompanionBearer(req, raw)
	// No X-Task-ID header — non-counterfactual call path.
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr)

	var resp struct {
		Hits []struct {
			ChunkID string `json:"chunk_id"`
		} `json:"hits"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	require.Len(t, resp.Hits, 2, "no task header → no filter → both hits returned")
}
