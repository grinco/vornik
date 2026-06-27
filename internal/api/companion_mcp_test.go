package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/taskcreate"
)

// newCompanionMCPServer builds a Server primed for MCP-handler tests:
// in-memory APIKey repo, mocked TaskRepository (test scripts its Funcs),
// real TaskCreator + project registry seeded by seedRegistry (alpha+beta
// projects, wf-alpha+wf-beta workflows). Returns the server plus the
// mocks the test can manipulate.
func newCompanionMCPServer(t *testing.T) (*Server, *memAPIKeyRepo, *mocks.MockTaskRepository) {
	t.Helper()
	reg := seedRegistry(t)
	keyRepo := &memAPIKeyRepo{}
	taskRepo := &mocks.MockTaskRepository{}
	creator := taskcreate.New(
		taskcreate.WithTaskRepository(taskRepo),
		taskcreate.WithProjectRegistry(reg),
	)
	srv := &Server{
		logger:          zerolog.Nop(),
		apiKeyRepo:      keyRepo,
		taskRepo:        taskRepo,
		taskCreator:     creator,
		projectRegistry: reg,
	}
	return srv, keyRepo, taskRepo
}

// seedCompanionKey writes a companion-scoped APIKey into the in-memory
// repo and returns (rawKey, row). The caller stamps the raw key into
// the request context via withCompanionBearer.
func seedCompanionKey(t *testing.T, repo *memAPIKeyRepo, projectID string, allowed []string) (string, *persistence.APIKey) {
	t.Helper()
	raw, err := apikey.Generate(projectID)
	require.NoError(t, err)
	row := &persistence.APIKey{
		ID:               "akey-co-" + projectID,
		ProjectID:        projectID,
		Name:             "session-1",
		KeyHash:          apikey.Hash(raw),
		KeyPrefix:        apikey.DisplayPrefix(raw),
		ClientKind:       "claude-code",
		SessionLabel:     "test/laptop",
		AllowedWorkflows: allowed,
		CreatedAt:        time.Now().UTC(),
	}
	require.NoError(t, repo.Create(context.Background(), row))
	return raw, row
}

// withCompanionBearer stamps the raw bearer into the request context
// the way AuthMiddleware would, so the MCP handler's
// APIKeyFromContext call returns it.
func withCompanionBearer(req *http.Request, raw string) *http.Request {
	ctx := context.WithValue(req.Context(), apiKeyKey, raw)
	return req.WithContext(ctx)
}

func mcpRequest(t *testing.T, method string, params any) *http.Request {
	t.Helper()
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
	}
	if params != nil {
		body["params"] = params
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	return httptest.NewRequest(http.MethodPost, "/api/v1/mcp/companion", bytes.NewReader(raw))
}

func decodeJSONRPC(t *testing.T, body []byte) jsonRPCResponse {
	t.Helper()
	var resp jsonRPCResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	return resp
}

// decodeToolText pulls the first content[0].text out of a tools/call
// result. Many tool methods write structured JSON into that text
// field so the test can unmarshal it again.
func decodeToolText(t *testing.T, resp jsonRPCResponse) (text string, isError bool) {
	t.Helper()
	require.Nil(t, resp.Error, "JSON-RPC error: %+v", resp.Error)
	raw, err := json.Marshal(resp.Result)
	require.NoError(t, err)
	var tcr mcpToolCallResult
	require.NoError(t, json.Unmarshal(raw, &tcr))
	require.NotEmpty(t, tcr.Content, "tool result had no content")
	return tcr.Content[0].Text, tcr.IsError
}

// TestCompanionMCP_Initialize — the first call any MCP client makes.
// Must return the protocolVersion + serverInfo even before the bearer
// has been validated (clients may probe capabilities first).
func TestCompanionMCP_Initialize(t *testing.T) {
	srv, _, _ := newCompanionMCPServer(t)
	req := mcpRequest(t, "initialize", nil)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeJSONRPC(t, rec.Body.Bytes())
	require.Nil(t, resp.Error)
	result, ok := resp.Result.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, companionMCPProtocolVersion, result["protocolVersion"])
	srvInfo, _ := result["serverInfo"].(map[string]any)
	assert.Equal(t, companionMCPServerName, srvInfo["name"])
}

// TestCompanionMCP_ToolsList — the 6-tool palette must be present
// with the exact tool names the LLD pins. Renaming any of these is
// a contract break for every shipped plugin manifest.
func TestCompanionMCP_ToolsList(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})
	req := mcpRequest(t, "tools/list", nil)
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	resp := decodeJSONRPC(t, rec.Body.Bytes())
	require.Nil(t, resp.Error)
	result, _ := resp.Result.(map[string]any)
	tools, _ := result["tools"].([]any)
	got := make(map[string]bool)
	for _, t := range tools {
		if td, ok := t.(map[string]any); ok {
			got[td["name"].(string)] = true
		}
	}
	for _, expected := range []string{"delegate", "status", "result", "cancel", "list", "catalog"} {
		assert.Truef(t, got[expected], "tools/list missing tool %q", expected)
	}
}

func TestCompanionMCP_ToolsList_NonCompanionKeyAuthFails(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, err := apikey.Generate("alpha")
	require.NoError(t, err)
	require.NoError(t, keyRepo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-legacy", ProjectID: "alpha", Name: "legacy",
		KeyHash: apikey.Hash(raw), KeyPrefix: apikey.DisplayPrefix(raw),
	}))

	req := mcpRequest(t, "tools/list", nil)
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	resp := decodeJSONRPC(t, rec.Body.Bytes())
	require.NotNil(t, resp.Error)
	assert.Contains(t, resp.Error.Message, "not a companion-scoped key")
}

func TestCompanionMCP_UnknownMethod_ReturnsJSONRPCError(t *testing.T) {
	srv, _, _ := newCompanionMCPServer(t)
	req := mcpRequest(t, "totally/bogus", nil)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	resp := decodeJSONRPC(t, rec.Body.Bytes())
	require.NotNil(t, resp.Error)
	assert.Equal(t, -32601, resp.Error.Code)
}

func TestCompanionMCP_MalformedJSON_ParseError(t *testing.T) {
	srv, _, _ := newCompanionMCPServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp/companion", strings.NewReader("{not json"))
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	resp := decodeJSONRPC(t, rec.Body.Bytes())
	require.NotNil(t, resp.Error)
	assert.Equal(t, -32700, resp.Error.Code)
}

func TestCompanionMCP_GETReturns200_LivenessProbe(t *testing.T) {
	srv, _, _ := newCompanionMCPServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/mcp/companion", nil)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestCompanionMCP_ToolCall_NonCompanionKey_AuthFails — a regular
// (non-companion) bearer can authenticate against the rest of the
// API but must not reach the tool surface. This is the gate that
// keeps companion scope columns authoritative; a non-companion key
// has NULLs everywhere and would otherwise behave as "uncapped".
func TestCompanionMCP_ToolCall_NonCompanionKey_AuthFails(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, err := apikey.Generate("alpha")
	require.NoError(t, err)
	require.NoError(t, keyRepo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-legacy", ProjectID: "alpha", Name: "legacy",
		KeyHash: apikey.Hash(raw), KeyPrefix: apikey.DisplayPrefix(raw),
		// ClientKind intentionally empty — this is a NON-companion key.
	}))

	req := mcpRequest(t, "tools/call", map[string]any{
		"name": "catalog", "arguments": map[string]any{},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	resp := decodeJSONRPC(t, rec.Body.Bytes())
	require.NotNil(t, resp.Error, "non-companion key must JSON-RPC-error, not produce a tool result")
	assert.Contains(t, resp.Error.Message, "not a companion-scoped key")
}

func TestCompanionMCP_Delegate_HappyPath_CreatesTaskWithCompanionSource(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})

	req := mcpRequest(t, "tools/call", map[string]any{
		"name": "delegate",
		"arguments": map[string]any{
			"workflow": "wf-alpha",
			"prompt":   "audit this diff",
		},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	resp := decodeJSONRPC(t, rec.Body.Bytes())
	text, isErr := decodeToolText(t, resp)
	require.False(t, isErr, "delegate should not produce IsError; got: %s", text)

	var dout map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &dout))
	assert.NotEmpty(t, dout["task_id"])
	assert.Equal(t, "alpha", dout["project"])
	assert.Equal(t, "wf-alpha", dout["workflow"])

	require.Equal(t, 1, taskRepo.CallCount.Create, "exactly one task created")
	created := taskRepo.LastCall.Task
	require.NotNil(t, created)
	assert.Equal(t, persistence.TaskCreationSourceCompanion, created.CreationSource,
		"audit trail must record companion provenance — distinguishes from A2A")
	// Payload should carry the companion session marker so list()
	// + later filters can identify who delegated this.
	require.NotEmpty(t, created.Payload)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(created.Payload, &payload))
	// taskcreate.buildPayload wraps RawContext at payload.context,
	// so the companion marker lives at payload.context.companion.
	taskCtx, _ := payload["context"].(map[string]any)
	require.NotNil(t, taskCtx, "task payload must carry a context block")
	companion, _ := taskCtx["companion"].(map[string]any)
	require.NotNil(t, companion, "context must include the 'companion' marker block")
	assert.Equal(t, "claude-code", companion["client_kind"])
	assert.Equal(t, "akey-co-alpha", companion["api_key_id"])
}

