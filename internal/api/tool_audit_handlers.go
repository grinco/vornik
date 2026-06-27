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

	// Truncate output to keep DB rows bounded — the agent's
	// entrypoint already trims to ~4096 chars but we apply a
	// defensive cap here too in case a future agent path streams
	// larger blobs.
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
	entry := &persistence.ToolAuditEntry{
		ID:          req.AuditID,
		ProjectID:   req.ProjectID,
		TaskID:      req.TaskID,
		ExecutionID: req.ExecutionID,
		StepID:      req.StepID,
		ToolName:    req.ToolName,
		ToolInput:   req.ToolInput,
		ToolOutput:  out,
		DurationMs:  clampedMs,
		CreatedAt:   time.Now().UTC(),
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
