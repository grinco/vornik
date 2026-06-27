package api

import (
	"encoding/json"
	"net/http"

	"vornik.io/vornik/internal/persistence"
)

// llmUsageStreamRequest is the wire shape for
// POST /api/v1/internal/llm-usage. The agent's entrypoint.sh
// flushes one of these after every LLM iteration — cumulative
// numbers for the (task, step, role) row, with a deterministic
// ID so successive flushes upsert into the same DB row instead
// of inserting duplicates.
//
// The deterministic ID is the agent's responsibility:
// `tu_<task_id>_<step_id>_<role>`. Postgres' ON CONFLICT (id)
// DO UPDATE handles the upsert atomically.
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
	// than upsert into the wrong project's cost ledger.
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

	row := &persistence.TaskLLMUsage{
		ID:                  req.UsageID,
		ProjectID:           req.ProjectID,
		TaskID:              taskPtr,
		ExecutionID:         execPtr,
		StepID:              req.StepID,
		Role:                req.Role,
		Model:               req.Model,
		PromptTokens:        req.PromptTokens,
		CompletionTokens:    req.CompletionTokens,
		CacheCreationTokens: req.CacheCreationTokens,
		CacheReadTokens:     req.CacheReadTokens,
		Iterations:          req.Iterations,
		CostUSD:             req.CostUSD,
		Source:              persistence.TaskLLMUsageSourceWorkflowStep,
	}

	if err := s.llmUsageRepo.Upsert(r.Context(), row); err != nil {
		s.logger.Warn().
			Err(err).
			Str("usage_id", req.UsageID).
			Str("task_id", req.TaskID).
			Str("step_id", req.StepID).
			Msg("llm usage upsert failed")
		respondError(w, http.StatusInternalServerError, "PERSIST_FAILED", err.Error())
		return
	}

	s.observeChatCacheUsage(row)

	w.WriteHeader(http.StatusNoContent)
}