// stubUsageRepo satisfies persistence.TaskLLMUsageRepository by
// embedding the interface (only SumCostByAPIKey is exercised by the
// budget gate; any other call would panic, which is fine for these
// tests). Lets the delegate budget-cap tests script prior spend.
type stubUsageRepo struct {
	persistence.TaskLLMUsageRepository
	spend float64
	err   error
	// meanCost / meanSample script the per-workflow cost estimate
	// (§8.2). meanSample 0 = "no prior runs" → estimate omitted.
	meanCost   float64
	meanSample int
}

func (s stubUsageRepo) SumCostByAPIKey(_ context.Context, _ string, _, _ time.Time) (float64, error) {
	return s.spend, s.err
}

func (s stubUsageRepo) MeanCostByWorkflow(_ context.Context, _, _ string, _, _ time.Time) (float64, int, error) {
	if s.err != nil {
		return 0, 0, s.err
	}
	return s.meanCost, s.meanSample, nil
}

// TestCompanionMCP_Delegate_BudgetCapBlocksWhenExceeded pins finding
// #2: a key whose prior spend has reached its BudgetCapUSD must be
// refused with a BUDGET_EXCEEDED error before any task is created.
func TestCompanionMCP_Delegate_BudgetCapBlocksWhenExceeded(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})
	cap := 1.0
	keyRepo.rows[0].BudgetCapUSD = &cap // stored copy carries the cap
	srv.llmUsageRepo = stubUsageRepo{spend: 5.0}

	req := mcpRequest(t, "tools/call", map[string]any{
		"name": "delegate",
		"arguments": map[string]any{
			"workflow": "wf-alpha",
			"prompt":   "audit this diff",
		},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	resp := decodeJSONRPC(t, rec.Body.Bytes())
	text, isErr := decodeToolText(t, resp)
	require.True(t, isErr, "over-cap delegate must be an error; got: %s", text)
	assert.Contains(t, text, "BUDGET_EXCEEDED")
	assert.Equal(t, 0, taskRepo.CallCount.Create, "no task may be created once the cap is reached")
}

// TestCompanionMCP_Delegate_BudgetCapAllowsUnderCap confirms a key
// below its cap still delegates normally.
func TestCompanionMCP_Delegate_BudgetCapAllowsUnderCap(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})
	cap := 10.0
	keyRepo.rows[0].BudgetCapUSD = &cap
	srv.llmUsageRepo = stubUsageRepo{spend: 5.0}

	req := mcpRequest(t, "tools/call", map[string]any{
		"name": "delegate",
		"arguments": map[string]any{
			"workflow": "wf-alpha",
			"prompt":   "audit this diff",
		},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	resp := decodeJSONRPC(t, rec.Body.Bytes())
	text, isErr := decodeToolText(t, resp)
	require.False(t, isErr, "under-cap delegate should succeed; got: %s", text)
	assert.Equal(t, 1, taskRepo.CallCount.Create, "task should be created under cap")
}

// fakeInputArtifactStore captures the artifacts it received and
// returns a synthetic persistence.Artifact so the companion delegate
// path can complete without touching real disk. Test-only.
type fakeInputArtifactStore struct {
	stored []struct{ Project, Name string }
}

func (f *fakeInputArtifactStore) StoreInput(_ context.Context, projectID, name, _ string) (*persistence.Artifact, error) {
	f.stored = append(f.stored, struct{ Project, Name string }{projectID, name})
	return &persistence.Artifact{
		ID:          "art-" + name,
		ProjectID:   projectID,
		Name:        name,
		StoragePath: "/fake/storage/" + name,
	}, nil
}

// TestCompanionMCP_Delegate_InputArtifacts_FoldsIntoContext — the
// 2026-05-27 ingestion-failure path that drove this feature: a
// remote-client delegate carrying inline base64 file payloads must
// (a) snapshot each via the input-artifact store and (b) fold the
// resulting inputFiles / inputArtifactIDs into the task payload's
// context so the workflow agent sees real paths instead of ghost
// laptop paths it can't reach.
func TestCompanionMCP_Delegate_InputArtifacts_FoldsIntoContext(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	srv.inputArtifactStore = &fakeInputArtifactStore{}
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})

	// Two tiny base64 payloads — "hello" and "world".
	req := mcpRequest(t, "tools/call", map[string]any{
		"name": "delegate",
		"arguments": map[string]any{
			"workflow": "wf-alpha",
			"prompt":   "ingest these",
			"inputArtifacts": []map[string]any{
				{"name": "README.md", "content": "aGVsbG8="},  // "hello"
				{"name": "BACKLOG.md", "content": "d29ybGQ="}, // "world"
			},
		},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	resp := decodeJSONRPC(t, rec.Body.Bytes())
	text, isErr := decodeToolText(t, resp)
	require.False(t, isErr, "delegate with inputArtifacts must succeed; got: %s", text)

	require.Equal(t, 1, taskRepo.CallCount.Create)
	created := taskRepo.LastCall.Task
	require.NotNil(t, created)

	// Payload.context must carry both inputFiles and inputArtifactIDs
	// so the workflow agent finds files at known paths instead of
	// globbing for ghosts.
	var payload map[string]any
	require.NoError(t, json.Unmarshal(created.Payload, &payload))
	taskCtx, _ := payload["context"].(map[string]any)
	require.NotNil(t, taskCtx, "task payload must carry a context block")

	files, _ := taskCtx["inputFiles"].([]any)
	require.Len(t, files, 2, "both files must be folded into context.inputFiles")
	ids, _ := taskCtx["inputArtifactIDs"].([]any)
	require.Len(t, ids, 2, "artifact IDs must be folded so the agent can reference them")

	// Companion marker should still be present — folding artifacts
	// must not stomp the existing payload shape.
	companion, _ := taskCtx["companion"].(map[string]any)
	require.NotNil(t, companion, "companion marker must survive the inputArtifacts merge")
}

// TestCompanionMCP_Delegate_SkipAutoExtract — skip_auto_extract: true
// must reach processInputArtifactsWithOpts so that file uploads aimed
// at workflow-driven ingest (companion-rag-ingest et al.) don't
// double-process at upload time. The fake store records what was
// stored; we then assert the resulting task payload has NO
// inputExtractions entry (auto-extract was skipped).
func TestCompanionMCP_Delegate_SkipAutoExtract(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	srv.inputArtifactStore = &fakeInputArtifactStore{}
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})

	req := mcpRequest(t, "tools/call", map[string]any{
		"name": "delegate",
		"arguments": map[string]any{
			"workflow":          "wf-alpha",
			"prompt":            "ingest these raw, workflow will chunk",
			"skip_auto_extract": true,
			"inputArtifacts": []map[string]any{
				{"name": "README.md", "content": "aGVsbG8="},
			},
		},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "delegate with skip_auto_extract must succeed; got: %s", text)
	require.Equal(t, 1, taskRepo.CallCount.Create)
	created := taskRepo.LastCall.Task
	require.NotNil(t, created)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(created.Payload, &payload))
	taskCtx, _ := payload["context"].(map[string]any)
	require.NotNil(t, taskCtx)

	// inputFiles + inputArtifactIDs should be present (staging is
	// still wired); inputExtractions must NOT be present because
	// auto-extract was skipped. Without this guarantee, the
	// workflow's extractTaskInputArtifacts would skip staging the
	// raw file and the agent would have nothing to ingest.
	require.NotEmpty(t, taskCtx["inputFiles"], "raw file path must still reach context")
	require.NotEmpty(t, taskCtx["inputArtifactIDs"], "artifact ID must still reach context")
	_, hasExtractions := taskCtx["inputExtractions"]
	assert.False(t, hasExtractions,
		"skip_auto_extract=true must produce NO inputExtractions entry — "+
			"otherwise the workflow's staging code skips the raw file and the agent globs forever (B-10)")
}

