package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/taskcreate"
)

// TestCompanionE2E_GrantThenDelegateThenStatus stitches every phase-1
// surface together in one happy-path test:
//
//  1. POST /api/v1/admin/companion/grant mints a scoped bearer.
//  2. Capture the one-time secret.
//  3. POST /api/v1/mcp/companion (tools/call name=delegate) creates a
//     task with the companion bearer.
//  4. Capture the task_id from the tool's text result.
//  5. POST /api/v1/mcp/companion (tools/call name=status) returns the
//     task in QUEUED state.
//
// Catches contract drift across the four moving parts: admin handler,
// API-key persistence layer, MCP server, task creator.
//
// This test deliberately uses the in-package handler entrypoints
// rather than spinning up the Router middleware chain — the
// AuthMiddleware would otherwise require additional cert/config
// fixtures, and that surface is exercised by its own dedicated tests.
// We stamp the API key directly into context here, mirroring what
// AuthMiddleware does after a successful bearer match.
func TestCompanionE2E_GrantThenDelegateThenStatus(t *testing.T) {
	reg := seedRegistry(t)
	keyRepo := &memAPIKeyRepo{}
	taskRepo := &mocks.MockTaskRepository{
		// Stand-in storage so Get() resolves to the task Create()
		// just landed. Keys the most recent Create's Task by ID.
	}
	stored := map[string]*persistence.Task{}
	taskRepo.CreateFunc = func(_ context.Context, t *persistence.Task) error {
		cp := *t
		stored[t.ID] = &cp
		return nil
	}
	taskRepo.GetFunc = func(_ context.Context, id string) (*persistence.Task, error) {
		if got, ok := stored[id]; ok {
			cp := *got
			return &cp, nil
		}
		return nil, persistence.ErrNotFound
	}
	taskRepo.GetByIdempotencyKeyFunc = func(_ context.Context, _, _ string) (*persistence.Task, error) {
		return nil, persistence.ErrNotFound
	}

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

	// ---------- Phase 1: mint the companion key ------------------
	grantBody := map[string]any{
		"projectId":        "alpha",
		"clientKind":       "claude-code",
		"sessionLabel":     "e2e/laptop",
		"allowedWorkflows": []string{"wf-alpha"},
	}
	raw, _ := json.Marshal(grantBody)
	grantReq := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/companion/grant", bytes.NewReader(raw))
	grantRec := httptest.NewRecorder()
	srv.CompanionGrant(grantRec, withAuthDisabled(grantReq))

	require.Equal(t, http.StatusCreated, grantRec.Code,
		"grant must succeed; body=%s", grantRec.Body.String())

	var grant companionGrantResponse
	require.NoError(t, json.Unmarshal(grantRec.Body.Bytes(), &grant))
	// New keys carry the vornik prefix + a non-reversible project tag;
	// the raw project name must NOT appear in the secret.
	require.True(t, strings.HasPrefix(grant.Secret, "sk-vornik-"),
		"phase 1 didn't return the expected secret shape: %q", grant.Secret)
	require.NotContains(t, grant.Secret, "alpha",
		"secret leaks the raw project name: %q", grant.Secret)
	require.NotEmpty(t, grant.ID)

	// ---------- Phase 2: delegate via the MCP server -------------
	mcpBody := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "delegate",
			"arguments": map[string]any{
				"workflow": "wf-alpha",
				"prompt":   "e2e: review this small diff",
			},
		},
	}
	raw, _ = json.Marshal(mcpBody)
	delegateReq := httptest.NewRequest(http.MethodPost,
		"/api/v1/mcp/companion", bytes.NewReader(raw))
	delegateReq = withCompanionBearer(delegateReq, grant.Secret)
	delegateRec := httptest.NewRecorder()
	srv.CompanionMCPHandler(delegateRec, delegateReq)

	require.Equal(t, http.StatusOK, delegateRec.Code,
		"phase 2 (delegate) must return 200; body=%s", delegateRec.Body.String())

	delegateResp := decodeJSONRPC(t, delegateRec.Body.Bytes())
	text, isErr := decodeToolText(t, delegateResp)
	require.False(t, isErr,
		"delegate must not produce IsError; tool text was: %s", text)

	var dout map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &dout))
	taskID, _ := dout["task_id"].(string)
	require.NotEmpty(t, taskID, "delegate must return a non-empty task_id")
	assert.Equal(t, "wf-alpha", dout["workflow"])
	assert.Equal(t, "alpha", dout["project"])

	// Spot-check the persisted task carries the companion provenance
	// marker — phase-1 audit story depends on this.
	require.Len(t, stored, 1)
	for _, st := range stored {
		assert.Equal(t, persistence.TaskCreationSourceCompanion, st.CreationSource)
	}

	// ---------- Phase 3: status reads it back --------------------
	statusBody := map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "status",
			"arguments": map[string]any{"task_id": taskID},
		},
	}
	raw, _ = json.Marshal(statusBody)
	statusReq := httptest.NewRequest(http.MethodPost,
		"/api/v1/mcp/companion", bytes.NewReader(raw))
	statusReq = withCompanionBearer(statusReq, grant.Secret)
	statusRec := httptest.NewRecorder()
	srv.CompanionMCPHandler(statusRec, statusReq)

	require.Equal(t, http.StatusOK, statusRec.Code)
	statusResp := decodeJSONRPC(t, statusRec.Body.Bytes())
	statusText, statusIsErr := decodeToolText(t, statusResp)
	require.False(t, statusIsErr, "status must not produce IsError; got: %s", statusText)

	var sout map[string]any
	require.NoError(t, json.Unmarshal([]byte(statusText), &sout))
	assert.Equal(t, taskID, sout["task_id"])
	assert.Equal(t, "alpha", sout["project"])
	// New tasks start in QUEUED per taskcreate.Create defaults.
	assert.Equalf(t, "QUEUED", sout["status"],
		"phase 3 must read back the freshly-created task; full payload: %s", statusText)
}

// TestCompanionE2E_RevokedKey_DelegateFails confirms revocation
// closes the loop: a key revoked via the regular Revoke path (the
// existing apiKeyRepo.Revoke) must immediately fail subsequent MCP
// calls. Distinct from the unit test on resolveCompanionKey
// (which uses a never-existed key) — this one verifies the soft-
// delete path AuthMiddleware relies on.
func TestCompanionE2E_RevokedKey_DelegateFails(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, row := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})

	require.NoError(t, keyRepo.Revoke(context.Background(), row.ID))

	req := mcpRequest(t, "tools/call", map[string]any{
		"name": "delegate",
		"arguments": map[string]any{
			"workflow": "wf-alpha",
			"prompt":   "should never run",
		},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	resp := decodeJSONRPC(t, rec.Body.Bytes())
	require.NotNil(t, resp.Error,
		"revoked key must JSON-RPC-error, not produce a tool result")
	assert.Contains(t, resp.Error.Message, "does not match any active key")
	assert.Equal(t, 0, taskRepo.CallCount.Create,
		"no task may land in the queue when the key is revoked")
}
