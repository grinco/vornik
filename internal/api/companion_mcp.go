package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/taskcreate"
)

// Companion MCP server (LLD 21). Thin JSON-RPC face on the daemon's
// existing task fabric so host-LLM plugins (Claude Code today;
// Codex / Gemini CLI / opencode tomorrow) can delegate async work
// over MCP-over-HTTP.
//
// Endpoint: POST /mcp/companion
//
// Methods accepted on the JSON-RPC envelope: initialize, tools/list,
// tools/call, notifications/initialized. Tools are the six the LLD
// names: delegate, status, result, cancel, list, catalog.
//
// Auth: the existing AuthMiddleware stamps the raw bearer key into
// context. resolveCompanionKey() looks up the APIKey row by hash and
// rejects requests whose key isn't a companion-minted one
// (ClientKind != ""). Subsequent scope enforcement reads
// AllowedWorkflows / BudgetCapUSD from the resolved row — never from
// the request body — so a tampered client can't widen its own scope.

const (
	companionMCPMaxBodyBytes = 1 << 20 // 1 MiB; tool payloads should never
	// approach this. Generous enough to carry a moderate-size diff in
	// delegate() arguments without 413-ing common workflows.

	companionMCPProtocolVersion = "2024-11-05"
	companionMCPServerName      = "vornik-companion"
	companionMCPServerVersion   = "0.1.0"

	// companionListMaxDays bounds the lookback window for list().
	// Older delegations stay queryable by ID via status()/result() —
	// the list view is for "what did I have in flight recently".
	companionListMaxDays = 14
)

// jsonRPCRequest is the inbound JSON-RPC 2.0 envelope. Only the
// subset of fields the companion server actually consumes.
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // accept int OR string
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// jsonRPCError is the err shape per the JSON-RPC 2.0 spec.
type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// jsonRPCResponse wraps either a result OR an error — never both.
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

// MCP tool-content shape. Returned by tools/call. Plain-text only for
// v1; rich content shapes (images, embedded resources) land later.
type mcpToolContent struct {
	Type string `json:"type"` // always "text" for v1
	Text string `json:"text"`
}

// mcpToolCallResult is the body shape tools/call returns. IsError
// flips when a tool reports a recoverable failure — the JSON-RPC
// envelope still has Result set so the client sees the message as
// tool output rather than transport error.
type mcpToolCallResult struct {
	Content []mcpToolContent `json:"content"`
	IsError bool             `json:"isError,omitempty"`
}

// mcpToolDef describes one tool in tools/list output. Kept slim:
// MCP clients (including Claude Code's) only need name + description
// + inputSchema to render the tool palette.
type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// CompanionMCPHandler is the entry for POST /mcp/companion. Reads
// the JSON-RPC envelope, validates auth + companion-key shape,
// dispatches to the matching tool handler, and writes the response.
//
// The handler returns ONLY JSON-RPC error envelopes on failures
// (never HTTP non-200) once the request has parsed — that's what MCP
// clients expect. Auth failures and malformed bodies are the
// pre-parse cases that legitimately HTTP-error.
func (s *Server) CompanionMCPHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// Liveness probe for tooling that hits the endpoint to
		// check reachability before opening a session. MCP clients
		// don't strictly need this; it's purely operator-friendly.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("companion MCP server alive\n"))
		return
	case http.MethodPost:
		// fall through
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer func() { _ = r.Body.Close() }()
	r.Body = http.MaxBytesReader(w, r.Body, companionMCPMaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "request body too large") {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, "read body: "+err.Error(), status)
		return
	}

	var req jsonRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONRPCError(w, nil, -32700, "parse error: "+err.Error())
		return
	}

	switch req.Method {
	case "initialize":
		// Initialize MUST work without a resolved companion key —
		// the client may be inspecting capabilities before having
		// pasted in the bearer. AuthMiddleware still gates the
		// route on a valid key shape (sk-vornik-*); we just don't
		// look up the row yet.
		//
		// Streamable HTTP clients such as Codex expect a session id
		// on initialize and echo it on subsequent requests. The
		// companion endpoint is stateless today, so a stable logical
		// session token is sufficient.
		w.Header().Set("Mcp-Session-Id", "vornik-companion")
		writeJSONRPCResult(w, req.ID, map[string]any{
			"protocolVersion": companionMCPProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{
				"name":    companionMCPServerName,
				"version": companionMCPServerVersion,
			},
		})
		return
	case "notifications/initialized":
		// Fire-and-forget; no response body per spec.
		w.WriteHeader(http.StatusAccepted)
		return
	case "tools/list":
		if _, err := s.resolveCompanionKey(r); err != nil {
			writeJSONRPCError(w, req.ID, -32000, "auth: "+err.Error())
			return
		}
		writeJSONRPCResult(w, req.ID, map[string]any{"tools": companionToolDefs()})
		return
	case "tools/call":
		s.handleCompanionToolCall(w, r, req.ID, req.Params)
		return
	default:
		writeJSONRPCError(w, req.ID, -32601, "method not found: "+req.Method)
		return
	}
}

