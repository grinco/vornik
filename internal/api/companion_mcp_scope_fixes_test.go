package api

// RED tests for the 2026-05-28 companion-MCP scope investigation
// fixes. Covers four user-facing additions:
//
//   1. `strict_scope` arg on recall + recent_memory → threads
//      through to memory.SearchOptions.StrictScope / a stricter
//      ListRecentChunks variant.
//   2. `repo_scope` field on recall hits + recent_memory rows so a
//      client can SEE whether a hit is properly scoped or NULL-leak.
//   3. New `list_scopes` MCP tool — enumerates distinct repo_scope
//      values in the project's RAG with chunk counts.
//   4. Tool-description copy mentioning the NULL-as-wildcard default.
//   5. `ingest_status` field on recent_memory rows so a client
//      distinguishes "pending embedding/classification" from "ready".
//
// All tests landed RED before any code change; this file is the
// regression guard against a future refactor walking back the fix.

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- Fix #1: schema includes strict_scope ------------------------

// TestCompanionMCP_Recall_SchemaHasStrictScope — the MCP tool/list
// surface must advertise the new arg so client SDKs that generate
// types from the schema pick it up.
func TestCompanionMCP_Recall_SchemaHasStrictScope(t *testing.T) {
	defs := companionToolDefs()
	for _, d := range defs {
		if d.Name != "recall" {
			continue
		}
		props, _ := d.InputSchema["properties"].(map[string]any)
		require.NotNil(t, props, "recall schema missing properties")
		_, ok := props["strict_scope"]
		assert.True(t, ok, "recall schema must expose strict_scope arg")
		return
	}
	t.Fatal("recall tool not found in companionToolDefs")
}

func TestCompanionMCP_RecentMemory_SchemaHasStrictScope(t *testing.T) {
	defs := companionToolDefs()
	for _, d := range defs {
		if d.Name != "recent_memory" {
			continue
		}
		props, _ := d.InputSchema["properties"].(map[string]any)
		require.NotNil(t, props)
		_, ok := props["strict_scope"]
		assert.True(t, ok, "recent_memory schema must expose strict_scope arg")
		return
	}
	t.Fatal("recent_memory tool not found")
}

// ---- Fix #2: repo_scope on result envelopes ----------------------

// TestCompanionMCP_Recall_HitsCarryRepoScope — every hit's JSON
// response must include repo_scope (or echo a chunk-level scope)
// so the client can see whether it landed via the NULL-as-wildcard
// fallthrough or a properly scoped match.
func TestCompanionMCP_Recall_HitsCarryRepoScope(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", []string{"wf-alpha"}, true, false)

	scope := "github.com/test/repo"
	srv.memoryCompanion = &fakeMemoryCompanion{
		recallReturn: []MemorySearchResult{
			{ChunkID: "c1", ProjectID: "alpha", SourceName: "doc.md",
				Content: "test", Score: 0.5, RepoScope: scope},
			{ChunkID: "c2", ProjectID: "alpha", SourceName: "n.md",
				Content: "null-scoped", Score: 0.3, RepoScope: ""},
		},
	}

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "recall",
		"arguments": map[string]any{"query": "hi"},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr)

	// Decode the recall result envelope and assert repo_scope per hit.
	var resp struct {
		Hits []struct {
			ChunkID   string `json:"chunk_id"`
			RepoScope string `json:"repo_scope"`
		} `json:"hits"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	require.Len(t, resp.Hits, 2)
	assert.Equal(t, scope, resp.Hits[0].RepoScope,
		"scoped hit must surface its repo_scope")
	assert.Equal(t, "", resp.Hits[1].RepoScope,
		"NULL-scoped hit must surface empty string (operator-visible leak signal)")
}

// TestCompanionMCP_RecentMemory_RowsCarryRepoScope — same for the
// recent_memory tool.
func TestCompanionMCP_RecentMemory_RowsCarryRepoScope(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, false)

	srv.memoryCompanion = &fakeMemoryCompanion{
		recentMemoryReturn: []RecentMemoryEntry{
			{ChunkID: "c1", SourceName: "scoped.md", Content: "x",
				RepoScope: "github.com/owner/repo"},
			{ChunkID: "c2", SourceName: "null.md", Content: "y", RepoScope: ""},
		},
	}

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "recent_memory",
		"arguments": map[string]any{},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, _ := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	var resp struct {
		Entries []struct {
			ChunkID   string `json:"chunk_id"`
			RepoScope string `json:"repo_scope"`
		} `json:"entries"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	require.Len(t, resp.Entries, 2)
	assert.Equal(t, "github.com/owner/repo", resp.Entries[0].RepoScope)
	assert.Equal(t, "", resp.Entries[1].RepoScope)
}