// TestCompanionMCP_Delegate_AutoExtractDefaultPreserved — default
// (skip_auto_extract unset / false) preserves the Telegram-email
// upload shape. Auto-extract is wired via tryAutoExtract which we
// don't run in this test (no extractor registry), but the option
// path must NOT short-circuit when the flag is false.
func TestCompanionMCP_Delegate_AutoExtractDefaultPreserved(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	srv.inputArtifactStore = &fakeInputArtifactStore{}
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})

	// Don't set skip_auto_extract at all.
	req := mcpRequest(t, "tools/call", map[string]any{
		"name": "delegate",
		"arguments": map[string]any{
			"workflow": "wf-alpha",
			"prompt":   "ingest",
			"inputArtifacts": []map[string]any{
				{"name": "README.md", "content": "aGVsbG8="},
			},
		},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	_, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr)
	require.Equal(t, 1, taskRepo.CallCount.Create,
		"default-path delegate must still create the task — only the auto-extract toggle differs")
}

// TestCompanionMCP_Delegate_InputArtifacts_NoStoreConfigured — fail
// fast (no task created) when the operator hasn't wired an
// InputArtifactStore but a caller tries to send attachments anyway.
func TestCompanionMCP_Delegate_InputArtifacts_NoStoreConfigured(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	// inputArtifactStore intentionally left nil
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})

	req := mcpRequest(t, "tools/call", map[string]any{
		"name": "delegate",
		"arguments": map[string]any{
			"workflow": "wf-alpha",
			"prompt":   "ingest these",
			"inputArtifacts": []map[string]any{
				{"name": "README.md", "content": "aGVsbG8="},
			},
		},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	resp := decodeJSONRPC(t, rec.Body.Bytes())
	text, isErr := decodeToolText(t, resp)
	require.True(t, isErr, "missing store must surface as a tool-error result, not a created task")
	assert.Contains(t, text, "input-artifact store not configured")
	assert.Equal(t, 0, taskRepo.CallCount.Create, "must NOT create a task when the store is missing")
}

// TestCompanionMCP_Delegate_RecallHint_EmittedOnStrongHit — LLD 22
// Phase 2. When the key carries memory_read AND the prompt has at
// least one strong (≥ 0.7) neighbour in the project's RAG, the
// delegate response carries a recall_hint field with the top hits.
// The hint message tells the host LLM to consider recall first.
func TestCompanionMCP_Delegate_RecallHint_EmittedOnStrongHit(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fake := &fakeMemoryCompanion{
		recallReturn: []MemorySearchResult{
			{ChunkID: "ck1", ProjectID: "alpha", SourceName: "decision/legal-review",
				Content: "auth middleware rewrite is driven by compliance, not tech-debt",
				Score:   0.91},
			{ChunkID: "ck2", ProjectID: "alpha", SourceName: "research/old-1",
				Content: "below threshold; must not appear in hint",
				Score:   0.40},
		},
	}
	srv.memoryCompanion = fake
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", []string{"wf-alpha"}, true, false)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name": "delegate",
		"arguments": map[string]any{
			"workflow": "wf-alpha",
			"prompt":   "Review the auth middleware refactor for compliance impact",
		},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "delegate must still succeed: %s", text)

	var dout map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &dout))
	require.NotNil(t, dout["recall_hint"], "expected recall_hint when ≥1 strong hit exists")

	hint, _ := dout["recall_hint"].(map[string]any)
	require.NotNil(t, hint)
	assert.Contains(t, hint["message"].(string), "recall")
	hits, _ := hint["hits"].([]any)
	require.Len(t, hits, 1, "only the score ≥ 0.7 hit must appear")
	first, _ := hits[0].(map[string]any)
	assert.Equal(t, "ck1", first["chunk_id"])

	// The recall MUST be project-scoped to the key's project.
	// ActorKind carries the client_kind suffix per LLD-22 §"Provenance
	// and audit" so dashboards can split recalls by client.
	require.Len(t, fake.recallCalls, 1)
	assert.Equal(t, "alpha", fake.recallCalls[0].ProjectID)
	assert.Equal(t, "companion:claude-code", fake.recallCalls[0].Opts.ActorKind)
}

// TestCompanionMCP_Delegate_RecallHint_SuppressedWhenAllWeak — hits
// exist but none meet the score threshold. The hint must be omitted
// entirely (not "empty hits" — absent), because Claude reading
// "recall already covered this" off a weak hit would degrade trust.
func TestCompanionMCP_Delegate_RecallHint_SuppressedWhenAllWeak(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	srv.memoryCompanion = &fakeMemoryCompanion{
		recallReturn: []MemorySearchResult{
			{ChunkID: "ck1", ProjectID: "alpha", Score: 0.55},
			{ChunkID: "ck2", ProjectID: "alpha", Score: 0.30},
		},
	}
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", []string{"wf-alpha"}, true, false)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name": "delegate",
		"arguments": map[string]any{
			"workflow": "wf-alpha",
			"prompt":   "audit the latest deployment posture for the trading swarm",
		},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)
	text, _ := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	var dout map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &dout))
	_, present := dout["recall_hint"]
	assert.False(t, present, "weak-only hits must not produce a hint")
}

// TestCompanionMCP_Delegate_RecallHint_SkippedWithoutMemoryRead — the
// per-key capability gates the hint check entirely. No recall call,
// no field in the response.
func TestCompanionMCP_Delegate_RecallHint_SkippedWithoutMemoryRead(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fake := &fakeMemoryCompanion{
		recallReturn: []MemorySearchResult{
			{ChunkID: "ck1", Score: 0.95},
		},
	}
	srv.memoryCompanion = fake
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"}) // memory_read=false

	req := mcpRequest(t, "tools/call", map[string]any{
		"name": "delegate",
		"arguments": map[string]any{
			"workflow": "wf-alpha",
			"prompt":   "Review the trading-swarm risk officer's veto logic",
		},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)
	text, _ := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	var dout map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &dout))
	_, present := dout["recall_hint"]
	assert.False(t, present)
	assert.Empty(t, fake.recallCalls, "recall must not be called when key lacks memory_read")
}

// TestCompanionMCP_Delegate_RecallHint_SilentOnError — a failing
// adapter must never break delegate. The task creation still
// succeeds; the hint is just absent.
func TestCompanionMCP_Delegate_RecallHint_SilentOnError(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	srv.memoryCompanion = &fakeMemoryCompanion{
		recallErr: errors.New("embedder unreachable"),
	}
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", []string{"wf-alpha"}, true, false)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name": "delegate",
		"arguments": map[string]any{
			"workflow": "wf-alpha",
			"prompt":   "audit the latest deployment posture for the trading swarm",
		},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)
	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "delegate must succeed even when recall fails")
	var dout map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &dout))
	assert.NotEmpty(t, dout["task_id"], "task creation must still succeed")
	_, present := dout["recall_hint"]
	assert.False(t, present, "no hint when recall errored")
}

// TestCompanionMCP_Delegate_RecallHint_SkipsShortPrompt — guards
// against noise from tiny prompts. Embedding "go" or "yes" produces
// near-random hits; the threshold check would catch most, but bypass
// the recall entirely for prompts under recallHintMinPromptChars.
func TestCompanionMCP_Delegate_RecallHint_SkipsShortPrompt(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fake := &fakeMemoryCompanion{
		recallReturn: []MemorySearchResult{{ChunkID: "ck1", Score: 0.95}},
	}
	srv.memoryCompanion = fake
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", []string{"wf-alpha"}, true, false)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name": "delegate",
		"arguments": map[string]any{
			"workflow": "wf-alpha",
			"prompt":   "go",
		},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)
	text, _ := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	var dout map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &dout))
	_, present := dout["recall_hint"]
	assert.False(t, present)
	assert.Empty(t, fake.recallCalls, "recall must be skipped for sub-threshold-length prompts")
}