// companionToolDefs returns the canonical tool palette. Schemas use
// JSON Schema (the MCP spec's recommended dialect). Kept inline
// rather than loaded from disk because the tool surface is small
// and the schemas double as live documentation.
func companionToolDefs() []mcpToolDef {
	return []mcpToolDef{
		{
			Name: "delegate",
			Description: "Queue an async task on vornik. Returns immediately with a task_id, a numeric " +
				"eta_seconds poll hint, and (when prior runs exist) a cost_estimate; " +
				"call status() / result() to poll, or use mode=wait_short to block up to 30s.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workflow": map[string]any{
						"type":        "string",
						"description": "Workflow ID. Must be in the key's allowedWorkflows when that field is set. Workflows with require_input_artifacts=true (see catalog()) reject delegations without inputArtifacts.",
					},
					"prompt": map[string]any{
						"type":        "string",
						"description": "Human-readable task description. Becomes context.prompt in the persisted task payload.",
					},
					"task_type": map[string]any{
						"type":        "string",
						"description": "Free-form task category. Defaults to the workflow ID when omitted.",
					},
					"mode": map[string]any{
						"type":        "string",
						"enum":        []string{"fire_and_forget", "wait_short"},
						"description": "fire_and_forget: return immediately. wait_short: block up to 30s if completes fast (not yet implemented; v1 always returns immediately).",
					},
					"repo_scope": map[string]any{
						"type":        "string",
						"description": "Optional repo scope token (migration 75). When set, the executor stamps the value onto every chunk the workflow produces (via ingestOutputArtifacts) and onto the task payload so the workflow agents can reference it. Empty = uncategorized; '*' = cross-cutting. Plugin's SessionStart hook auto-resolves this from the cwd's git remote so the operator's many repos don't pollute each other's RAG.",
					},
					"inputArtifacts": map[string]any{
						"type":        "array",
						"description": "Optional inline file attachments. Each entry: {name: \"README.md\", content: \"<base64>\"}. The daemon snapshots them via the artifact store, places the files on disk, and folds inputFiles / inputArtifactIDs / inputExtractions into context — same shape the REST POST /tasks endpoint produces. Intended use: remote-client ingestion runs where the laptop reads the file bytes locally and submits them via a slash-command bash (so the LLM never sees the base64 in its token stream). Mandatory when the target workflow sets require_input_artifacts.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"name":    map[string]any{"type": "string", "description": "Filename as it will land in the agent's /app/input/uploads/ directory."},
								"content": map[string]any{"type": "string", "description": "Base64-encoded file bytes."},
							},
							"required": []string{"name", "content"},
						},
					},
					"skip_auto_extract": map[string]any{
						"type":        "boolean",
						"description": "When true, inputArtifacts are stored as raw files only — no automatic MIME-based extraction + memory ingest at upload time. Use this when the chosen workflow is itself an ingestion workflow (companion-rag-ingest, future document-ingest) so the agent gets the raw file staged at /app/workspace/artifacts/in/ instead of finding the file already extracted and skipped. Default false preserves the Telegram/email upload shape where 'just index it' is the right behaviour.",
					},
				},
				"required": []string{"workflow", "prompt"},
			},
		},
		{
			Name:        "status",
			Description: "Return current task status (QUEUED/LEASED/RUNNING/COMPLETED/FAILED/CANCELLED).",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"task_id": map[string]any{"type": "string"}},
				"required":   []string{"task_id"},
			},
		},
		{
			Name:        "result",
			Description: "Return a completed task's output artifacts INLINE (non-transcript OUTPUT class; 64 KiB shared budget, truncated entries flagged). This is the only artifact-read surface a companion key has — companion keys cannot reach the REST API. Returns {complete:false} cleanly while the task is still in flight.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"task_id": map[string]any{"type": "string"}},
				"required":   []string{"task_id"},
			},
		},
		{
			Name:        "cancel",
			Description: "Cancel a task that hasn't completed yet. Idempotent — cancelling a terminal task is a no-op.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task_id": map[string]any{"type": "string"},
					"reason":  map[string]any{"type": "string"},
				},
				"required": []string{"task_id"},
			},
		},
		{
			Name:        "list",
			Description: "List recent companion-delegated tasks for this key's project, newest first.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 20},
					"status": map[string]any{"type": "string", "description": "Optional filter: QUEUED, RUNNING, COMPLETED, FAILED, CANCELLED."},
				},
			},
		},
		{
			Name:        "catalog",
			Description: "Return the workflow palette this key can delegate to — each with a historical cost_estimate when prior runs exist — plus the delegate_input_schema (what to send to delegate()), the budget cap, and the project ID.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			Name: "recall",
			Description: "Semantic search over this key's project memory (LLD 22). Returns ranked snippets " +
				"with provenance. Use to check what vornik already knows before paying compute for delegate(). " +
				"By default a scoped query INCLUDES NULL-scoped (uncategorized) chunks via the migration-grace " +
				"`OR repo_scope IS NULL` clause — use strict_scope=true to drop them and see only properly-tagged " +
				"results. Each hit's repo_scope field shows the chunk's actual scope (empty string = NULL-scoped).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":     map[string]any{"type": "string"},
					"limit":     map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "default": 10},
					"from_date": map[string]any{"type": "string", "description": "RFC3339; clip to >= this timestamp"},
					"to_date":   map[string]any{"type": "string", "description": "RFC3339; clip to <= this timestamp"},
					"min_score": map[string]any{"type": "number", "description": "Drop hits below this score after retrieval"},
					"class":     map[string]any{"type": "string", "description": "Filter hits to one ContentClass (e.g. 'spec', 'decision', 'companion_note'). Case-insensitive; chunks of other classes are dropped from the result set."},
					"repo_scope": map[string]any{
						"type":        "string",
						"description": "Optional repo scope filter (migration 75). Default match = this scope OR '*' OR NULL. Empty / unset = project-wide. Plugin auto-detects from cwd at session start.",
					},
					"strict_scope": map[string]any{
						"type":        "boolean",
						"description": "When true AND repo_scope is non-empty, drops the NULL-scope fallthrough. Use this to spot NULL-scoped leaks or to verify a scope is actually populated.",
					},
				},
				"required": []string{"query"},
			},
		},
		{
			Name: "remember",
			Description: "Deposit a note into this key's project memory (LLD 22). Runs the standard gate stack " +
				"(secret_scan + dedup + min_content + ...). Default class is companion_note (30-day TTL); pass " +
				"`class` (e.g. \"spec\", \"decision\", \"diagnostic\") and `ttl_days` to override per deposit. " +
				"Content cap: 64 KiB per call — larger payloads should be uploaded as an artifact and ingested " +
				"through the agent path (or via the companion-rag-ingest workflow).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content":     map[string]any{"type": "string", "maxLength": 65536},
					"source_name": map[string]any{"type": "string", "description": "Optional caller-supplied source label; defaults to 'companion:<client_kind>:note'."},
					"class": map[string]any{
						"type":        "string",
						"description": "Optional ContentClass override (e.g. 'spec', 'decision', 'diagnostic', 'research', 'summary', 'external_fetch'). Empty / unset = let the role-map classifier choose (companion-origin deposits default to 'companion_note').",
					},
					"ttl_days": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"description": "Optional per-deposit TTL override in days. <= 0 / unset uses the class-policy default (companion_note = 30 days; spec / decision typically much longer).",
					},
					"repo_scope": map[string]any{
						"type":        "string",
						"description": "Optional repo scope token partitioning this deposit within the project's RAG (migration 75). Empty = uncategorized; '*' = cross-cutting (surfaces in every scoped recall); any other string = repo token (typically the git remote URL <host>/<path> or repo basename). Set this when one project's RAG serves multiple repos so VORNIK chunks don't dilute N8N / OpenPlatform recall results and vice versa.",
					},
					"tags": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string", "maxLength": 32},
						"maxItems":    10,
						"description": "Optional free-form labels (max 10 × 32 chars). Encoded as a ';tags=a,b' suffix on the chunk's source_name so they're LIKE-queryable; use them to group related deposits (e.g. 'architecture', 'incident', a ticket id).",
					},
				},
				"required": []string{"content"},
			},
		},
		{
			Name: "recent_memory",
			Description: "Return the most recently created chunks in this key's project memory (LLD 22 Phase 2). " +
				"Powers the SessionStart digest enrichment so the host LLM opens each session knowing what the " +
				"project has been learning. Recency-ordered (newest first); no query needed. " +
				"By default a scoped query INCLUDES NULL-scoped (uncategorized) chunks via the migration-grace " +
				"clause — use strict_scope=true to drop them. Each row's ingest_status field tells you whether " +
				"the chunk is `ready` (embedded + classified, fully recallable), `pending_embedding` (just " +
				"ingested, vector search will miss it until the async worker drains), or `pending_classification` " +
				"(embedded but classifier hasn't run). The repo_scope field exposes the chunk's actual scope " +
				"(empty = NULL-scoped leak surface). To enumerate ONLY the untagged (NULL-scoped) chunks that " +
				"`list_scopes` flags for retagging, pass only_untagged=true.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 20, "default": 5},
					"repo_scope": map[string]any{
						"type":        "string",
						"description": "Optional repo scope filter (migration 75). Default match = scope OR '*' OR NULL. Plugin's SessionStart hook auto-fills with the cwd-resolved repo.",
					},
					"strict_scope": map[string]any{
						"type":        "boolean",
						"description": "When true AND repo_scope is non-empty, drops the NULL-scope fallthrough so only properly-tagged chunks appear.",
					},
					"only_untagged": map[string]any{
						"type":        "boolean",
						"description": "When true, return ONLY untagged (NULL repo_scope) chunks, ignoring repo_scope/strict_scope. The retag-triage selector: list_scopes shows the untagged count, this enumerates its contents/sources so you know what `vornikctl memory scope retag` should touch.",
					},
				},
			},
		},
		{
			Name: "list_scopes",
			Description: "Enumerate the distinct repo_scope values present in this key's project memory, with " +
				"chunk counts per scope. NULL-scoped chunks appear under the empty-string bucket so you can see " +
				"the migration-grace leak surface (chunks that match every scoped query by default). Use this " +
				"to discover what scope tokens exist before calling recall(repo_scope=...) — and to identify " +
				"NULL-scoped chunks that need retagging via `vornikctl memory scope retag`.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			Name: "memory_correct",
			Description: "Refute a wrong/stale fact in this project's memory and (optionally) deposit the correction. " +
				"Two targeting modes: (a) `wrong_claim` — hybrid-search the claim and refute the top matches " +
				"(`max_refutes` cap); or (b) `chunk_ids` — surgically refute exactly those chunk ids. Prefer chunk_ids " +
				"(from a prior recall) when authoritative corrections already exist, since a claim search would rank " +
				"those corrections highest and refute THEM instead of the stale chunk. Refuted chunks are auto-excluded " +
				"from future recall. If `correction` is given it's stored as a verified, authoritative chunk. Requires " +
				"a memory_write-scoped key. Scoped to this key's project only.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"wrong_claim": map[string]any{
						"type":        "string",
						"description": "Claim-search mode: the wrong/stale claim phrased as it appears in memory. The tool hybrid-searches this and refutes the closest matches. Provide this OR chunk_ids.",
					},
					"chunk_ids": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Surgical mode: exact chunk ids to refute (e.g. from a prior recall). Flips only these — never the claim-search top matches. Provide this OR wrong_claim.",
					},
					"correction": map[string]any{
						"type":        "string",
						"description": "Optional. The correct fact as it should read going forward (1-3 sentences with distinguishing context). Stored as a verified chunk. Omit for refute-only.",
					},
					"max_refutes": map[string]any{
						"type":        "integer",
						"description": "Claim-search mode only: cap on chunks to refute. Default 3, max 20. Ignored when chunk_ids is set.",
					},
					"repo_scope": map[string]any{
						"type":        "string",
						"description": "Optional repo scope for the deposited correction (migration 75). Typically the git remote '<host>/<path>'.",
					},
				},
			},
		},
	}
}

