package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

// Migration-110 root-cause fix: a companion key may carry a
// DefaultRepoScope so that memory calls which OMIT repo_scope still land
// scoped. This closes the leak where clients without a SessionStart scope
// injector (Codex) deposited NULL-scoped chunks whenever the model forgot
// the arg. These tests pin both the pure resolver and the four MCP wire
// paths (remember / recall / recent_memory / delegate).

// TestEffectiveRepoScope is the pure resolver contract: an explicit arg
// always wins; an omitted arg falls back to the key default; both empty
// yields project-wide (""); a nil key never panics.
func TestEffectiveRepoScope(t *testing.T) {
	keyWithDefault := &persistence.APIKey{DefaultRepoScope: "github.com/grinco/vornik"}
	keyNoDefault := &persistence.APIKey{}

	cases := []struct {
		name     string
		key      *persistence.APIKey
		argScope string
		want     string
	}{
		{"explicit arg wins over key default", keyWithDefault, "github.com/grinco/headmatch", "github.com/grinco/headmatch"},
		{"omitted arg falls back to key default", keyWithDefault, "", "github.com/grinco/vornik"},
		{"whitespace-only arg falls back to key default", keyWithDefault, "   ", "github.com/grinco/vornik"},
		{"explicit arg is trimmed", keyNoDefault, "  github.com/x/y  ", "github.com/x/y"},
		{"both empty is project-wide", keyNoDefault, "", ""},
		{"nil key with arg returns arg", nil, "github.com/x/y", "github.com/x/y"},
		{"nil key without arg is project-wide", nil, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, effectiveRepoScope(tc.key, tc.argScope))
		})
	}
}

// testDefaultRepoScope is the canonical scope these tests bind on the key
// and assert flows through to each memory operation.
const testDefaultRepoScope = "github.com/grinco/vornik"

// seedCompanionKeyWithDefaultScope mints a memory-capable Codex companion
// key whose DefaultRepoScope is set at create time (the in-memory repo
// stores a copy, so post-create mutation would be a no-op).
func seedCompanionKeyWithDefaultScope(t *testing.T, repo *memAPIKeyRepo, projectID string) string {
	t.Helper()
	raw, err := apikey.Generate(projectID)
	require.NoError(t, err)
	row := &persistence.APIKey{
		ID:               "akey-codex-" + projectID,
		ProjectID:        projectID,
		Name:             "codex-session-1",
		KeyHash:          apikey.Hash(raw),
		KeyPrefix:        apikey.DisplayPrefix(raw),
		ClientKind:       "codex",
		SessionLabel:     "test/codex",
		DefaultRepoScope: testDefaultRepoScope,
		MemoryRead:       true,
		MemoryWrite:      true,
		CreatedAt:        time.Now().UTC(),
	}
	require.NoError(t, repo.Create(context.Background(), row))
	return raw
}

func callCompanionTool(t *testing.T, srv *Server, raw, tool string, args map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	req := httptest.NewRequest("POST", "/api/v1/mcp/companion", bytes.NewReader(body))
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)
}

// TestCompanionMCP_Remember_UsesKeyDefaultScope_WhenOmitted is the core
// regression for the NULL-scoped-deposit leak: a remember() with NO
// repo_scope must inherit the key's DefaultRepoScope.
func TestCompanionMCP_Remember_UsesKeyDefaultScope_WhenOmitted(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fc := &fakeMemoryCompanion{}
	srv.memoryCompanion = fc
	raw := seedCompanionKeyWithDefaultScope(t, keyRepo, "companion-example")

	callCompanionTool(t, srv, raw, "remember", map[string]any{
		"content": "a durable project note with no explicit scope",
	})

	require.Len(t, fc.rememberCalls, 1)
	assert.Equal(t, testDefaultRepoScope, fc.rememberCalls[0].RepoScope,
		"omitted repo_scope must inherit the key's DefaultRepoScope, not land NULL-scoped")
}

