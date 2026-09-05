package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"vornik.io/vornik/internal/persistence"
)

// GetExecutionStepInput is GET /executions/{id}/steps/{step}/input.
func (s *Server) GetExecutionStepInput(w http.ResponseWriter, r *http.Request, executionID, stepID string) {
	s.GetExecutionStepIO(w, r, executionID, stepID, persistence.StepPromptInput)
}

// GetExecutionStepResult is GET /executions/{id}/steps/{step}/result.
func (s *Server) GetExecutionStepResult(w http.ResponseWriter, r *http.Request, executionID, stepID string) {
	s.GetExecutionStepIO(w, r, executionID, stepID, persistence.StepPromptResult)
}

// GetExecutionStepIO serves one of the two files at the container boundary
// as stored (step-I/O persistence design §5): part is StepPromptInput
// (the task.json the executor handed the container) or StepPromptResult
// (the result.json the daemon read back). The body is the stored bytes
// verbatim — redacted at write, so served as-is; a reader must not treat a
// [REDACTED:…] marker as what the container saw. Scope is the execution's
// project, through the same check every execution read applies.
//
// Status codes, in the order they are decided: 405 on anything but GET;
// 404 NOT_FOUND for an unknown execution; 403 across projects; 404
// <PART>_NOT_RECORDED when the newest outcome row for the step carries no
// hash — suffixed _NO_CONTAINER when the row says no container ran, so a
// client can tell "nothing to record" from "not recorded"; 410 GONE when the
// row carries a hash the store no longer resolves (pruned by retention).
func (s *Server) GetExecutionStepIO(w http.ResponseWriter, r *http.Request, executionID, stepID string, part persistence.StepPromptPart) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	if stepID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "stepId is required")
		return
	}
	if s.executionRepo == nil || s.stepOutcomeRepo == nil || s.stepPromptRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "PROMPT_STORE_UNAVAILABLE", "step-prompt store not wired into API server")
		return
	}
	exec, err := s.executionRepo.Get(r.Context(), executionID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "Execution not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to get execution")
		return
	}
	if !requestAllowsProject(r, exec.ProjectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "Access denied to project")
		return
	}
	// The newest outcome row for the step: List is newest-first, so a retried
	// step answers for its latest attempt, as the prompt endpoint does.
	rows, err := s.stepOutcomeRepo.List(r.Context(), persistence.ExecutionStepOutcomeFilter{
		ExecutionID: &executionID, StepID: &stepID, PageSize: 1,
	})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to read step outcome")
		return
	}
	if len(rows) == 0 {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "no outcome recorded for this step")
		return
	}
	row := rows[0]
	hash := row.PromptHashes.Input
	if part == persistence.StepPromptResult {
		hash = row.PromptHashes.Result
	}
	upper := strings.ToUpper(string(part))
	if hash == "" {
		if row.ContainerExitCode == nil {
			respondError(w, http.StatusNotFound, upper+"_NOT_RECORDED_NO_CONTAINER",
				"no container ran for this step, so there is no "+string(part)+" file to serve")
			return
		}
		respondError(w, http.StatusNotFound, upper+"_NOT_RECORDED",
			"no "+string(part)+" was recorded for this step — the daemon predated step-I/O persistence when it ran, or the file exceeded the 4 MiB ceiling (see vornik_executor_step_io_skipped_total)")
		return
	}
	p, err := s.stepPromptRepo.Get(r.Context(), hash)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			respondError(w, http.StatusGone, "GONE", "the "+string(part)+" was recorded and has since been pruned by retention")
			return
		}
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to read "+string(part))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Vornik-Content-Hash", hash)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(p.Body))
}