// handleCompanionToolCall dispatches tools/call to the matching
// tool method. Every tool resolves the companion key first so a
// non-companion bearer can't reach the tool surface.
func (s *Server) handleCompanionToolCall(w http.ResponseWriter, r *http.Request, id json.RawMessage, paramsRaw json.RawMessage) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(paramsRaw, &params); err != nil {
		writeJSONRPCError(w, id, -32602, "invalid tools/call params: "+err.Error())
		return
	}

	key, err := s.resolveCompanionKey(r)
	if err != nil {
		writeJSONRPCError(w, id, -32000, "auth: "+err.Error())
		return
	}

	ctx := r.Context()
	// Propagate the agent's task / execution origin onto the
	// context the same way CallMCPTool does, so companion tools
	// can read mcp.TaskIDHeaderKey when they need to gate on the
	// task payload (Phase C v2 VariableMemoryChunkExcluded reads
	// counterfactual.excluded_chunks from the task).
	if v := r.Header.Get("X-Task-ID"); v != "" {
		ctx = context.WithValue(ctx, mcp.TaskIDHeaderKey{}, v)
	}
	executionID := r.Header.Get("X-Execution-ID")
	if err := s.validateExecutionTaskBinding(ctx, r.Header.Get("X-Task-ID"), executionID); err != nil {
		writeJSONRPCError(w, id, -32000, "auth: X-Execution-ID does not belong to X-Task-ID")
		return
	}
	if executionID != "" {
		ctx = context.WithValue(ctx, mcp.ExecutionIDHeaderKey{}, executionID)
	}
	var (
		result  string
		toolErr error
	)
	// B-17: roll up every companion tool call into tool_audit_log so
	// the unified "everything I called" view picks them up alongside
	// agent-container tool calls. The full request/response stays in
	// the memory_*_audit tables (or task rows for delegate); this row
	// is a thin index — tool name + duration + ok/error.
	auditStart := time.Now()
	switch params.Name {
	case "delegate":
		result, toolErr = s.companionToolDelegate(ctx, key, params.Arguments)
	case "status":
		result, toolErr = s.companionToolStatus(ctx, key, params.Arguments)
	case "result":
		result, toolErr = s.companionToolResult(ctx, key, params.Arguments)
	case "cancel":
		result, toolErr = s.companionToolCancel(ctx, key, params.Arguments)
	case "list":
		result, toolErr = s.companionToolList(ctx, key, params.Arguments)
	case "catalog":
		result, toolErr = s.companionToolCatalog(ctx, key)
	case "recall":
		result, toolErr = s.companionToolRecall(ctx, key, params.Arguments)
	case "remember":
		result, toolErr = s.companionToolRemember(ctx, key, params.Arguments)
	case "recent_memory":
		result, toolErr = s.companionToolRecentMemory(ctx, key, params.Arguments)
	case "list_scopes":
		result, toolErr = s.companionToolListScopes(ctx, key)
	case "memory_correct":
		result, toolErr = s.companionToolMemoryCorrect(ctx, key, params.Arguments)
	default:
		writeJSONRPCError(w, id, -32601, "unknown tool: "+params.Name)
		return
	}
	s.recordCompanionToolAudit(ctx, key, params.Name, params.Arguments, result, toolErr, time.Since(auditStart))

	if toolErr != nil {
		// Recoverable tool error — surface as MCP tool content with
		// IsError=true (not JSON-RPC error). The host LLM sees the
		// message as feedback and can adjust.
		writeJSONRPCResult(w, id, mcpToolCallResult{
			Content: []mcpToolContent{{Type: "text", Text: toolErr.Error()}},
			IsError: true,
		})
		return
	}
	writeJSONRPCResult(w, id, mcpToolCallResult{
		Content: []mcpToolContent{{Type: "text", Text: result}},
	})
}

