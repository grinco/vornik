package api

import (
	"encoding/json"
	"net/http"
	"time"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

// mismatchedTaskScopedKey reports whether the request authenticated with
// a TASK-scoped API key whose bound task differs from reqTaskID
// (Finding B3). Returns false for non-task-scoped keys (admin/operator),
// for unauthenticated/legacy requests, and when the key's task matches —
// so only the forge case (task X's key writing task Y's row) is blocked.
//
// An empty reqTaskID under a task-scoped key is also a mismatch: a
// per-task key must always identify its own task on the ingest body.
func mismatchedTaskScopedKey(r *http.Request, reqTaskID string) bool {
	if r == nil {
		return false
	}
	id := IdentityFromContext(r.Context())
	if id == nil {
		return false
	}
	row, ok := id.Extra[auth.ExtraDBKeyRow].(*persistence.APIKey)
	if !ok || row == nil {
		return false
	}
	boundTaskID, isTaskKey := persistence.TaskIDFromKeyName(row.Name)
	if !isTaskKey {
		return false
	}
	return reqTaskID != boundTaskID
}

// toolAuditStreamRequest is the wire shape for
// POST /api/v1/internal/tool-audit. The agent's entrypoint.sh
// flushes one of these per tool call as it completes — turning
// the previously-batched per-step audit (from result.json at step
// end) into a realtime stream.
//
// AuditID is the agent-side unique identifier (filename token in
// $WORKSPACE/.tool_audit/). The post-step batch reuses the same
// ID so both writers' INSERTs collide cleanly on the (id) PK
// and the second is a silent no-op via ON CONFLICT DO NOTHING.
// This makes the realtime stream non-destructive: if it fails
// for any reason (network, daemon transient), the batch path
// still persists every entry from result.json at step end.
type toolAuditStreamRequest struct {
	AuditID     string `json:"audit_id"`
	ProjectID   string `json:"project_id"`
	TaskID      string `json:"task_id"`
	ExecutionID string `json:"execution_id"`
	StepID      string `json:"step_id"`
	ToolName    string `json:"tool_name"`
	ToolInput   string `json:"tool_input"`
	ToolOutput  string `json:"tool_output"`
	DurationMS  int64  `json:"duration_ms"`
	// Outcome / OutcomeClass are the AGENT's own view of how the call ended.
	//
	// Optional, and used only as a FALLBACK: for a tool the daemon executed
	// itself (every MCP call) the daemon's own observation wins, because an
	// agent that mis-narrates a connector failure is precisely the failure
	// mode this typing exists to catch. They matter for container-local tools,
	// which the daemon never sees. See tool_outcome_buffer.go.
	Outcome      string `json:"outcome"`
	OutcomeClass string `json:"outcome_class"`
}

// IngestToolAudit handles POST /api/v1/internal/tool-audit.
// Body shape: toolAuditStreamRequest. Idempotent on AuditID.
//
// Returns 204 No Content on success. The endpoint never blocks the
// agent's tool-call code path — failure here means the post-step
// batch will catch the row instead. The agent treats 4xx/5xx as
// a logged warning, not a tool-call failure.
//
// "Internal" path because only the agent reaches it; the API key
// it uses is the same VORNIK_API_KEY env var injected at container
// startup. The handler intentionally does no further authorization
// — anyone with the key can write rows. That's the same trust
// boundary the chat-completions proxy uses.
//
// ORDERING: rows written here are counted on the tool-audit coverage census
// (vornik_tool_audit_rows_total), whose collectors are registered in
// initHTTPServer's pass-2 block — after the observability registry exists. That
// is safe TODAY only because this handler is HTTP-fed and the listener starts
// after that phase. A writer added on a NON-HTTP path (a migration, a batch
// CLI, a background consumer) could reach the seam earlier; its rows are still
// counted and flushed at Attach, and the daemon WARNs naming pre_attach_rows so
// the broken assumption is visible rather than papered over. See
// https://docs.vornik.io D3.
func (s *Server) IngestToolAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.toolAuditRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "AUDIT_NOT_CONFIGURED",
			"tool audit repo not wired; this should not happen in a production deployment")
		return
	}

	body, err := readLimitedBody(w, r, 1<<20) // 1 MiB cap
	if err != nil {
		respondError(w, http.StatusBadRequest, "READ_FAILED", err.Error())
		return
	}
	defer func() { _ = r.Body.Close() }()

	var req toolAuditStreamRequest
	if err := json.Unmarshal(body, &req); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	if req.AuditID == "" || req.ProjectID == "" || req.ToolName == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"audit_id, project_id, and tool_name are required")
		return
	}

	// Authorisation: API keys with a project allowlist must include
	// the body's project_id. Without this check an authenticated
	// caller could write audit rows for any project just by changing
	// the JSON, poisoning a project they have no scope on.
	if !requestAllowsProject(r, req.ProjectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "API key not authorised for project")
		return
	}

	// Finding B3: when the caller's key is TASK-scoped, the request's
	// task_id must equal the key's bound task. Without this a per-task
	// key for task X could forge audit rows for any sibling task Y in
	// the same project (the project check below passes, but key→task
	// binding doesn't). Mirrors the stricter check in CallMCPTool.
	// Non-task-scoped keys (admin/operator) keep project-level behavior.
	if mismatchedTaskScopedKey(r, req.TaskID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN",
			"task_id does not match the task-scoped API key")
		return
	}

	// When task_id is supplied, confirm the task actually belongs to
	// the claimed project. The agent injects task_id from its launch
	// env, so a legitimate caller is always consistent — a divergence
	// means either a bug or an attempt to write someone else's task
	// audit. Either way refuse rather than corrupt audit history.
	if req.TaskID != "" && s.taskRepo != nil {
		task, err := s.taskRepo.Get(r.Context(), req.TaskID)
		if err == nil && task != nil && task.ProjectID != req.ProjectID {
			respondError(w, http.StatusForbidden, "FORBIDDEN",
				"task_id belongs to a different project than project_id")
			return
		}
	}
	if err := s.validateExecutionTaskBinding(r.Context(), req.TaskID, req.ExecutionID); err != nil {
		respondError(w, http.StatusForbidden, "FORBIDDEN",
			"execution_id does not belong to task_id")
		return
	}

	// Truncate output to keep DB rows bounded. BACKSTOP ONLY — the agent
	// truncates first, and since 2026-08-26 it does so STRUCTURALLY
	// (truncate_tool_output_for_audit in entrypoint.sh): a JSON result has its
	// largest string field shortened and is re-emitted as valid JSON, so the
	// scraper envelope the verifier parses survives.
	//
	// This cut is a BLIND slice and must therefore never fire on such a row —
	// it would cut valid JSON straight back into invalid JSON and silently undo
	// the fix. The agent targets a budget with headroom (3900) precisely so it
	// cannot reach this ceiling. Raising the agent's budget to 4096 would
	// reintroduce the bug at the boundary.
	//
	// Kept because its stated purpose still holds: a future agent path that
	// streams a larger blob must not write an unbounded row. If that path
	// appears, make this structural too rather than widening the cap.
	//
	// See https://docs.vornik.io §3b.
	out := req.ToolOutput
	if len(out) > 4096 {
		out = out[:4096] + "…"
	}

	clampedMs := persistence.ClampToolAuditDurationMs(req.DurationMS)
	if clampedMs != req.DurationMS {
		s.logger.Warn().
			Str("audit_id", req.AuditID).
			Str("tool", req.ToolName).
			Str("execution_id", req.ExecutionID).
			Int64("reported_ms", req.DurationMS).
			Int64("clamped_to", clampedMs).
			Msg("tool audit ingest: duration_ms outside sane range — clamping (likely agent ms_now() drift)")
	}
	// Typed outcome (migration 168). The daemon's own observation of this call
	// wins over the agent's report; the agent's is used only where the daemon
	// has none, which means a tool it did not execute.
	outcome, outcomeClass := req.Outcome, req.OutcomeClass
	if observed, class, ok := s.toolOutcomes.Claim(req.ExecutionID, req.ToolName); ok {
		outcome, outcomeClass = observed, string(class)
	}
	// A NAME THAT CANNOT BE A TOOL NAME gets its own class, when nothing else
	// has classified the row. The gate already refused the call correctly; what
	// was missing is that the refusal was stored with outcome_class = '', so the
	// one failure class that means "a model is leaking reasoning markup into its
	// tool calls" was invisible to the index built to make classes queryable.
	//
	// Last, and only into an EMPTY class: a daemon observation or an agent's own
	// report is a statement about what happened, and a structural fact about the
	// name must not overwrite either. See tool_audit_name_shape.go for why shape
	// is not the message-text sniffing the connector-auth design forbids.
	if outcomeClass == "" {
		if class := classifyToolNameShape(req.ToolName); class != "" {
			outcomeClass = class
			if outcome == "" {
				outcome = "error"
			}
			s.logger.Warn().
				Str("tool", req.ToolName).
				Str("execution_id", req.ExecutionID).
				Str("step_id", req.StepID).
				Msg("tool audit ingest: tool name is structurally impossible — the model leaked markup into its tool call")
		}
	}
	entry := &persistence.ToolAuditEntry{
		ID:           req.AuditID,
		ProjectID:    req.ProjectID,
		TaskID:       req.TaskID,
		ExecutionID:  req.ExecutionID,
		StepID:       req.StepID,
		ToolName:     req.ToolName,
		ToolInput:    req.ToolInput,
		ToolOutput:   out,
		DurationMs:   clampedMs,
		Outcome:      outcome,
		OutcomeClass: outcomeClass,
		CreatedAt:    time.Now().UTC(),
	}
	if err := s.toolAuditRepo.Log(r.Context(), entry); err != nil {
		s.logger.Warn().
			Err(err).
			Str("audit_id", req.AuditID).
			Str("tool", req.ToolName).
			Msg("tool audit ingest: persist failed")
		respondError(w, http.StatusInternalServerError, "PERSIST_FAILED", err.Error())
		return
	}

	// Surface the tool call on the execution's /live stream. The daemon's
	// chat-stream tap can't see in-container agent tool calls — this per-call
	// report is the only place they reach the daemon — so publishing here is
	// what makes tool use visible live in /ui/tasks/<id>/live (the template
	// already renders tool_call_started/finished). Fire-and-forget: Publish
	// returns no error and must never block or fail the agent's ingest. The
	// tool I/O is plain text, so JSON-encode it into the RawMessage fields.
	if s.liveSub != nil && req.ExecutionID != "" {
		inJSON, _ := json.Marshal(req.ToolInput)
		outJSON, _ := json.Marshal(out)
		s.liveSub.Publish(r.Context(), req.ExecutionID, livepubsub.KindToolCallStarted,
			livepubsub.ToolCallStartedPayload{StepID: req.StepID, CallID: req.AuditID, Tool: req.ToolName, InputJSON: inJSON})
		s.liveSub.Publish(r.Context(), req.ExecutionID, livepubsub.KindToolCallFinished,
			livepubsub.ToolCallFinishedPayload{CallID: req.AuditID, OutputJSON: outJSON, DurationMs: clampedMs})
	}

	w.WriteHeader(http.StatusNoContent)
}