func TestCompanionMCP_Delegate_RejectsWorkflowNotInAllowlist(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	// Key allowlists only wf-alpha; client tries wf-beta.
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})

	req := mcpRequest(t, "tools/call", map[string]any{
		"name": "delegate",
		"arguments": map[string]any{
			"workflow": "wf-beta",
			"prompt":   "off-allowlist work",
		},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	resp := decodeJSONRPC(t, rec.Body.Bytes())
	text, isErr := decodeToolText(t, resp)
	require.True(t, isErr, "off-allowlist workflow must surface as tool IsError")
	assert.Contains(t, text, "not in this key's allowedWorkflows")
	assert.Equal(t, 0, taskRepo.CallCount.Create, "no task should land in the queue")
}

// TestCompanionMCP_Delegate_RequireInputArtifacts_RejectsWithoutArtifacts
// pins the 2026-06-05 silent-skip incident fix: a workflow that
// declares require_input_artifacts=true must reject a delegate()
// carrying no inputArtifacts up front, before any task is created.
// The error must steer the caller to the staging commands.
func TestCompanionMCP_Delegate_RequireInputArtifacts_RejectsWithoutArtifacts(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-artifacts"})

	req := mcpRequest(t, "tools/call", map[string]any{
		"name": "delegate",
		"arguments": map[string]any{
			"workflow": "wf-artifacts",
			"prompt":   "ingest /home/me/docs/spec.md and /home/me/docs/notes.md",
		},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	resp := decodeJSONRPC(t, rec.Body.Bytes())
	text, isErr := decodeToolText(t, resp)
	require.True(t, isErr, "artifact-less delegate to an artifact-only workflow must be an error; got: %s", text)
	assert.Contains(t, text, "inputArtifacts")
	assert.Equal(t, 0, taskRepo.CallCount.Create, "no task may be created when artifacts are missing")
}

// TestCompanionMCP_Delegate_RequireInputArtifacts_PassesWithArtifacts
// confirms the guard is precise: the same artifact-only workflow
// delegates normally once at least one inputArtifact is staged.
func TestCompanionMCP_Delegate_RequireInputArtifacts_PassesWithArtifacts(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	srv.inputArtifactStore = &fakeInputArtifactStore{}
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-artifacts"})

	req := mcpRequest(t, "tools/call", map[string]any{
		"name": "delegate",
		"arguments": map[string]any{
			"workflow": "wf-artifacts",
			"prompt":   "ingest the staged spec",
			"inputArtifacts": []map[string]any{
				{"name": "spec.md", "content": "aGVsbG8="}, // "hello"
			},
		},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	resp := decodeJSONRPC(t, rec.Body.Bytes())
	text, isErr := decodeToolText(t, resp)
	require.False(t, isErr, "delegate with a staged artifact must succeed; got: %s", text)
	assert.Equal(t, 1, taskRepo.CallCount.Create, "task should be created when an artifact is staged")
}

func TestCompanionMCP_Status_CrossProjectAccessBlocked(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)

	// Script the task repo to return a task owned by project "beta"
	// — even though such a task could legitimately exist, this key
	// is bound to "alpha". The handler must hide its existence.
	taskRepo.GetFunc = func(_ context.Context, _ string) (*persistence.Task, error) {
		return &persistence.Task{
			ID: "task-leaks", ProjectID: "beta",
			Status: persistence.TaskStatusRunning,
		}, nil
	}

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "status",
		"arguments": map[string]any{"task_id": "task-leaks"},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	resp := decodeJSONRPC(t, rec.Body.Bytes())
	text, isErr := decodeToolText(t, resp)
	require.True(t, isErr,
		"cross-project lookup must error as 'not found' — never leak existence")
	assert.Contains(t, text, "not found")
}

func TestCompanionMCP_Result_PendingShape_WhenNonTerminal(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	taskRepo.GetFunc = func(_ context.Context, _ string) (*persistence.Task, error) {
		return &persistence.Task{
			ID: "t1", ProjectID: "alpha",
			Status: persistence.TaskStatusRunning,
		}, nil
	}

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "result",
		"arguments": map[string]any{"task_id": "t1"},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "pending is NOT an error condition; host LLM pattern-matches the shape")
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, false, out["complete"])
	assert.Equal(t, "RUNNING", out["status"])
}

func TestCompanionMCP_Cancel_TerminalTask_NoOp(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	taskRepo.GetFunc = func(_ context.Context, _ string) (*persistence.Task, error) {
		return &persistence.Task{
			ID: "t1", ProjectID: "alpha",
			Status: persistence.TaskStatusCompleted,
		}, nil
	}
	// TransitionToCancelled returns (false, nil) for a terminal task.
	taskRepo.TransitionToCancelledFunc = func(_ context.Context, _ string) (bool, error) {
		return false, nil
	}

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "cancel",
		"arguments": map[string]any{"task_id": "t1", "reason": "user changed mind"},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "cancel on terminal task is idempotent — not an error")
	assert.Contains(t, text, "no-op")
}

func TestCompanionMCP_Catalog_ReturnsAllowedWorkflowsFromKey(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "catalog",
		"arguments": map[string]any{},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, "alpha", out["project"])
	assert.Equal(t, "claude-code", out["client_kind"])
	wfs, _ := out["workflows"].([]any)
	require.Len(t, wfs, 1, "catalog must reflect the key's allowlist exactly")
	first, _ := wfs[0].(map[string]any)
	assert.Equal(t, "wf-alpha", first["id"])
}

// TestCompanionMCP_Delegate_ETASecondsAndCostEstimate pins the
// LLD-21 §67 delegate contract ({task_id, eta_seconds, cost_estimate})
// the 2026-05-29 audit (§8.2) flagged as drifted: delegate returned a
// free-text eta_hint and no cost_estimate. eta_seconds must be numeric;
// cost_estimate must surface the historical mean when a sample exists.
func TestCompanionMCP_Delegate_ETASecondsAndCostEstimate(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})
	srv.llmUsageRepo = stubUsageRepo{meanCost: 0.1234, meanSample: 7}

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "delegate",
		"arguments": map[string]any{"workflow": "wf-alpha", "prompt": "audit this diff"},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "delegate: %s", text)
	var dout map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &dout))

	// eta_seconds is numeric (JSON numbers unmarshal to float64).
	eta, ok := dout["eta_seconds"].(float64)
	require.True(t, ok, "eta_seconds must be a number, got %T (%v)", dout["eta_seconds"], dout["eta_seconds"])
	assert.Equal(t, float64(companionDelegateETASeconds), eta)

	est, ok := dout["cost_estimate"].(map[string]any)
	require.True(t, ok, "cost_estimate must be present when a sample exists")
	assert.InDelta(t, 0.1234, est["usd"], 1e-9)
	assert.Equal(t, float64(7), est["sample_size"])
	assert.Equal(t, "historical_mean_per_task", est["basis"])
}

// TestCompanionMCP_Delegate_CostEstimateOmittedWithoutSample — when a
// workflow has no prior runs the estimate is omitted entirely rather
// than reported as a misleading $0.
func TestCompanionMCP_Delegate_CostEstimateOmittedWithoutSample(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})
	srv.llmUsageRepo = stubUsageRepo{meanSample: 0}

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "delegate",
		"arguments": map[string]any{"workflow": "wf-alpha", "prompt": "audit this diff"},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, _ := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	var dout map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &dout))
	_, present := dout["cost_estimate"]
	assert.False(t, present, "no cost_estimate when there's no historical sample")
	// eta_seconds is still present (it doesn't depend on history).
	assert.Contains(t, dout, "eta_seconds")
}

// TestCompanionMCP_Catalog_InputSchemaAndCostEstimate pins the LLD-21
// §72 catalog contract (workflows + schemas + cost estimate). Before
// the §8.2 fix catalog returned neither the delegate input schema nor
// per-workflow cost estimates.
func TestCompanionMCP_Catalog_InputSchemaAndCostEstimate(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})
	srv.llmUsageRepo = stubUsageRepo{meanCost: 0.05, meanSample: 3}

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "catalog",
		"arguments": map[string]any{},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "catalog: %s", text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))

	// delegate_input_schema is present and shaped like a JSON Schema
	// object naming the required workflow + prompt args.
	schema, ok := out["delegate_input_schema"].(map[string]any)
	require.True(t, ok, "catalog must surface delegate_input_schema")
	assert.Equal(t, "object", schema["type"])
	req2, _ := schema["required"].([]any)
	assert.Contains(t, req2, "workflow")
	assert.Contains(t, req2, "prompt")

	wfs, _ := out["workflows"].([]any)
	require.Len(t, wfs, 1)
	first, _ := wfs[0].(map[string]any)
	est, ok := first["cost_estimate"].(map[string]any)
	require.True(t, ok, "each workflow entry carries a cost_estimate when a sample exists")
	assert.InDelta(t, 0.05, est["usd"], 1e-9)
	assert.Equal(t, float64(3), est["sample_size"])
}

// ---- LLD 22: recall + remember -------------------------------------