// TestCompanionMCP_Remember_ExplicitScopeOverridesKeyDefault — a multi-repo
// key must still honour a per-call scope.
func TestCompanionMCP_Remember_ExplicitScopeOverridesKeyDefault(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fc := &fakeMemoryCompanion{}
	srv.memoryCompanion = fc
	raw := seedCompanionKeyWithDefaultScope(t, keyRepo, "companion-example")

	callCompanionTool(t, srv, raw, "remember", map[string]any{
		"content":    "a note that belongs to a different repo",
		"repo_scope": "github.com/grinco/headmatch",
	})

	require.Len(t, fc.rememberCalls, 1)
	assert.Equal(t, "github.com/grinco/headmatch", fc.rememberCalls[0].RepoScope,
		"explicit per-call repo_scope must override the key default")
}

// TestCompanionMCP_Recall_UsesKeyDefaultScope_WhenOmitted — recall scopes
// to the key default when the caller omits it.
func TestCompanionMCP_Recall_UsesKeyDefaultScope_WhenOmitted(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fc := &fakeMemoryCompanion{}
	srv.memoryCompanion = fc
	raw := seedCompanionKeyWithDefaultScope(t, keyRepo, "companion-example")

	callCompanionTool(t, srv, raw, "recall", map[string]any{"query": "anything"})

	require.Len(t, fc.recallCalls, 1)
	assert.Equal(t, testDefaultRepoScope, fc.recallCalls[0].Opts.RepoScope,
		"omitted repo_scope on recall must inherit the key's DefaultRepoScope")
}

// TestCompanionMCP_Recall_StrictScopeAppliesToKeyDefault — strict_scope
// applies to the EFFECTIVE scope, not just the caller-supplied arg. A
// caller that omits repo_scope but sets strict_scope=true must have the
// key default both filled in AND treated strictly (NULL fallthrough
// dropped). Guards against a future change that resolves the default but
// loses the strict flag, or vice versa.
func TestCompanionMCP_Recall_StrictScopeAppliesToKeyDefault(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fc := &fakeMemoryCompanion{}
	srv.memoryCompanion = fc
	raw := seedCompanionKeyWithDefaultScope(t, keyRepo, "companion-example")

	callCompanionTool(t, srv, raw, "recall", map[string]any{
		"query":        "anything",
		"strict_scope": true,
	})

	require.Len(t, fc.recallCalls, 1)
	assert.Equal(t, testDefaultRepoScope, fc.recallCalls[0].Opts.RepoScope,
		"strict_scope omitting repo_scope must still inherit the key default")
	assert.True(t, fc.recallCalls[0].Opts.StrictScope,
		"strict_scope=true must reach the backend alongside the resolved default scope")
}

// TestCompanionMCP_RecentMemory_UsesKeyDefaultScope_WhenOmitted — the
// recent_memory digest path inherits the default too.
func TestCompanionMCP_RecentMemory_UsesKeyDefaultScope_WhenOmitted(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fc := &fakeMemoryCompanion{}
	srv.memoryCompanion = fc
	raw := seedCompanionKeyWithDefaultScope(t, keyRepo, "companion-example")

	callCompanionTool(t, srv, raw, "recent_memory", map[string]any{})

	require.Len(t, fc.recentMemoryCalls, 1)
	assert.Equal(t, testDefaultRepoScope, fc.recentMemoryCalls[0].RepoScope,
		"omitted repo_scope on recent_memory must inherit the key's DefaultRepoScope")
}

// TestCompanionMCP_Delegate_UsesKeyDefaultScope_WhenOmitted — a delegate()
// with no repo_scope stamps the key default onto the task payload so the
// executor's output-ingest hook tags emitted chunks with the right scope.
func TestCompanionMCP_Delegate_UsesKeyDefaultScope_WhenOmitted(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	// The key is memory-capable, so delegate runs its pre-delegation
	// recall hint; wire an empty adapter so that path no-ops cleanly
	// (no strong hit → hint suppressed → task still created).
	srv.memoryCompanion = &fakeMemoryCompanion{}
	// Use the registry-known project/workflow (alpha/wf-alpha) so the
	// task creator accepts the delegation; scope-default is independent
	// of the project name.
	raw := seedCompanionKeyWithDefaultScope(t, keyRepo, "alpha")

	callCompanionTool(t, srv, raw, "delegate", map[string]any{
		"workflow": "wf-alpha",
		"prompt":   "audit this diff with no explicit scope",
	})

	require.Equal(t, 1, taskRepo.CallCount.Create, "exactly one task created")
	created := taskRepo.LastCall.Task
	require.NotNil(t, created)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(created.Payload, &payload))
	taskCtx, _ := payload["context"].(map[string]any)
	require.NotNil(t, taskCtx, "task payload must carry a context block")
	assert.Equal(t, testDefaultRepoScope, taskCtx["repo_scope"],
		"omitted repo_scope on delegate must inherit the key's DefaultRepoScope")
}