// resolveCompanionKey reads the raw bearer from context (set by
// AuthMiddleware), looks up the APIKey row, and verifies it carries
// companion scope. Returned errors are operator-readable.
func (s *Server) resolveCompanionKey(r *http.Request) (*persistence.APIKey, error) {
	if s.apiKeyRepo == nil {
		return nil, errors.New("companion MCP server not wired (api-key repo missing)")
	}
	raw := APIKeyFromContext(r.Context())
	if raw == "" {
		return nil, errors.New("missing or unrecognised bearer token")
	}
	row, err := s.apiKeyRepo.LookupActiveByHash(r.Context(), apikey.Hash(raw))
	if err != nil {
		if errors.Is(err, persistence.ErrAPIKeyNotFound) {
			return nil, errors.New("bearer token does not match any active key")
		}
		return nil, fmt.Errorf("lookup: %w", err)
	}
	if row.ClientKind == "" {
		// A regular non-companion sk-vornik-* key can authenticate
		// against the rest of the API but must not reach the
		// companion tool surface — its scope columns are NULL so
		// every delegate would behave as "uncapped, all workflows",
		// which is exactly the god-mode posture LLD 21 forbids.
		return nil, errors.New("bearer token is not a companion-scoped key")
	}
	return row, nil
}

// ---- tool: delegate -----------------------------------------------

const (
	// companionDelegateETASeconds is the coarse "poll again in N
	// seconds" hint returned as eta_seconds. v1 doesn't model
	// per-workflow latency, so this is a deliberately conservative
	// fixed value matching the eta_hint prose ("10-30 seconds"). A
	// future enhancement can derive it from historical run durations
	// the same way cost_estimate derives from historical spend.
	companionDelegateETASeconds = 30

	// companionCostEstimateWindowDays bounds the historical window
	// the cost estimate is computed over. Recent runs reflect the
	// current model routing + prompt shape better than ancient ones.
	companionCostEstimateWindowDays = 30
)

// estimateWorkflowCost returns the cost_estimate sub-object for a
// (project, workflow) pair, or nil when no estimate can be produced
// (usage repo unwired, query error, or no prior-run sample). The
// estimate is the historical mean per-task spend over the last
// companionCostEstimateWindowDays days; sample_size lets the client
// judge confidence. Returning nil (rather than a $0 estimate) keeps
// the client from surfacing a misleading "free" figure for a
// workflow that simply hasn't run yet. Never errors — cost hints are
// best-effort and must not fail the delegate/catalog reply.
func (s *Server) estimateWorkflowCost(ctx context.Context, projectID, workflowID string) map[string]any {
	if s.llmUsageRepo == nil || projectID == "" || workflowID == "" {
		return nil
	}
	since := time.Now().UTC().AddDate(0, 0, -companionCostEstimateWindowDays)
	mean, sample, err := s.llmUsageRepo.MeanCostByWorkflow(ctx, projectID, workflowID, since, time.Time{})
	if err != nil {
		s.logger.Warn().Err(err).Str("workflow", workflowID).Str("project", projectID).
			Msg("companion: workflow cost estimate lookup failed; omitting estimate")
		return nil
	}
	if sample == 0 {
		return nil
	}
	return map[string]any{
		"usd":         mean,
		"basis":       "historical_mean_per_task",
		"sample_size": sample,
		"window_days": companionCostEstimateWindowDays,
	}
}

type delegateArgs struct {
	Workflow        string          `json:"workflow"`
	Prompt          string          `json:"prompt"`
	TaskType        string          `json:"task_type"`
	Mode            string          `json:"mode"`
	RepoScope       string          `json:"repo_scope"`
	InputArtifacts  []InputArtifact `json:"inputArtifacts"`
	SkipAutoExtract bool            `json:"skip_auto_extract"`
}