// fakeMemoryCompanion is the api.MemoryCompanionAdapter test double.
// Captures the inbound call shape and returns scripted outputs so
// assertions can pin the per-tool wire contract without standing up
// a real memory subsystem.
type fakeMemoryCompanion struct {
	recallCalls []struct {
		ProjectID, Query string
		Opts             RecallOptions
	}
	recallReturn      []MemorySearchResult
	recallErr         error
	rememberCalls     []RememberInput
	rememberOut       RememberResult
	rememberErr       error
	recentMemoryCalls []struct {
		ProjectID    string
		Limit        int
		RepoScope    string
		StrictScope  bool
		OnlyUntagged bool
		ActorKind    string
		ActorID      string
	}
	recentMemoryReturn []RecentMemoryEntry
	recentMemoryErr    error
	// listScopesCalls / Return wire the new ListRepoScopes path
	// (2026-05-28 scope-investigation closure).
	listScopesCalls  []string
	listScopesReturn []RepoScopeCount
	listScopesErr    error
	// correctCalls / Return wire the memory_correct path.
	correctCalls  []CorrectInput
	correctReturn CorrectResult
	correctErr    error
}

func (f *fakeMemoryCompanion) Recall(_ context.Context, projectID, query string, opts RecallOptions) ([]MemorySearchResult, error) {
	f.recallCalls = append(f.recallCalls, struct {
		ProjectID, Query string
		Opts             RecallOptions
	}{projectID, query, opts})
	if f.recallErr != nil {
		return nil, f.recallErr
	}
	return f.recallReturn, nil
}

func (f *fakeMemoryCompanion) Remember(_ context.Context, in RememberInput) (RememberResult, error) {
	f.rememberCalls = append(f.rememberCalls, in)
	if f.rememberErr != nil {
		return RememberResult{}, f.rememberErr
	}
	return f.rememberOut, nil
}

func (f *fakeMemoryCompanion) RecentMemory(_ context.Context, projectID string, limit int, repoScope string, strictScope, onlyUntagged bool, actorKind, actorID string) ([]RecentMemoryEntry, error) {
	f.recentMemoryCalls = append(f.recentMemoryCalls, struct {
		ProjectID    string
		Limit        int
		RepoScope    string
		StrictScope  bool
		OnlyUntagged bool
		ActorKind    string
		ActorID      string
	}{projectID, limit, repoScope, strictScope, onlyUntagged, actorKind, actorID})
	if f.recentMemoryErr != nil {
		return nil, f.recentMemoryErr
	}
	return f.recentMemoryReturn, nil
}

func (f *fakeMemoryCompanion) ListRepoScopes(_ context.Context, projectID string) ([]RepoScopeCount, error) {
	f.listScopesCalls = append(f.listScopesCalls, projectID)
	if f.listScopesErr != nil {
		return nil, f.listScopesErr
	}
	return f.listScopesReturn, nil
}

func (f *fakeMemoryCompanion) Correct(_ context.Context, in CorrectInput) (CorrectResult, error) {
	f.correctCalls = append(f.correctCalls, in)
	if f.correctErr != nil {
		return CorrectResult{}, f.correctErr
	}
	return f.correctReturn, nil
}

// seedCompanionKeyWithCaps is seedCompanionKey + per-key memory caps,
// so LLD 22 tests can seed a key with memory_read / memory_write
// pre-set rather than mutating after Create() (memAPIKeyRepo stores a
// copy, so post-create mutation of the returned pointer is a no-op).
func seedCompanionKeyWithCaps(t *testing.T, repo *memAPIKeyRepo, projectID string, allowed []string, memRead, memWrite bool) (string, *persistence.APIKey) {
	t.Helper()
	raw, err := apikey.Generate(projectID)
	require.NoError(t, err)
	row := &persistence.APIKey{
		ID:               "akey-co-" + projectID,
		ProjectID:        projectID,
		Name:             "session-1",
		KeyHash:          apikey.Hash(raw),
		KeyPrefix:        apikey.DisplayPrefix(raw),
		ClientKind:       "claude-code",
		SessionLabel:     "test/laptop",
		AllowedWorkflows: allowed,
		MemoryRead:       memRead,
		MemoryWrite:      memWrite,
		CreatedAt:        time.Now().UTC(),
	}
	require.NoError(t, repo.Create(context.Background(), row))
	return raw, row
}

// TestCompanionMCP_Recall_DeniedWithoutMemoryRead — the per-key
// capability gates access at the tool boundary. A companion key with
// memory_read=false must receive a clean error directing them to ask
// the operator, not a generic auth failure.
func TestCompanionMCP_Recall_DeniedWithoutMemoryRead(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	srv.memoryCompanion = &fakeMemoryCompanion{}
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, false, false)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "recall",
		"arguments": map[string]any{"query": "anything"},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.True(t, isErr, "missing memory_read must surface as IsError tool result")
	assert.Contains(t, text, "memory_read")
}

// TestCompanionMCP_Recall_HappyPath — the recall tool ships query
// through the adapter scoped to the key's project and renders the
// returned hits as JSON. Verifies the actor stamp on the audit-options
// struct so retrieval-audit can later partition companion vs agent
// recalls.
func TestCompanionMCP_Recall_HappyPath(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fake := &fakeMemoryCompanion{
		recallReturn: []MemorySearchResult{
			{ChunkID: "ck1", ProjectID: "alpha", SourceName: "diagnostic/whatever",
				Content: "the IBKR market-data subscriptions need the bundle", Score: 0.87},
			{ChunkID: "ck2", ProjectID: "alpha", SourceName: "research/r2",
				Content: "lower-score hit", Score: 0.30},
		},
	}
	srv.memoryCompanion = fake
	raw, row := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, false)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name": "recall",
		"arguments": map[string]any{
			"query":     "ibkr",
			"limit":     5,
			"min_score": 0.5,
		},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "happy path must not flag IsError: %s", text)
	var out recallResult
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, "alpha", out.ProjectID)
	assert.Equal(t, "ibkr", out.Query)
	require.Len(t, out.Hits, 1, "min_score=0.5 must drop the 0.30 hit")
	assert.Equal(t, "ck1", out.Hits[0].ChunkID)

	require.Len(t, fake.recallCalls, 1)
	got := fake.recallCalls[0]
	assert.Equal(t, "alpha", got.ProjectID)
	assert.Equal(t, "ibkr", got.Query)
	assert.Equal(t, 5, got.Opts.Limit)
	// ActorKind carries the client_kind suffix per LLD-22 §"Provenance
	// and audit". The seed sets ClientKind=claude-code; legacy keys
	// with empty ClientKind fall back to plain "companion" (see
	// TestCompanionMCP_Recall_ActorKindLegacyKey for that path).
	assert.Equal(t, "companion:claude-code", got.Opts.ActorKind)
	assert.Equal(t, row.ID, got.Opts.ActorID)
}

// TestCompanionActorKind_LegacyEmptyClientKind — keys with an empty
// ClientKind (pre-2026.6.x rows or bare-bones creators that didn't
// stamp the field) must degrade to plain "companion" so the audit
// row still tags the actor class. Without this fallback, every
// pre-migration key would write empty-suffix rows that break the
// LLD-22 actor-split contract.
func TestCompanionActorKind_LegacyEmptyClientKind(t *testing.T) {
	got := companionActorKind(&persistence.APIKey{ID: "akey-legacy"})
	assert.Equal(t, "companion", got,
		"empty ClientKind must degrade to bare \"companion\" (no trailing colon)")
}

// TestCompanionActorKind_WithClientKind — the happy path: every modern
// key carries a ClientKind, and the audit row gets the per-client
// suffix per LLD-22 §"Provenance and audit".
func TestCompanionActorKind_WithClientKind(t *testing.T) {
	for _, kind := range []string{"claude-code", "codex", "gemini-cli", "opencode"} {
		t.Run(kind, func(t *testing.T) {
			got := companionActorKind(&persistence.APIKey{ID: "akey-x", ClientKind: kind})
			assert.Equal(t, "companion:"+kind, got)
		})
	}
}

// TestCompanionMCP_Remember_DeniedWithoutMemoryWrite — same shape as
// the recall denial test. memory_write is a hard gate at the tool
// boundary.
func TestCompanionMCP_Remember_DeniedWithoutMemoryWrite(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	srv.memoryCompanion = &fakeMemoryCompanion{}
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, false)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "remember",
		"arguments": map[string]any{"content": "something to remember"},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.True(t, isErr)
	assert.Contains(t, text, "memory_write")
}

