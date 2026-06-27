package api

import (
	"errors"
	"net/http"
	"strings"

	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
)

// ExecutionHintRequest is the body shape for POST
// /api/v1/executions/{id}/hints. step_id is optional — omit (or
// pass empty string) for a "next step, any" general nudge.
type ExecutionHintRequest struct {
	StepID  string `json:"step_id,omitempty"`
	Content string `json:"content"`
}

// ExecutionHintResponse is what the handler returns on success.
// Carries the assigned ID so the UI can correlate the subsequent
// `hint_applied` live event back to its insert.
type ExecutionHintResponse struct {
	ID          string `json:"id"`
	TaskID      string `json:"task_id,omitempty"`
	ExecutionID string `json:"execution_id,omitempty"`
	StepID      string `json:"step_id,omitempty"`
	Content     string `json:"content"`
}

const maxHintContentBytes = 4096

// ExecutionHintCreate handles POST
// /api/v1/executions/{executionId}/hints. The body carries an
// optional step_id + required content; the row lands with
// applied_at=NULL and the executor picks it up at the next step
// boundary.
//
// Refusals:
//   - hint repo unwired → 503
//   - missing execution id → 400
//   - bad JSON / missing content → 400
//   - content too long → 413
//   - execution not found → 404
//   - caller can't see the execution's project → 403
func (s *Server) ExecutionHintCreate(w http.ResponseWriter, r *http.Request, executionID string) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed")
		return
	}
	if s.hintRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "HINTS_DISABLED",
			"execution hints not wired on this deployment")
		return
	}
	if executionID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "execution_id is required")
		return
	}
	if s.executionRepo == nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Execution repository not available")
		return
	}

	// Scope check before reading the body so an unauthorised
	// caller can't probe content sizes.
	exec, err := s.executionRepo.Get(r.Context(), executionID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "Execution not found")
			return
		}
		s.logger.Error().Err(err).Str("executionId", executionID).
			Msg("hint create: failed to load execution")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to load execution")
		return
	}
	if exec == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "Execution not found")
		return
	}
	if !requestAllowsProject(r, exec.ProjectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "Access denied to project")
		return
	}

	var body ExecutionHintRequest
	if err := decodeJSONBody(w, r, maxOptionalBodyBytes, &body); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"request body must be JSON: "+err.Error())
		return
	}
	body.Content = strings.TrimSpace(body.Content)
	if body.Content == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "content is required")
		return
	}
	if len(body.Content) > maxHintContentBytes {
		respondError(w, http.StatusRequestEntityTooLarge, "CONTENT_TOO_LARGE",
			"hint content exceeds 4 KiB")
		return
	}
	body.StepID = strings.TrimSpace(body.StepID)

	hint := &persistence.ExecutionHint{
		ID:          persistence.GenerateID("hint"),
		ExecutionID: executionID,
		StepID:      body.StepID,
		Content:     body.Content,
		CreatedBy:   requestOperatorID(r),
	}
	if hint.CreatedBy == "" {
		hint.CreatedBy = "anonymous"
	}
	if err := s.hintRepo.Insert(r.Context(), hint); err != nil {
		s.logger.Error().Err(err).Str("executionId", executionID).
			Msg("hint create: insert failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to persist hint")
		return
	}

	respondJSON(w, http.StatusCreated, ExecutionHintResponse{
		ID:          hint.ID,
		ExecutionID: hint.ExecutionID,
		StepID:      hint.StepID,
		Content:     hint.Content,
	})
}

// ExecutionHintList handles GET
// /api/v1/executions/{executionId}/hints. Returns all hints
// (applied + pending) newest first. Used by the live view's
// "hint history" pane.
func (s *Server) ExecutionHintList(w http.ResponseWriter, r *http.Request, executionID string) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed")
		return
	}
	if s.hintRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "HINTS_DISABLED",
			"execution hints not wired on this deployment")
		return
	}
	if executionID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "execution_id is required")
		return
	}
	if s.executionRepo == nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Execution repository not available")
		return
	}
	exec, err := s.executionRepo.Get(r.Context(), executionID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "Execution not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to load execution")
		return
	}
	if exec == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "Execution not found")
		return
	}
	if !requestAllowsProject(r, exec.ProjectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "Access denied to project")
		return
	}
	// Include task-scoped hints (which carry across retries), not
	// just execution-scoped ones — the live "hint history" pane
	// otherwise under-reports steering issued at the task level
	// (2026-05-29 LLD-drift audit §8.6). exec.TaskID is the join key.
	hints, err := s.hintRepo.ListForExecution(r.Context(), executionID, exec.TaskID)
	if err != nil {
		s.logger.Error().Err(err).Str("executionId", executionID).
			Msg("hint list: failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to list hints")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"hints": hints,
	})
}