func (s *Server) companionToolDelegate(ctx context.Context, key *persistence.APIKey, raw json.RawMessage) (string, error) {
	if s.taskCreator == nil {
		return "", errors.New("task creator not wired on this daemon")
	}
	var args delegateArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	args.Workflow = strings.TrimSpace(args.Workflow)
	args.Prompt = strings.TrimSpace(args.Prompt)
	args.TaskType = strings.TrimSpace(args.TaskType)
	if args.Workflow == "" {
		return "", errors.New("workflow is required")
	}
	if args.Prompt == "" {
		return "", errors.New("prompt is required")
	}
	if args.TaskType == "" {
		args.TaskType = args.Workflow
	}

	// Scope check: workflow must be in the key's allowlist when set.
	// nil allowlist means "every workflow the project permits" — the
	// taskCreator's own validator will reject unknown IDs.
	if len(key.AllowedWorkflows) > 0 {
		allowed := false
		for _, wf := range key.AllowedWorkflows {
			if wf == args.Workflow {
				allowed = true
				break
			}
		}
		if !allowed {
			return "", fmt.Errorf("workflow %q not in this key's allowedWorkflows", args.Workflow)
		}
	}

	// Artifact-only workflow guard (2026-06-05 rag-ingest
	// silent-skip incident). A delegate() that names file PATHS in
	// the prompt without staging inputArtifacts silently no-op'd:
	// companion-rag-ingest reads exclusively from
	// context.inputArtifacts, found nothing, and reported COMPLETED
	// with "ingestion skipped". Workflows now declare
	// require_input_artifacts in front-matter; reject artifact-less
	// delegations up front with a pointer to the staging commands.
	// Use the same registry accessor catalog() uses to read the
	// workflow definition. Unknown / ad-hoc workflows (nil lookup)
	// are NOT blocked — preserve the prior behaviour for IDs the
	// catalog doesn't know.
	if s.projectRegistry != nil {
		if wf := s.projectRegistry.GetWorkflow(args.Workflow); wf != nil &&
			wf.RequireInputArtifacts && len(args.InputArtifacts) == 0 {
			return "", fmt.Errorf("workflow %q ingests context.inputArtifacts and none were provided — file paths in the prompt are NOT uploaded; stage files via the /vornik-rag-ingest or /vornik-upload plugin commands (base64 inputArtifacts)", args.Workflow)
		}
	}

	// Per-key budget cap (finding #2 / mitigation plan §7.2).
	// see LLD § https://docs.vornik.io
	// "Bundle 4 — Cost & Observability Surface" (per-session/per-project
	// budget caps). The key
	// row carries a lifetime USD ceiling that, before this gate, was
	// stored + echoed in catalog() but never enforced — a key with
	// BudgetCapUSD=0.01 could delegate unboundedly. Sum prior spend
	// across every task this key created and refuse once the cap is
	// reached. nil cap = uncapped; nil repo (lean deployments) = skip.
	// Lifetime window, so since/until are zero (unbounded). On a query
	// error we log and fall open — the project-level budget gate in
	// taskCreator.Create is the backstop, and a transient DB blip
	// shouldn't freeze every delegate.
	if key.BudgetCapUSD != nil && s.llmUsageRepo != nil {
		spent, sumErr := s.llmUsageRepo.SumCostByAPIKey(ctx, key.ID, time.Time{}, time.Time{})
		if sumErr != nil {
			s.logger.Warn().Err(sumErr).Str("api_key_id", key.ID).
				Msg("companion delegate: budget-cap spend lookup failed; allowing (project budget gate still applies)")
		} else if spent >= *key.BudgetCapUSD {
			return "", fmt.Errorf("BUDGET_EXCEEDED: key budget cap $%.4f reached (spent $%.4f); delegate refused", *key.BudgetCapUSD, spent)
		}
	}

	// Build the task payload context. Stamp the companion session
	// markers (client_kind, session_label, key_id) so list() can
	// filter by session and the audit trail records who delegated.
	// Stamp repo_scope (migration 75) so the executor's
	// ingestOutputArtifacts hook can tag chunks with the correct
	// scope when this workflow's outputs flow into project memory.
	payload := map[string]any{
		"prompt": args.Prompt,
		"companion": map[string]any{
			"client_kind":   key.ClientKind,
			"session_label": key.SessionLabel,
			"api_key_id":    key.ID,
		},
	}
	if scope := effectiveRepoScope(key, args.RepoScope); scope != "" {
		payload["repo_scope"] = scope
	}
	rawCtx, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode payload: %w", err)
	}

	// InputArtifacts: snapshot each inline base64 payload via the
	// artifact store (same primitive used by the REST POST /tasks
	// path) and fold inputFiles / inputArtifactIDs / inputExtractions
	// into the context JSON before handing to the Creator. Without
	// this, a remote-client ingestion run has to either burn LLM
	// tokens embedding file bytes in the prompt or scp files
	// out-of-band — see 2026-05-27 incident where the
	// companion-rag-ingest workflow ghost-globbed for paths on the
	// laptop that the agent container could never see.
	if len(args.InputArtifacts) > 0 {
		if s.inputArtifactStore == nil {
			return "", errors.New("inputArtifacts supplied but input-artifact store not configured on this daemon")
		}
		// skip_auto_extract: when set, files are stored as raw INPUT
		// artifacts WITHOUT auto-extraction into project memory. Right
		// default for delegate paths whose workflow itself ingests files
		// (companion-rag-ingest, any future document-ingest). Without
		// this, the REST auto-extract chunks the file at upload time AND
		// the workflow's extraction-skip rule (workflow.go:1052) then
		// hides the raw file from the agent — the agent has nothing to
		// read and burns its iteration budget globbing. Observed 2026-05-28
		// on task_20260528134611, B-10.
		results, err := s.processInputArtifactsWithOpts(ctx, key.ProjectID, args.InputArtifacts,
			processInputArtifactsOpts{SkipAutoExtract: args.SkipAutoExtract})
		if err != nil {
			return "", fmt.Errorf("inputArtifacts: %w", err)
		}
		merged, err := mergeInputsIntoContext(rawCtx, results)
		if err != nil {
			return "", fmt.Errorf("inputArtifacts: merge into context: %w", err)
		}
		rawCtx = merged
	}

	task, err := s.taskCreator.Create(ctx, taskcreate.Params{
		ProjectID:      key.ProjectID,
		TaskType:       args.TaskType,
		WorkflowID:     args.Workflow,
		RawContext:     rawCtx,
		CreationSource: persistence.TaskCreationSourceCompanion,
	})
	if err != nil {
		// Surface taskcreate's typed errors so the host LLM sees a
		// useful message rather than "internal error".
		if ce := taskcreate.AsError(err); ce != nil {
			return "", fmt.Errorf("delegate failed: %s", ce.Message)
		}
		return "", fmt.Errorf("delegate failed: %w", err)
	}

	// v1 always returns immediately. mode=wait_short is accepted in
	// the schema for forward-compat but doesn't yet block — the
	// SessionStart hook closes the loop on next-turn.
	//
	// eta_seconds is the LLD-21 § "delegate returns {task_id,
	// eta_seconds, cost_estimate}" contract. Before the 2026-05-29
	// drift fix (§8.2) delegate returned only a free-text `eta_hint`,
	// which a client couldn't compute a poll schedule from. We now
	// emit a numeric eta_seconds (a coarse poll-again hint, not a
	// promise) AND keep eta_hint for human readability.
	out := map[string]any{
		"task_id":     task.ID,
		"status":      string(task.Status),
		"workflow":    args.Workflow,
		"project":     key.ProjectID,
		"eta_seconds": companionDelegateETASeconds,
		"eta_hint":    "poll status() in 10-30 seconds; result() once status=COMPLETED",
		"created":     task.CreatedAt.UTC().Format(time.RFC3339),
	}
	// cost_estimate (LLD-21 §67 / drift-mitigation §8.2). Historical
	// mean cost for this (project, workflow) built on the Phase-1
	// usage-attribution infra. Omitted (rather than a misleading $0)
	// when there's no prior-run sample to estimate from.
	if est := s.estimateWorkflowCost(ctx, key.ProjectID, args.Workflow); est != nil {
		out["cost_estimate"] = est
	}
	// LLD 22 Phase 2: recall_hint. When the key carries memory_read,
	// run a bounded semantic search over the prompt and attach the
	// strongest hits to the response. The host LLM can surface the
	// hint to the user as "vornik already knows X — still delegate?"
	// before the task starts consuming swarm budget.
	if hint := s.maybeBuildRecallHint(ctx, key, args.Prompt); hint != nil {
		out["recall_hint"] = hint
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return string(b), nil
}