// TestCompanionMCP_Remember_HappyPath — the remember tool routes the
// caller's content through the adapter with the key's client_kind +
// id stamped on the provenance, then renders the gate decision back.
func TestCompanionMCP_Remember_HappyPath(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fake := &fakeMemoryCompanion{
		rememberOut: RememberResult{
			Decision: "ALLOW", ArtifactID: "companion_20260527_aaaa",
			Admitted: 1,
		},
	}
	srv.memoryCompanion = fake
	raw, row := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, true)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name": "remember",
		"arguments": map[string]any{
			"content":     "Quick note: fractional shares still blocked, error 10243",
			"source_name": "companion:claude-code:note",
		},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "happy path: %s", text)
	var out rememberResult
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, "ALLOW", out.Decision)
	assert.Equal(t, "companion_20260527_aaaa", out.ArtifactID)
	assert.Equal(t, 1, out.Admitted)

	require.Len(t, fake.rememberCalls, 1)
	got := fake.rememberCalls[0]
	assert.Equal(t, "alpha", got.ProjectID)
	assert.Equal(t, "claude-code", got.ClientKind)
	assert.Equal(t, row.ID, got.KeyID)
	assert.Equal(t, "companion:claude-code:note", got.SourceName)
}

// TestCompanionMCP_Remember_TagsEncodedIntoSourceName pins the LLD-22
// `tags` arg (audit §8.1: previously dropped entirely). v1 encodes
// tags as a ';tags=a,b' suffix on source_name; this test fails if the
// arg is accepted but produces no observable effect on the deposit.
func TestCompanionMCP_Remember_TagsEncodedIntoSourceName(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fake := &fakeMemoryCompanion{rememberOut: RememberResult{Decision: "ALLOW", Admitted: 1}}
	srv.memoryCompanion = fake
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, true)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name": "remember",
		"arguments": map[string]any{
			"content":     "Decision: prefer open-weight models for swarm roles.",
			"source_name": "companion:claude-code:note",
			"tags":        []string{"architecture", "decision", "architecture"}, // dup dropped
		},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "tags deposit: %s", text)
	require.Len(t, fake.rememberCalls, 1)
	// Observable effect: the source_name carries the encoded suffix.
	assert.Equal(t, "companion:claude-code:note;tags=architecture,decision",
		fake.rememberCalls[0].SourceName)
}

// TestCompanionMCP_Remember_TagsResolveDefaultSourceName — tags must
// land even when the caller omits source_name; the handler resolves
// the role-default base so the suffix has something to attach to.
func TestCompanionMCP_Remember_TagsResolveDefaultSourceName(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fake := &fakeMemoryCompanion{rememberOut: RememberResult{Decision: "ALLOW", Admitted: 1}}
	srv.memoryCompanion = fake
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, true)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name": "remember",
		"arguments": map[string]any{
			"content": "Incident root cause: chat-proxy QueueHooks flipped task.status.",
			"tags":    []string{"incident"},
		},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	_, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr)
	require.Len(t, fake.rememberCalls, 1)
	assert.Equal(t, "companion:claude-code:note;tags=incident", fake.rememberCalls[0].SourceName)
}

// TestNormalizeTags exercises the bounding helper directly: trim,
// drop-empty, comma-strip, dedup, 32-char truncate, 10-entry cap.
func TestNormalizeTags(t *testing.T) {
	assert.Nil(t, normalizeTags(nil))
	assert.Nil(t, normalizeTags([]string{"", "   "}))
	// Comma inside a tag is replaced with a space so the encoding
	// stays unambiguous.
	assert.Equal(t, []string{"a b"}, normalizeTags([]string{"a,b"}))
	// 32-char truncation.
	long := strings.Repeat("x", 40)
	got := normalizeTags([]string{long})
	require.Len(t, got, 1)
	assert.Len(t, got[0], 32)
	// 10-entry cap.
	many := make([]string, 0, 15)
	for i := 0; i < 15; i++ {
		many = append(many, fmt.Sprintf("tag%02d", i))
	}
	assert.Len(t, normalizeTags(many), 10)
}

// TestCompanionMCP_Recall_ClassFilter pins the LLD-22 `class` arg
// (audit §8.1: previously accepted via `_ = args.Class` and ignored).
// With ContentClass now propagated onto recall hits, the filter drops
// non-matching classes. This test fails if `class` is accepted but
// produces no observable effect.
func TestCompanionMCP_Recall_ClassFilter(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fake := &fakeMemoryCompanion{
		recallReturn: []MemorySearchResult{
			{ChunkID: "ck1", ProjectID: "alpha", SourceName: "s1", Content: "a decision", Score: 0.9, ContentClass: "decision"},
			{ChunkID: "ck2", ProjectID: "alpha", SourceName: "s2", Content: "a note", Score: 0.8, ContentClass: "companion_note"},
			{ChunkID: "ck3", ProjectID: "alpha", SourceName: "s3", Content: "another decision", Score: 0.7, ContentClass: "DECISION"},
		},
	}
	srv.memoryCompanion = fake
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, false)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name": "recall",
		"arguments": map[string]any{
			"query": "anything",
			"class": "decision",
		},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "class filter: %s", text)
	var out recallResult
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	require.Len(t, out.Hits, 2, "only the two decision-class chunks survive (case-insensitive)")
	for _, h := range out.Hits {
		assert.True(t, strings.EqualFold(h.ContentClass, "decision"), "hit %s class=%s", h.ChunkID, h.ContentClass)
	}
}

// TestCompanionMCP_Remember_BudgetCapBlocks pins the LLD-22 per-key
// budget cap on remember (audit §8.1: BudgetCapUSD stored but never
// consulted). A key at/over its cap is refused with BUDGET_EXCEEDED
// before any deposit reaches the adapter.
func TestCompanionMCP_Remember_BudgetCapBlocks(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fake := &fakeMemoryCompanion{rememberOut: RememberResult{Decision: "ALLOW", Admitted: 1}}
	srv.memoryCompanion = fake
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, true)
	cap := 0.01
	keyRepo.rows[0].BudgetCapUSD = &cap
	srv.llmUsageRepo = stubUsageRepo{spend: 5.0}

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "remember",
		"arguments": map[string]any{"content": "a note that should be refused on budget"},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.True(t, isErr, "over-cap remember must be an error; got: %s", text)
	assert.Contains(t, text, "BUDGET_EXCEEDED")
	assert.Empty(t, fake.rememberCalls, "no deposit may reach the adapter once the cap is reached")
}

// TestCompanionMCP_Remember_BudgetCapAllowsUnderCap — a key below its
// cap still deposits normally.
func TestCompanionMCP_Remember_BudgetCapAllowsUnderCap(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fake := &fakeMemoryCompanion{rememberOut: RememberResult{Decision: "ALLOW", Admitted: 1}}
	srv.memoryCompanion = fake
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, true)
	cap := 10.0
	keyRepo.rows[0].BudgetCapUSD = &cap
	srv.llmUsageRepo = stubUsageRepo{spend: 5.0}

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "remember",
		"arguments": map[string]any{"content": "a note that should be allowed under budget"},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	_, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr)
	require.Len(t, fake.rememberCalls, 1, "deposit should reach the adapter under cap")
}

// TestCompanionMCP_Remember_ContentCap — payloads over 64 KiB are
// rejected up-front to keep a single deposit bounded. Operators upload
// big content as an artifact through the agent path.
func TestCompanionMCP_Remember_ContentCap(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	srv.memoryCompanion = &fakeMemoryCompanion{}
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, true)

	big := strings.Repeat("a", rememberMaxContentBytes+1)
	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "remember",
		"arguments": map[string]any{"content": big},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.True(t, isErr)
	assert.Contains(t, text, "exceeds")
}

func TestCompanionMCP_Remember_RejectsOversizedTTL(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fake := &fakeMemoryCompanion{}
	srv.memoryCompanion = fake
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, true)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name": "remember",
		"arguments": map[string]any{
			"content":  "retention test",
			"ttl_days": rememberMaxTTLDays + 1,
		},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.True(t, isErr)
	assert.Contains(t, text, "ttl_days")
	assert.Empty(t, fake.rememberCalls)
}