// ---- Fix #3: new list_scopes tool --------------------------------

func TestCompanionMCP_ListScopes_SchemaPresent(t *testing.T) {
	defs := companionToolDefs()
	var found bool
	for _, d := range defs {
		if d.Name == "list_scopes" {
			found = true
			break
		}
	}
	assert.True(t, found, "list_scopes tool must be registered")
}

func TestCompanionMCP_ListScopes_ReturnsScopeInventory(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, false)
	srv.memoryCompanion = &fakeMemoryCompanion{
		listScopesReturn: []RepoScopeCount{
			{Scope: "github.com/owner/a", Chunks: 42},
			{Scope: "github.com/owner/b", Chunks: 11},
			{Scope: "", Chunks: 3}, // NULL-scope bucket
		},
	}

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "list_scopes",
		"arguments": map[string]any{},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr)

	var resp struct {
		Scopes []struct {
			Scope  string `json:"scope"`
			Chunks int    `json:"chunks"`
		} `json:"scopes"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	require.Len(t, resp.Scopes, 3)
	assert.Equal(t, "github.com/owner/a", resp.Scopes[0].Scope)
	assert.Equal(t, 42, resp.Scopes[0].Chunks)
	assert.Equal(t, "", resp.Scopes[2].Scope,
		"NULL-scope bucket must surface as empty string so operators see the leak surface")
}

// ---- Fix #4: descriptions mention NULL-as-wildcard ---------------

func TestCompanionMCP_ToolDescriptions_MentionStrictScope(t *testing.T) {
	defs := companionToolDefs()
	want := map[string]bool{"recall": false, "recent_memory": false}
	for _, d := range defs {
		if _, ok := want[d.Name]; !ok {
			continue
		}
		desc := strings.ToLower(d.Description)
		// The description must mention either "strict" (the new arg)
		// or "null" (the default-include semantics) so an operator
		// reading the schema can see why a NULL-scoped hit leaked in.
		if strings.Contains(desc, "strict") || strings.Contains(desc, "null") {
			want[d.Name] = true
		}
	}
	for name, ok := range want {
		assert.True(t, ok, "%s description should mention strict / NULL semantics", name)
	}
}

// ---- Fix #5: ingest_status on recent_memory ----------------------

// TestCompanionMCP_RecentMemory_RowsCarryIngestStatus — every row
// must include ingest_status so a client distinguishes "pending"
// from "ready". The user's report was unable to tell whether a
// fresh ingest hadn't surfaced yet vs. was genuinely absent.
func TestCompanionMCP_RecentMemory_RowsCarryIngestStatus(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, false)

	srv.memoryCompanion = &fakeMemoryCompanion{
		recentMemoryReturn: []RecentMemoryEntry{
			{ChunkID: "c1", SourceName: "ready.md", Content: "x",
				IngestStatus: "ready"},
			{ChunkID: "c2", SourceName: "embedding.md", Content: "y",
				IngestStatus: "pending_embedding"},
			{ChunkID: "c3", SourceName: "classify.md", Content: "z",
				IngestStatus: "pending_classification"},
		},
	}

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "recent_memory",
		"arguments": map[string]any{},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, _ := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	var resp struct {
		Entries []struct {
			ChunkID      string `json:"chunk_id"`
			IngestStatus string `json:"ingest_status"`
		} `json:"entries"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	require.Len(t, resp.Entries, 3)
	assert.Equal(t, "ready", resp.Entries[0].IngestStatus)
	assert.Equal(t, "pending_embedding", resp.Entries[1].IngestStatus)
	assert.Equal(t, "pending_classification", resp.Entries[2].IngestStatus)
}

// TestCompanionMCP_Recall_StrictScopeReachesAdapter — the args→
// adapter wiring test. When strict_scope=true comes off the wire,
// the adapter sees it on RecallOptions.
func TestCompanionMCP_Recall_StrictScopeReachesAdapter(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, false)
	fc := &fakeMemoryCompanion{}
	srv.memoryCompanion = fc

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "recall",
			"arguments": map[string]any{
				"query":        "x",
				"repo_scope":   "github.com/test/repo",
				"strict_scope": true,
			},
		},
	})
	req := httptest.NewRequest("POST", "/api/v1/mcp/companion", bytes.NewReader(body))
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	require.Len(t, fc.recallCalls, 1)
	assert.True(t, fc.recallCalls[0].Opts.StrictScope,
		"strict_scope=true on the wire must reach RecallOptions.StrictScope")
	// And the repo_scope still threads through.
	assert.Equal(t, "github.com/test/repo", fc.recallCalls[0].Opts.RepoScope)
}