// Recall-hint tuning. Kept as named constants in this file so the
// values are reviewable alongside the delegate path that consumes them.
const (
	// recallHintMinPromptChars skips the hint check for trivial
	// prompts ("status", "go") whose embeddings produce noisy hits.
	recallHintMinPromptChars = 16
	// recallHintScoreThreshold filters the recall result to "almost
	// certainly relevant" hits. Lower thresholds would surface noise
	// and train Claude (and operators) to ignore the hint.
	recallHintScoreThreshold = 0.7
	// recallHintSearchLimit is how many candidates the recall fetches
	// before threshold filtering. > recallHintTopN so the threshold
	// has hits to filter from.
	recallHintSearchLimit = 10
	// recallHintTopN caps how many hits we attach to the response.
	// Three is enough to make the hint actionable without bloating
	// the delegate response or leaking unrelated chunks.
	recallHintTopN = 3
	// recallHintSnippetChars truncates each hit's content so the
	// hint stays small. Claude rehydrates the full chunk via the
	// recall() tool if it wants to drill in.
	recallHintSnippetChars = 200
	// recallHintTimeout bounds the recall call so a slow embedder
	// or DB can never delay delegate beyond this. On timeout we
	// silently skip the hint — the delegate result is what matters.
	//
	// The hint uses the scored-sufficiency path (reranked recall), whose
	// LLM rerank call costs several seconds — and the score threshold above
	// only reads as calibrated [0,1] relevance when reranking ran. So the
	// budget is generous: this is the non-interactive delegate-setup path,
	// not interactive chat, and a hint that short-circuits a whole delegation
	// is worth a few seconds. When reranking is disabled the recall is fast
	// (RRF) and finishes well within this bound. On timeout the hint is
	// silently skipped (delegate proceeds), and the sufficiency loop's own
	// fail-to-best keeps a partial round usable.
	recallHintTimeout = 12 * time.Second
)

// maybeBuildRecallHint runs the bounded semantic-neighbour check that
// produces the delegate response's optional `recall_hint` field.
// Returns nil when the hint should be omitted (capability off,
// adapter unwired, prompt too short, no strong hits, timeout, error).
// Never returns an error: hint failures must be invisible to the
// delegate caller. See LLD 22 §Roadmap fit (Phase 2 deliverable).
func (s *Server) maybeBuildRecallHint(ctx context.Context, key *persistence.APIKey, prompt string) map[string]any {
	if key == nil || !key.MemoryRead {
		return nil
	}
	if s.memoryCompanion == nil {
		return nil
	}
	if len(strings.TrimSpace(prompt)) < recallHintMinPromptChars {
		return nil
	}

	hintCtx, cancel := context.WithTimeout(ctx, recallHintTimeout)
	defer cancel()
	results, err := s.memoryCompanion.Recall(hintCtx, key.ProjectID, prompt, RecallOptions{
		Limit:     recallHintSearchLimit,
		ActorKind: companionActorKind(key),
		ActorID:   key.ID,
		// Context-assembly path: route through scored-sufficiency + reranking
		// so the hint's score threshold reads calibrated [0,1] relevance. This
		// is non-interactive (background delegate setup), so the rerank latency
		// is acceptable here — unlike the interactive recall/memory_search path.
		Sufficient: true,
	})
	if err != nil {
		// Hint failures are silent. The most likely causes (embedder
		// timeout, search backend hiccup) shouldn't penalise a
		// delegate that otherwise succeeded.
		return nil
	}

	hits := make([]map[string]any, 0, recallHintTopN)
	for _, r := range results {
		if r.Score < recallHintScoreThreshold {
			continue
		}
		snippet := r.Content
		if len(snippet) > recallHintSnippetChars {
			snippet = snippet[:recallHintSnippetChars] + "…"
		}
		hits = append(hits, map[string]any{
			"chunk_id":    r.ChunkID,
			"score":       r.Score,
			"source_name": r.SourceName,
			"snippet":     snippet,
		})
		if len(hits) >= recallHintTopN {
			break
		}
	}
	if len(hits) == 0 {
		return nil
	}
	return map[string]any{
		"message": fmt.Sprintf(
			"vornik memory already has %d strong hit(s) for this prompt; "+
				"consider /vornik-companion:recall before paying for this delegation",
			len(hits)),
		"hits": hits,
	}
}

// ---- tool: status -------------------------------------------------

type taskIDArg struct {
	TaskID string `json:"task_id"`
}

func (s *Server) companionToolStatus(ctx context.Context, key *persistence.APIKey, raw json.RawMessage) (string, error) {
	if s.taskRepo == nil {
		return "", errors.New("task repo not wired")
	}
	var args taskIDArg
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	args.TaskID = strings.TrimSpace(args.TaskID)
	if args.TaskID == "" {
		return "", errors.New("task_id is required")
	}
	task, err := s.taskRepo.Get(ctx, args.TaskID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return "", fmt.Errorf("task %s not found", args.TaskID)
		}
		return "", fmt.Errorf("lookup: %w", err)
	}
	// Cross-project access guard — the key is bound to one project;
	// a task in another project must look like "not found" (don't
	// leak existence).
	if task.ProjectID != key.ProjectID {
		return "", fmt.Errorf("task %s not found", args.TaskID)
	}

	out := map[string]any{
		"task_id":    task.ID,
		"status":     string(task.Status),
		"project":    task.ProjectID,
		"workflow":   derefString(task.WorkflowID),
		"created_at": task.CreatedAt.UTC().Format(time.RFC3339),
		"attempt":    task.Attempt,
	}
	if task.LastError != nil && *task.LastError != "" {
		out["last_error"] = *task.LastError
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return string(b), nil
}