// TaskHintCreate handles POST /api/v1/projects/{projectId}/tasks/{taskId}/hints.
//
// Task-scoped hints (2026-05-26): the row carries task_id instead of
// execution_id. Any subsequent execution for the task will consume it
// at its first step boundary — including post-retry executions. Use
// for steering a research / plan task that bounces through recover
// loops, where execution-scoped hints get orphaned when the task is
// requeued.
//
// Refusals mirror ExecutionHintCreate (repo wired, body sane, project
// access). 404 when the task isn't visible.
func (s *Server) TaskHintCreate(w http.ResponseWriter, r *http.Request, projectID, taskID string) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed")
		return
	}
	if s.hintRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "HINTS_DISABLED",
			"task hints not wired on this deployment")
		return
	}
	if taskID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "task_id is required")
		return
	}
	if s.taskRepo == nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Task repository not available")
		return
	}
	task, err := s.taskRepo.Get(r.Context(), taskID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
			return
		}
		s.logger.Error().Err(err).Str("taskId", taskID).Msg("task hint create: failed to load task")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to load task")
		return
	}
	if task == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
		return
	}
	if projectID != "" && task.ProjectID != projectID {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
		return
	}
	if !requestAllowsProject(r, task.ProjectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "Access denied to project")
		return
	}

	var body ExecutionHintRequest
	if err := decodeJSONBody(w, r, maxOptionalBodyBytes, &body); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"request body must be JSON: "+err.Error())
		return
	}
	body.Content = strings.TrimSpace(body.Content)
	if body.Content == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "content is required")
		return
	}
	if len(body.Content) > maxHintContentBytes {
		respondError(w, http.StatusRequestEntityTooLarge, "CONTENT_TOO_LARGE",
			"hint content exceeds 4 KiB")
		return
	}
	body.StepID = strings.TrimSpace(body.StepID)

	hint := &persistence.ExecutionHint{
		ID:        persistence.GenerateID("hint"),
		TaskID:    taskID,
		StepID:    body.StepID,
		Content:   body.Content,
		CreatedBy: requestOperatorID(r),
	}
	if hint.CreatedBy == "" {
		hint.CreatedBy = "anonymous"
	}
	if err := s.hintRepo.Insert(r.Context(), hint); err != nil {
		s.logger.Error().Err(err).Str("taskId", taskID).Msg("task hint create: insert failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to persist hint")
		return
	}

	// Steer keyword: `model: fallback` (or `@fallback`) in the hint
	// switches every role with a configured modelFallback onto that
	// fallback for the next retry. The override is written to the task
	// payload now so the retry the operator fires right after this POST
	// picks it up. Best-effort — a resolution/persist failure logs but
	// must not fail the hint that was already stored.
	if s.projectRegistry != nil && executor.ParseFallbackModelDirective(body.Content) {
		if applied, oerr := executor.ApplyFallbackModelOverride(r.Context(), s.projectRegistry, s.taskRepo, task); oerr != nil {
			s.logger.Warn().Err(oerr).Str("taskId", taskID).Msg("task hint: fallback-model override failed")
		} else if applied {
			s.logger.Info().Str("taskId", taskID).Msg("task hint: applied fallback-model override from steer keyword")
		}
	}

	respondJSON(w, http.StatusCreated, ExecutionHintResponse{
		ID:      hint.ID,
		TaskID:  hint.TaskID,
		StepID:  hint.StepID,
		Content: hint.Content,
	})
}

// TaskHintList handles GET /api/v1/projects/{projectId}/tasks/{taskId}/hints.
// Returns pending task-scoped hints (execution_id IS NULL, not yet
// applied) so the UI can show operators which steering messages are
// still queued for the next execution.
func (s *Server) TaskHintList(w http.ResponseWriter, r *http.Request, projectID, taskID string) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed")
		return
	}
	if s.hintRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "HINTS_DISABLED",
			"task hints not wired on this deployment")
		return
	}
	if taskID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "task_id is required")
		return
	}
	if s.taskRepo == nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Task repository not available")
		return
	}
	task, err := s.taskRepo.Get(r.Context(), taskID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to load task")
		return
	}
	if task == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
		return
	}
	if projectID != "" && task.ProjectID != projectID {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
		return
	}
	if !requestAllowsProject(r, task.ProjectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "Access denied to project")
		return
	}
	hints, err := s.hintRepo.ListPendingForTask(r.Context(), taskID)
	if err != nil {
		s.logger.Error().Err(err).Str("taskId", taskID).Msg("task hint list: failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to list hints")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"hints": hints,
	})
}
