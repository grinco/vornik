package api

import (
	"encoding/json"
	"net/http"

	"vornik.io/vornik/internal/llmspend"
	"vornik.io/vornik/internal/persistence"
)

// llmUsageStreamRequest is the wire shape for
// POST /api/v1/internal/llm-usage. The agent's entrypoint.sh
// flushes one of these after every LLM iteration — cumulative
// numbers for the (task, step, role) row, with a deterministic
// ID so successive flushes upsert into the same DB row instead
// of inserting duplicates.
//
// The deterministic ID is the DAEMON's responsibility as of
// 2026-08-16: the agent still sends `usage_id`, but any request
// naming a task has its id re-derived by llmspend.StepUsageID from
// the validated task/execution/step/role. The agent's shape omitted
// the execution, so a retry's stream overwrote the row of the
// execution it retried. Postgres' ON CONFLICT (id) DO UPDATE handles
// the upsert atomically either way.
//
// Why this matters: per-step `Record` rows only land in
// task_llm_usage at step finalize time. When an agent's
// container is force-killed mid-step (operator cancellation,
// daemon shutdown, OOM), the finalize path never runs and the
// cancelled task shows $0 in the cost summary. Streaming the
// cumulative usage per-iteration means the DB always has the
// latest numbers, so the UI's per-task cost panel renders
// correctly even for interrupted work.
type llmUsageStreamRequest struct {
	UsageID             string  `json:"usage_id"`
	ProjectID           string  `json:"project_id"`
	TaskID              string  `json:"task_id"`
	ExecutionID         string  `json:"execution_id"`
	StepID              string  `json:"step_id"`
	Role                string  `json:"role"`
	Model               string  `json:"model"`
	PromptTokens        int64   `json:"prompt_tokens"`
	CompletionTokens    int64   `json:"completion_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	Iterations          int     `json:"iterations"`
	CostUSD             float64 `json:"cost_usd"`
}

// IngestLLMUsage handles POST /api/v1/internal/llm-usage.
// Body shape: llmUsageStreamRequest. Idempotent UPSERT on UsageID.
//
// Returns 204 No Content on success. The endpoint never blocks
// the agent's iteration loop — failure here means the post-step
// batch (which persists from result.json) will catch the row
// instead. The agent treats 4xx/5xx as a logged warning, not an
// iteration failure.
//
// Same trust boundary as IngestToolAudit: only the agent
// container reaches this path with the daemon-injected
// VORNIK_API_KEY. No further authorisation.
func (s *Server) IngestLLMUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.llmUsageRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "LLM_USAGE_NOT_CONFIGURED",
			"llm usage repo not wired; this should not happen in a production deployment")
		return
	}

	body, err := readLimitedBody(w, r, 1<<20) // 1 MiB cap
	if err != nil {
		respondError(w, http.StatusBadRequest, "READ_FAILED", err.Error())
		return
	}
	defer func() { _ = r.Body.Close() }()

	var req llmUsageStreamRequest
	if err := json.Unmarshal(body, &req); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	if req.UsageID == "" || req.ProjectID == "" || req.Role == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"usage_id, project_id, and role are required")
		return
	}

	// Authorisation: API keys with a project allowlist must include
	// the body's project_id. Without this check an authenticated
	// caller could submit fake cost rows for any task they know the
	// ID of, poisoning budget enforcement and the cost summary UI.
	if !requestAllowsProject(r, req.ProjectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "API key not authorised for project")
		return
	}

	// Finding B3: a task-scoped key may only write its OWN task's usage
	// row. Without this a per-task key for task X could forge cost rows
	// for sibling task Y (same project), poisoning budget enforcement
	// and the spend dashboard. Non-task-scoped keys keep project-level
	// behavior.
	if mismatchedTaskScopedKey(r, req.TaskID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN",
			"task_id does not match the task-scoped API key")
		return
	}

	// When task_id is supplied, confirm the task belongs to the
	// claimed project. Mismatched project / task tuples would mean
	// either a bug or a tampering attempt; either way reject rather
	// than upsert into the wrong project's cost ledger. Keep the
	// fetched task around (rather than re-fetching below) so its
	// CreatedByAPIKeyID can be copied onto the usage row — this
	// endpoint is only ever called by the daemon-injected agent
	// container key (see doc comment above), never the original
	// caller's key, so the row's attribution has to come from the
	// task, not from this request's own auth context.
	var task *persistence.Task
	if req.TaskID != "" && s.taskRepo != nil {
		var err error
		task, err = s.taskRepo.Get(r.Context(), req.TaskID)
		if err == nil && task != nil && task.ProjectID != req.ProjectID {
			respondError(w, http.StatusForbidden, "FORBIDDEN",
				"task_id belongs to a different project than project_id")
			return
		}
		// A fetch failure does not fail the ingest — the cost is real and
		// dropping it would understate spend — but it DOES cost the row its
		// attribution, which then reads as "Unattributed" on /ui/spend with
		// nothing saying otherwise. Log it as attribution loss specifically, so
		// a growing unattributed bucket can be traced to this rather than
		// mistaken for traffic that genuinely has no key behind it.
		if err != nil || task == nil {
			s.logger.Warn().Err(err).
				Str("usage_id", req.UsageID).
				Str("task_id", req.TaskID).
				Msg("llm usage ingest: task lookup failed; recording spend WITHOUT api-key attribution")
		}
	} else if req.TaskID != "" {
		// taskRepo unwired (lean deployment): same attribution loss, different
		// cause. Named separately because the fix is configuration, not a retry.
		s.logger.Warn().
			Str("usage_id", req.UsageID).
			Str("task_id", req.TaskID).
			Msg("llm usage ingest: no task repository wired; recording spend WITHOUT api-key attribution")
	}
	if err := s.validateExecutionTaskBinding(r.Context(), req.TaskID, req.ExecutionID); err != nil {
		respondError(w, http.StatusForbidden, "FORBIDDEN",
			"execution_id does not belong to task_id")
		return
	}

	// taskID / executionID can be optional (dispatcher path doesn't
	// have an execution row), but for streaming from a step they're
	// always set. Pass through as nullable.
	var taskPtr, execPtr *string
	if req.TaskID != "" {
		taskPtr = &req.TaskID
	}
	if req.ExecutionID != "" {
		execPtr = &req.ExecutionID
	}

	// The streaming shape: a STABLE caller-supplied id, so each cumulative report
	// overwrites the last rather than adding a row. RoleOverride and CostUSD come
	// from the agent — it ran as a particular role and computed its own cost
	// inside the container.
	var apiKeyID *string
	if task != nil {
		apiKeyID = task.CreatedByAPIKeyID
	}
	in := llmspend.Input{
		ProjectID:           req.ProjectID,
		Model:               req.Model,
		PromptTokens:        int(req.PromptTokens),
		CompletionTokens:    int(req.CompletionTokens),
		TaskID:              taskPtr,
		ExecutionID:         execPtr,
		StepID:              req.StepID,
		RoleOverride:        req.Role,
		CostUSD:             &req.CostUSD,
		APIKeyID:            apiKeyID,
		Iterations:          req.Iterations,
		CacheCreationTokens: int(req.CacheCreationTokens),
		CacheReadTokens:     int(req.CacheReadTokens),
	}

	// The id is RE-DERIVED here rather than taken from req.UsageID. The agent
	// sends `tu_<task>_<step>_<role>`, which does not name the execution, so a
	// retry's stream overwrote the row of the execution it retried and erased
	// that attempt's spend. Deriving server-side from the already-validated
	// task/execution binding (checked above) fixes both writers at once — the
	// executor's finalize path derives the same id — without requiring the agent
	// container image to be redeployed in lockstep with the daemon.
	//
	// req.UsageID remains REQUIRED above: a caller that cannot name its own row
	// is malformed, and dropping the field would silently accept those.
	//
	// Only a request that names its TASK is re-derived. With an empty task the
	// derived id degenerates to `tu__<step>_<role>`, which would collide across
	// every caller that shares a step and role — strictly worse than the id the
	// caller chose. The dispatcher path has no task row, so it keeps its own.
	usageID := req.UsageID
	if req.TaskID != "" {
		usageID = llmspend.StepUsageID(req.TaskID, req.ExecutionID, req.StepID, req.Role)
	}
	if err := s.workflowStepSpend.Upsert(r.Context(), usageID, in); err != nil {
		s.logger.Warn().
			Err(err).
			Str("usage_id", usageID).
			Str("task_id", req.TaskID).
			Str("step_id", req.StepID).
			Msg("llm usage upsert failed")
		respondError(w, http.StatusInternalServerError, "PERSIST_FAILED", err.Error())
		return
	}

	s.observeChatCacheUsage(req.Model, req.Role,
		persistence.TaskLLMUsageSourceWorkflowStep,
		req.CacheCreationTokens, req.CacheReadTokens)

	w.WriteHeader(http.StatusNoContent)
}