// ---- tool: result -------------------------------------------------

func (s *Server) companionToolResult(ctx context.Context, key *persistence.APIKey, raw json.RawMessage) (string, error) {
	if s.taskRepo == nil {
		return "", errors.New("task repo not wired")
	}
	var args taskIDArg
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	args.TaskID = strings.TrimSpace(args.TaskID)
	if args.TaskID == "" {
		return "", errors.New("task_id is required")
	}
	task, err := s.taskRepo.Get(ctx, args.TaskID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return "", fmt.Errorf("task %s not found", args.TaskID)
		}
		return "", fmt.Errorf("lookup: %w", err)
	}
	if task.ProjectID != key.ProjectID {
		return "", fmt.Errorf("task %s not found", args.TaskID)
	}
	if !isTerminalStatus(task.Status) {
		// Not an error — a normal poll-not-yet-done outcome the host
		// LLM should handle gracefully. We return a clean "pending"
		// shape rather than IsError so the host can pattern-match.
		out := map[string]any{
			"task_id":  task.ID,
			"status":   string(task.Status),
			"complete": false,
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		return string(b), nil
	}

	out := map[string]any{
		"task_id":  task.ID,
		"status":   string(task.Status),
		"complete": true,
		"workflow": derefString(task.WorkflowID),
		"finished": task.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if task.LastError != nil && *task.LastError != "" {
		out["last_error"] = *task.LastError
	}
	// Inline the result body. Companion keys are confined to this
	// endpoint (isCompanionAllowedPath), so the MCP response is the
	// ONLY surface the caller can read artifacts from — the pre-fix
	// artifacts_url pointed at a REST route that (a) was never
	// registered and (b) the confinement blocks, a dead-end by
	// construction (incident 2026-06-07: the architecture-review
	// verdict was only recoverable via RAG recall).
	out["artifacts"] = s.companionInlineArtifacts(ctx, task.ID)
	b, _ := json.MarshalIndent(out, "", "  ")
	return string(b), nil
}

// companionResultInlineCapBytes is the total inline budget across all
// artifacts in one result() response. Matches remember()'s 64 KiB
// content cap; the JSON-RPC envelope rides well under the 1 MiB
// companionMCPMaxBodyBytes either way.
const companionResultInlineCapBytes = 64 * 1024

// companionInlineArtifacts returns the task's non-transcript OUTPUT
// artifacts with their content inlined, newest-write order preserved
// from the repository. Content beyond the shared budget is truncated
// with an explicit marker — the host LLM must know it saw a prefix.
// Errors degrade to metadata-only entries (name + id still tell the
// host what exists); a nil repo degrades to an empty list. Never
// fails the result() call itself.
func (s *Server) companionInlineArtifacts(ctx context.Context, taskID string) []map[string]any {
	inlined := []map[string]any{}
	if s.artifactRepo == nil {
		return inlined
	}
	arts, err := s.artifactRepo.List(ctx, persistence.ArtifactFilter{
		TaskID:   &taskID,
		PageSize: 200,
	})
	if err != nil {
		s.logger.Warn().Err(err).Str("task_id", taskID).
			Msg("companion result: artifact listing failed; returning metadata-free result")
		return inlined
	}
	budget := companionResultInlineCapBytes
	for _, a := range arts {
		if a == nil || a.ArtifactClass != persistence.ArtifactClassOutput {
			continue
		}
		// Same exclusion memory ingest applies: step transcripts are
		// execution diagnostics, not the deliverable.
		if executor.IsTranscriptArtifact(a.Name) {
			continue
		}
		entry := map[string]any{
			"artifact_id": a.ID,
			"name":        a.Name,
		}
		if a.SizeBytes != nil {
			entry["size_bytes"] = *a.SizeBytes
		}
		content, rerr := s.readArtifactBytes(ctx, a)
		switch {
		case rerr != nil:
			s.logger.Warn().Err(rerr).Str("artifact_id", a.ID).
				Msg("companion result: artifact read failed; inlining metadata only")
			entry["content_error"] = "artifact body could not be read"
		case budget <= 0:
			entry["truncated"] = true
			entry["content_error"] = "inline budget exhausted by earlier artifacts"
		case len(content) > budget:
			entry["content"] = string(content[:budget])
			entry["truncated"] = true
			budget = 0
		default:
			entry["content"] = string(content)
			budget -= len(content)
		}
		inlined = append(inlined, entry)
	}
	return inlined
}

// readArtifactBytes reads an artifact body via the backend-aware opener
// when wired (local + S3), falling back to the legacy direct-disk path
// — the same fallback ladder the executor's memory ingest uses.
func (s *Server) readArtifactBytes(ctx context.Context, a *persistence.Artifact) ([]byte, error) {
	if s.artifactOpener != nil {
		rc, err := s.artifactOpener.Open(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rc.Close() }()
		return io.ReadAll(rc)
	}
	if a.StoragePath == "" {
		return nil, errors.New("artifact has no storage path and no opener is wired")
	}
	return os.ReadFile(a.StoragePath)
}

// isTerminalStatus mirrors the executor's terminal-set check; kept
// inline so the companion server doesn't pull a dependency on the
// executor package for one constant.
func isTerminalStatus(st persistence.TaskStatus) bool {
	switch st {
	case persistence.TaskStatusCompleted,
		persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled,
		persistence.TaskStatusClosed:
		return true
	}
	return false
}

// ---- tool: cancel -------------------------------------------------

type cancelArgs struct {
	TaskID string `json:"task_id"`
	Reason string `json:"reason"`
}

func (s *Server) companionToolCancel(ctx context.Context, key *persistence.APIKey, raw json.RawMessage) (string, error) {
	if s.taskRepo == nil {
		return "", errors.New("task repo not wired")
	}
	var args cancelArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	args.TaskID = strings.TrimSpace(args.TaskID)
	if args.TaskID == "" {
		return "", errors.New("task_id is required")
	}
	task, err := s.taskRepo.Get(ctx, args.TaskID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return "", fmt.Errorf("task %s not found", args.TaskID)
		}
		return "", fmt.Errorf("lookup: %w", err)
	}
	if task.ProjectID != key.ProjectID {
		return "", fmt.Errorf("task %s not found", args.TaskID)
	}
	transitioned, err := s.taskRepo.TransitionToCancelled(ctx, args.TaskID)
	if err != nil {
		return "", fmt.Errorf("cancel: %w", err)
	}
	out := map[string]any{
		"task_id":      args.TaskID,
		"cancelled":    transitioned,
		"reason_given": args.Reason,
	}
	if !transitioned {
		// Terminal state (or already cancelled). Idempotent per LLD;
		// surface as a non-error so the host LLM doesn't retry.
		out["note"] = "task was already terminal; cancel is a no-op"
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return string(b), nil
}

// ---- tool: list ---------------------------------------------------

type listArgs struct {
	Limit  int    `json:"limit"`
	Status string `json:"status"`
}

func (s *Server) companionToolList(ctx context.Context, key *persistence.APIKey, raw json.RawMessage) (string, error) {
	if s.taskRepo == nil {
		return "", errors.New("task repo not wired")
	}
	var args listArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}
	if args.Limit <= 0 || args.Limit > 100 {
		args.Limit = 20
	}

	projectID := key.ProjectID
	filter := persistence.TaskFilter{
		ProjectID: &projectID,
		PageSize:  args.Limit,
	}
	if args.Status != "" {
		st := persistence.TaskStatus(strings.ToUpper(strings.TrimSpace(args.Status)))
		filter.Status = &st
	}
	tasks, err := s.taskRepo.List(ctx, filter)
	if err != nil {
		return "", fmt.Errorf("list: %w", err)
	}

	// Filter to companion-originated rows within the lookback
	// window. The TaskFilter shape doesn't currently expose
	// CreationSource or CreatedAfter, so we post-filter here.
	// Cheap — the page is bounded by Limit.
	cutoff := time.Now().Add(-companionListMaxDays * 24 * time.Hour)
	rows := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		if t == nil || t.CreationSource != persistence.TaskCreationSourceCompanion {
			continue
		}
		if t.CreatedAt.Before(cutoff) {
			continue
		}
		rows = append(rows, map[string]any{
			"task_id":    t.ID,
			"status":     string(t.Status),
			"workflow":   derefString(t.WorkflowID),
			"created_at": t.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	b, _ := json.MarshalIndent(map[string]any{
		"project": key.ProjectID,
		"tasks":   rows,
		"total":   len(rows),
	}, "", "  ")
	return string(b), nil
}

// ---- tool: catalog ------------------------------------------------

// delegateInputSchema returns the canonical delegate() argument
// schema lifted from the tools/list definition, so catalog() can
// echo "here's exactly what to send to delegate" without the client
// cross-referencing tools/list. Workflows don't carry per-workflow
// parameter schemas in v1 — every delegate takes the same
// {workflow, prompt, ...} shape — so the schema is workflow-agnostic
// and surfaced once at the top level. LLD-21 § "catalog returns
// schemas" / drift-mitigation §8.2.
func delegateInputSchema() map[string]any {
	for _, def := range companionToolDefs() {
		if def.Name == "delegate" {
			return def.InputSchema
		}
	}
	return nil
}

func (s *Server) companionToolCatalog(ctx context.Context, key *persistence.APIKey) (string, error) {
	if s.projectRegistry == nil {
		return "", errors.New("project registry not wired")
	}
	project := s.projectRegistry.GetProject(key.ProjectID)
	if project == nil {
		return "", fmt.Errorf("project %s not found", key.ProjectID)
	}

	// Build the workflow list. If the key has an allowlist, that's
	// the catalog. Otherwise expose the project's adaptive
	// candidates (the operator-curated menu) — better signal than
	// every workflow the registry knows about, which would include
	// daemon-global workflows unrelated to companion use.
	var workflowIDs []string
	if len(key.AllowedWorkflows) > 0 {
		workflowIDs = append(workflowIDs, key.AllowedWorkflows...)
	} else {
		workflowIDs = append(workflowIDs, project.AdaptiveCandidateWorkflows...)
		if project.DefaultWorkflowID != "" {
			// Ensure the default is in the menu even if it's not in
			// the adaptive list (some projects only set defaultWorkflow).
			already := false
			for _, wf := range workflowIDs {
				if wf == project.DefaultWorkflowID {
					already = true
					break
				}
			}
			if !already {
				workflowIDs = append(workflowIDs, project.DefaultWorkflowID)
			}
		}
	}

	wfEntries := make([]map[string]any, 0, len(workflowIDs))
	for _, wfID := range workflowIDs {
		entry := map[string]any{"id": wfID}
		if wf := s.projectRegistry.GetWorkflow(wfID); wf != nil {
			entry["display_name"] = wf.DisplayName
			entry["description"] = wf.Description
			// Surface the artifact-only contract so clients know to
			// stage inputArtifacts before delegating (2026-06-05
			// rag-ingest incident). Only emitted when set, keeping
			// the entry lean for the common case.
			if wf.RequireInputArtifacts {
				entry["require_input_artifacts"] = true
			}
		}
		// cost_estimate per workflow (LLD-21 § "catalog returns ...
		// cost estimate" / drift-mitigation §8.2). Historical mean
		// per-task spend; omitted when there's no prior-run sample so
		// the client doesn't read a $0 as "free".
		if est := s.estimateWorkflowCost(ctx, key.ProjectID, wfID); est != nil {
			entry["cost_estimate"] = est
		}
		wfEntries = append(wfEntries, entry)
	}

	out := map[string]any{
		"project":     key.ProjectID,
		"client_kind": key.ClientKind,
		"workflows":   wfEntries,
		// LLD-21 § "catalog returns ... schemas". The delegate call
		// shape so the host LLM knows what to send without guessing
		// from tool descriptions. Workflow-agnostic in v1.
		"delegate_input_schema": delegateInputSchema(),
		// LLD 22: surface memory capabilities so the client renders
		// the right tool palette without trial-and-error on recall /
		// remember. Always present (booleans default false) so the
		// schema is stable across keys.
		"memory_read":  key.MemoryRead,
		"memory_write": key.MemoryWrite,
	}
	if key.BudgetCapUSD != nil {
		out["budget_cap_usd"] = *key.BudgetCapUSD
	}
	if key.SessionLabel != "" {
		out["session_label"] = key.SessionLabel
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return string(b), nil
}

// derefString returns "" when the pointer is nil, the pointee
// otherwise. Used everywhere we project an optional Task column
// into the user-visible JSON.
func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ---- JSON-RPC helpers ---------------------------------------------

func writeJSONRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	})
}

func writeJSONRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonRPCError{Code: code, Message: msg},
	})
}