func TestCompanionMCP_Remember_RejectsOversizedMetadata(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{
			name: "source_name",
			args: map[string]any{
				"source_name": strings.Repeat("s", rememberMaxSourceNameBytes+1),
			},
			want: "source_name",
		},
		{
			name: "source_name_with_tags",
			args: map[string]any{
				"source_name": strings.Repeat("s", rememberMaxSourceNameBytes-6),
				"tags":        []string{"tag"},
			},
			want: "source_name with tags",
		},
		{
			name: "class",
			args: map[string]any{
				"class": strings.Repeat("c", rememberMaxClassBytes+1),
			},
			want: "class",
		},
		{
			name: "repo_scope",
			args: map[string]any{
				"repo_scope": strings.Repeat("r", rememberMaxRepoScopeBytes+1),
			},
			want: "repo_scope",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, keyRepo, _ := newCompanionMCPServer(t)
			fake := &fakeMemoryCompanion{}
			srv.memoryCompanion = fake
			raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, true)
			tc.args["content"] = "metadata bounds test"

			req := mcpRequest(t, "tools/call", map[string]any{
				"name":      "remember",
				"arguments": tc.args,
			})
			req = withCompanionBearer(req, raw)
			rec := httptest.NewRecorder()
			srv.CompanionMCPHandler(rec, req)

			text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
			require.True(t, isErr, "oversized metadata should fail: %s", text)
			assert.Contains(t, text, tc.want)
			assert.Empty(t, fake.rememberCalls)
		})
	}
}

// TestCompanionMCP_RecentMemory_DeniedWithoutMemoryRead — gate at
// the tool boundary, same shape as recall.
func TestCompanionMCP_RecentMemory_DeniedWithoutMemoryRead(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	srv.memoryCompanion = &fakeMemoryCompanion{}
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, false, false)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "recent_memory",
		"arguments": map[string]any{},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.True(t, isErr)
	assert.Contains(t, text, "memory_read")
}

// TestCompanionMCP_RecentMemory_HappyPath — adapter returns recency-
// ordered entries; the tool snippets long content + echoes per-entry
// metadata back. Powers the SessionStart digest enrichment.
func TestCompanionMCP_RecentMemory_HappyPath(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	long := strings.Repeat("x", 500)
	fake := &fakeMemoryCompanion{
		recentMemoryReturn: []RecentMemoryEntry{
			{
				ChunkID: "ck1", SourceName: "decision/legal-review",
				ContentClass: "decision",
				Content:      "auth middleware rewrite is driven by compliance, not tech-debt",
				CreatedAt:    "2026-05-26T10:00:00Z",
			},
			{
				ChunkID: "ck2", SourceName: "companion:claude-code:note",
				ContentClass: "companion_note",
				Content:      long, // forces snippet truncation
				CreatedAt:    "2026-05-25T08:00:00Z",
			},
		},
	}
	srv.memoryCompanion = fake
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, false)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "recent_memory",
		"arguments": map[string]any{"limit": 5},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "happy path: %s", text)
	var out recentMemoryResult
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, "alpha", out.ProjectID)
	require.Len(t, out.Entries, 2)
	assert.Equal(t, "ck1", out.Entries[0].ChunkID)
	assert.Equal(t, "decision", out.Entries[0].ContentClass)
	// Snippet for the long entry must be capped + ellipsised.
	assert.LessOrEqual(t, len(out.Entries[1].Snippet), recentMemorySnippetChars+5)
	assert.Contains(t, out.Entries[1].Snippet, "…")

	require.Len(t, fake.recentMemoryCalls, 1)
	assert.Equal(t, "alpha", fake.recentMemoryCalls[0].ProjectID)
	assert.Equal(t, 5, fake.recentMemoryCalls[0].Limit)
	assert.Equal(t, "companion:claude-code", fake.recentMemoryCalls[0].ActorKind)
	assert.NotEmpty(t, fake.recentMemoryCalls[0].ActorID)
}

// TestCompanionMCP_RecentMemory_LimitClamp — limits above the tool
// cap are clamped before reaching the adapter. Keeps the digest
// payload bounded regardless of caller input.
func TestCompanionMCP_RecentMemory_LimitClamp(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fake := &fakeMemoryCompanion{}
	srv.memoryCompanion = fake
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, false)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "recent_memory",
		"arguments": map[string]any{"limit": 100},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	require.Len(t, fake.recentMemoryCalls, 1)
	assert.Equal(t, 20, fake.recentMemoryCalls[0].Limit, "limit must be clamped to 20")
}

// TestCompanionMCP_RecentMemory_OnlyUntagged — the only_untagged flag
// (RAG BUG-3) threads through to the adapter so the operator can
// enumerate the NULL-scoped retag-triage bucket list_scopes reports.
func TestCompanionMCP_RecentMemory_OnlyUntagged(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fake := &fakeMemoryCompanion{}
	srv.memoryCompanion = fake
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, false)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "recent_memory",
		"arguments": map[string]any{"only_untagged": true},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	require.Len(t, fake.recentMemoryCalls, 1)
	assert.True(t, fake.recentMemoryCalls[0].OnlyUntagged, "only_untagged must thread through to the adapter")
}

// TestCompanionMCP_Catalog_SurfacesMemoryCapabilities — catalog now
// reports memory_read / memory_write so the client knows whether to
// render the recall / remember tools in its palette.
func TestCompanionMCP_Catalog_SurfacesMemoryCapabilities(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", []string{"wf-alpha"}, true, false)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "catalog",
		"arguments": map[string]any{},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, true, out["memory_read"])
	assert.Equal(t, false, out["memory_write"])
}

// TestCompanionMCP_FeatureFlag_ReflectsFullWiring — the capabilities
// endpoint's companion-mcp flag must flip ONLY when every dependency
// is wired. This is the contract a host plugin checks to decide
// whether to attempt MCP at all.
func TestCompanionMCP_FeatureFlag_ReflectsFullWiring(t *testing.T) {
	srv, _, _ := newCompanionMCPServer(t)
	flags := srv.featureFlags()
	assert.True(t, flags["companion-mcp"],
		"companion-mcp must be true when apiKeyRepo + taskRepo + taskCreator + projectRegistry are all wired")
	assert.True(t, flags["companion-v1"])
}

// ---- result(): inline output artifacts (incident 2026-06-07) -------

// Regression 2026-06-07: result() for a COMPLETED task returned only an
// artifacts_url pointing at /api/v1/projects/{p}/tasks/{t}/artifacts —
// a route that (a) was never registered on the API router and (b) sits
// on the REST surface companion keys are confined away from
// (isCompanionAllowedPath). A customer holding only a companion key had
// NO way to retrieve the result body; the operator only recovered the
// 2026-06-07 architecture review via RAG recall. result() must inline
// the non-transcript OUTPUT artifacts directly in the MCP response.
func TestCompanionMCP_Result_InlinesOutputArtifacts(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)

	dir := t.TempDir()
	reviewPath := filepath.Join(dir, "review-20260607-c4c6.md")
	reviewBody := "# Architecture Review\n\n**Verdict:** solid, two risks.\n"
	require.NoError(t, os.WriteFile(reviewPath, []byte(reviewBody), 0o600))

	taskRepo.GetFunc = func(_ context.Context, _ string) (*persistence.Task, error) {
		return &persistence.Task{
			ID: "t1", ProjectID: "alpha",
			Status: persistence.TaskStatusCompleted,
		}, nil
	}
	size := int64(len(reviewBody))
	srv.artifactRepo = &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, f persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			require.NotNil(t, f.TaskID, "artifact listing must be task-scoped")
			assert.Equal(t, "t1", *f.TaskID)
			return []*persistence.Artifact{
				// The payload the customer wants.
				{ID: "a1", Name: "review-20260607-c4c6.md", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: reviewPath, SizeBytes: &size},
				// Execution transcript — excluded, same rule as memory ingest.
				{ID: "a2", Name: "review-response-20260607-c4c6.md", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: reviewPath},
				// INPUT class — the customer sent this; echoing it back wastes tokens.
				{ID: "a3", Name: "uploaded.md", ArtifactClass: persistence.ArtifactClassInput, StoragePath: reviewPath},
			}, nil
		},
	}

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "result",
		"arguments": map[string]any{"task_id": "t1"},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "result: %s", text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))

	assert.Equal(t, true, out["complete"])
	_, hasURL := out["artifacts_url"]
	assert.False(t, hasURL,
		"artifacts_url points at a surface companion keys cannot reach (and a route that does not exist) — it must not be advertised")

	arts, ok := out["artifacts"].([]any)
	require.True(t, ok, "artifacts must be inlined for completed tasks")
	require.Len(t, arts, 1, "only the non-transcript OUTPUT artifact belongs in the result")
	first, _ := arts[0].(map[string]any)
	assert.Equal(t, "review-20260607-c4c6.md", first["name"])
	assert.Equal(t, reviewBody, first["content"], "the artifact body must arrive inline")
}