// TestCompanionMCP_Recall_ExplicitScopeWithStrictScope — the remaining cell
// of the (repo_scope × strict_scope) state space: an explicit repo_scope that
// DIFFERS from the key default, combined with strict_scope=true. The explicit
// arg must win over the key default (a multi-repo key honours a per-call
// scope) AND strict_scope must still reach the backend against that explicit
// scope. Guards against a future change that, e.g., applies strict_scope only
// when the scope came from the key default.
func TestCompanionMCP_Recall_ExplicitScopeWithStrictScope(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fc := &fakeMemoryCompanion{}
	srv.memoryCompanion = fc
	raw := seedCompanionKeyWithDefaultScope(t, keyRepo, "companion-example")

	callCompanionTool(t, srv, raw, "recall", map[string]any{
		"query":        "anything",
		"repo_scope":   "github.com/grinco/headmatch",
		"strict_scope": true,
	})

	require.Len(t, fc.recallCalls, 1)
	assert.Equal(t, "github.com/grinco/headmatch", fc.recallCalls[0].Opts.RepoScope,
		"explicit per-call repo_scope must override the key default even with strict_scope set")
	assert.True(t, fc.recallCalls[0].Opts.StrictScope,
		"strict_scope=true must reach the backend against the explicit scope")
}

// TestCompanionMCP_MemoryCorrect_UsesKeyDefaultScope_WhenOmitted —
// memory_correct must resolve scope the SAME way as recall/remember/
// recent_memory: an omitted repo_scope inherits the key's DefaultRepoScope
// (migration 110) so a forget-the-arg call doesn't refute across the whole
// project. Regression for the review finding that memory_correct was the one
// memory tool still passing strings.TrimSpace(args.RepoScope) through
// directly, bypassing effectiveRepoScope.
func TestCompanionMCP_MemoryCorrect_UsesKeyDefaultScope_WhenOmitted(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fc := &fakeMemoryCompanion{}
	srv.memoryCompanion = fc
	raw := seedCompanionKeyWithDefaultScope(t, keyRepo, "companion-example")

	callCompanionTool(t, srv, raw, "memory_correct", map[string]any{
		"wrong_claim": "the prod DB is throwaway; safe to drop",
		"max_refutes": 3,
	})

	require.Len(t, fc.correctCalls, 1)
	assert.Equal(t, testDefaultRepoScope, fc.correctCalls[0].RepoScope,
		"omitted repo_scope on memory_correct must inherit the key's DefaultRepoScope")
}

// TestCompanionMCP_MemoryCorrect_ExplicitScopeOverridesKeyDefault — a
// memory_correct that names a different repo_scope must override the key
// default (a multi-repo key can target a specific scope for refutation).
func TestCompanionMCP_MemoryCorrect_ExplicitScopeOverridesKeyDefault(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fc := &fakeMemoryCompanion{}
	srv.memoryCompanion = fc
	raw := seedCompanionKeyWithDefaultScope(t, keyRepo, "companion-example")

	callCompanionTool(t, srv, raw, "memory_correct", map[string]any{
		"wrong_claim": "the prod DB is throwaway; safe to drop",
		"repo_scope":  "github.com/grinco/headmatch",
		"max_refutes": 3,
	})

	require.Len(t, fc.correctCalls, 1)
	assert.Equal(t, "github.com/grinco/headmatch", fc.correctCalls[0].RepoScope,
		"explicit per-call repo_scope must override the key default on memory_correct")
}