// Companion MCP responses ride a 1 MiB body cap; inlining is budgeted.
// Oversized artifacts arrive truncated with an explicit marker rather
// than silently complete (the host LLM must know it saw a prefix).
func TestCompanionMCP_Result_TruncatesOversizedArtifact(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)

	dir := t.TempDir()
	bigPath := filepath.Join(dir, "big-report.md")
	big := strings.Repeat("x", companionResultInlineCapBytes+4096)
	require.NoError(t, os.WriteFile(bigPath, []byte(big), 0o600))

	taskRepo.GetFunc = func(_ context.Context, _ string) (*persistence.Task, error) {
		return &persistence.Task{
			ID: "t1", ProjectID: "alpha",
			Status: persistence.TaskStatusCompleted,
		}, nil
	}
	srv.artifactRepo = &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return []*persistence.Artifact{
				{ID: "a1", Name: "big-report.md", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: bigPath},
			}, nil
		},
	}

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "result",
		"arguments": map[string]any{"task_id": "t1"},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "result: %s", text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))

	arts, ok := out["artifacts"].([]any)
	require.True(t, ok)
	require.Len(t, arts, 1)
	first, _ := arts[0].(map[string]any)
	content, _ := first["content"].(string)
	assert.Len(t, content, companionResultInlineCapBytes)
	assert.Equal(t, true, first["truncated"])
}

// A wired-but-empty artifact set (or a nil artifactRepo on minimal
// deployments) must not break the terminal result shape.
func TestCompanionMCP_Result_NoArtifactRepo_StillCompletes(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	taskRepo.GetFunc = func(_ context.Context, _ string) (*persistence.Task, error) {
		return &persistence.Task{
			ID: "t1", ProjectID: "alpha",
			Status: persistence.TaskStatusCompleted,
		}, nil
	}

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "result",
		"arguments": map[string]any{"task_id": "t1"},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "result: %s", text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, true, out["complete"])
}

// --- memory_correct tool -------------------------------------------

func TestCompanionMCP_MemoryCorrect_DeniedWithoutMemoryWrite(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	srv.memoryCompanion = &fakeMemoryCompanion{}
	// memRead=true, memWrite=false → correcting must be refused.
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, false)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "memory_correct",
		"arguments": map[string]any{"wrong_claim": "x is wrong", "correction": "x is actually y"},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.True(t, isErr)
	assert.Contains(t, text, "memory_write")
}

func TestCompanionMCP_MemoryCorrect_MissingWrongClaim(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	srv.memoryCompanion = &fakeMemoryCompanion{}
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, true)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "memory_correct",
		"arguments": map[string]any{"correction": "only a correction, no claim"},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.True(t, isErr)
	assert.Contains(t, text, "wrong_claim")
}

func TestCompanionMCP_MemoryCorrect_HappyPath_RefuteAndCorrect(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fake := &fakeMemoryCompanion{
		correctReturn: CorrectResult{
			Refuted: []RefutedChunkInfo{
				{ChunkID: "c1", SourceName: "old-plan", Preview: "drop legacy_db in soak", Score: 0.91},
			},
			CorrectionChunkID: "companion_correction_aaaa",
		},
	}
	srv.memoryCompanion = fake
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, true)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name": "memory_correct",
		"arguments": map[string]any{
			"wrong_claim": "drop the old DB legacy_db in soak cleanup",
			"correction":  "legacy_db IS prod and must never be dropped",
			"max_refutes": 2,
			"repo_scope":  "github.com/grinco/vornik",
		},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "happy path: %s", text)
	var out correctResultOut
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, 1, out.RefutedCount)
	assert.Equal(t, "c1", out.Refuted[0].ChunkID)
	assert.Equal(t, "companion_correction_aaaa", out.CorrectionChunkID)

	// Adapter received project-scoped, well-formed input.
	require.Len(t, fake.correctCalls, 1)
	got := fake.correctCalls[0]
	assert.Equal(t, "alpha", got.ProjectID, "must use the key's own project, not a caller-supplied one")
	assert.Equal(t, "drop the old DB legacy_db in soak cleanup", got.WrongClaim)
	assert.Equal(t, "legacy_db IS prod and must never be dropped", got.Correction)
	assert.Equal(t, 2, got.MaxRefutes)
	assert.Equal(t, "github.com/grinco/vornik", got.RepoScope)
	assert.Equal(t, "companion:claude-code", got.ActorKind)
	assert.NotEmpty(t, got.ActorID)
}

func TestCompanionMCP_MemoryCorrect_RefuteOnly(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fake := &fakeMemoryCompanion{
		correctReturn: CorrectResult{
			Refuted: []RefutedChunkInfo{{ChunkID: "c9", SourceName: "stale", Preview: "...", Score: 0.8}},
		},
	}
	srv.memoryCompanion = fake
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, true)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "memory_correct",
		"arguments": map[string]any{"wrong_claim": "stale claim"},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "refute-only: %s", text)
	var out correctResultOut
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, 1, out.RefutedCount)
	assert.Empty(t, out.CorrectionChunkID, "refute-only: no correction chunk")
	require.Len(t, fake.correctCalls, 1)
	assert.Empty(t, fake.correctCalls[0].Correction)
}

func TestCompanionMCP_MemoryCorrect_NoMatchesGivesNote(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	srv.memoryCompanion = &fakeMemoryCompanion{correctReturn: CorrectResult{}}
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, true)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "memory_correct",
		"arguments": map[string]any{"wrong_claim": "nothing matches this"},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr)
	var out correctResultOut
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, 0, out.RefutedCount)
	assert.Contains(t, text, "\"refuted\": []", "refuted must serialise as [] not null")
	assert.Contains(t, out.Note, "nothing demoted")
}

func TestCompanionMCP_MemoryCorrect_NeedsTarget(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	srv.memoryCompanion = &fakeMemoryCompanion{}
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, true)

	// Neither wrong_claim nor chunk_ids — must error, not silently no-op.
	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "memory_correct",
		"arguments": map[string]any{"correction": "a correction with no target"},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.True(t, isErr)
	assert.Contains(t, text, "chunk_ids")
}

func TestCompanionMCP_MemoryCorrect_ByID(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fake := &fakeMemoryCompanion{
		correctReturn: CorrectResult{ByID: true, RefutedCount: 2},
	}
	srv.memoryCompanion = fake
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, true)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name": "memory_correct",
		"arguments": map[string]any{
			// dup + blank entries must be normalised away before the adapter sees them.
			"chunk_ids": []any{"chunk_a", "chunk_b", "chunk_a", "  "},
		},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "by-id happy: %s", text)
	var out correctResultOut
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, 2, out.RefutedCount)

	require.Len(t, fake.correctCalls, 1)
	got := fake.correctCalls[0]
	assert.Equal(t, "alpha", got.ProjectID)
	assert.Equal(t, []string{"chunk_a", "chunk_b"}, got.ChunkIDs, "dups + blanks must be normalised")
	assert.Empty(t, got.WrongClaim, "by-id mode does not require a claim")
}

func TestCompanionMCP_MemoryCorrect_ByID_CapsChunkIDs(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fake := &fakeMemoryCompanion{correctReturn: CorrectResult{ByID: true, RefutedCount: memoryCorrectMaxChunkIDs}}
	srv.memoryCompanion = fake
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, true)

	ids := make([]any, 0, memoryCorrectMaxChunkIDs+5)
	for i := 0; i < memoryCorrectMaxChunkIDs+5; i++ {
		ids = append(ids, fmt.Sprintf("chunk_%02d", i))
	}
	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "memory_correct",
		"arguments": map[string]any{"chunk_ids": ids},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "by-id cap: %s", text)
	require.Len(t, fake.correctCalls, 1)
	assert.Len(t, fake.correctCalls[0].ChunkIDs, memoryCorrectMaxChunkIDs)
	assert.Equal(t, "chunk_19", fake.correctCalls[0].ChunkIDs[memoryCorrectMaxChunkIDs-1])
}

func TestCompanionMCP_MemoryCorrect_ByID_PartialFlipNote(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	// Requested 2 ids but only 1 flipped (other already refuted / wrong project).
	fake := &fakeMemoryCompanion{correctReturn: CorrectResult{ByID: true, RefutedCount: 1}}
	srv.memoryCompanion = fake
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, true)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "memory_correct",
		"arguments": map[string]any{"chunk_ids": []any{"chunk_a", "chunk_b"}},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr)
	var out correctResultOut
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, 1, out.RefutedCount)
	assert.Contains(t, out.Note, "flipped 1 of 2")
}
