package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"vornik.io/vornik/internal/persistence"
)

// WithStepPromptRepository wires the read side of step-prompt persistence:
// the outcome rows (WithExecutionStepOutcomeRepository) carry the hashes, this
// store holds the (redacted) bodies.
func WithStepPromptRepository(prompts persistence.StepPromptRepository) ServerOption {
	return func(s *Server) {
		s.stepPromptRepo = prompts
	}
}

// StepPromptResponse is GET /api/v1/executions/{id}/steps/{step}/prompt.
type StepPromptResponse struct {
	ExecutionID string                       `json:"execution_id"`
	StepID      string                       `json:"step_id"`
	RecordedAt  string                       `json:"recorded_at"`
	Hashes      persistence.StepPromptHashes `json:"hashes"`
	// Parts is keyed by part name; a part whose hash is empty is absent.
	Parts map[string]string `json:"parts"`
}

// GetExecutionStepPrompt serves what the model was TOLD at the step's first
// request — system, user, tools — as stored: redacted at write, so served
// as-is (step-prompt persistence design §7). Operator scope is the execution's
// project scope: a project-scoped key sees only its own project's executions,
// through the same check every execution read applies.
func (s *Server) GetExecutionStepPrompt(w http.ResponseWriter, r *http.Request, executionID, stepID string) {
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
	// The newest outcome row for the step: List is newest-first.
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
	if row.PromptHashes == (persistence.StepPromptHashes{}) {
		respondError(w, http.StatusNotFound, "PROMPT_NOT_RECORDED",
			"no prompt was recorded for this step — the agent image predates step-prompt persistence, or the step never reached its first model request")
		return
	}
	resp := StepPromptResponse{ExecutionID: executionID, StepID: stepID, RecordedAt: row.RecordedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		Hashes: row.PromptHashes, Parts: map[string]string{}}
	for part, hash := range map[string]string{"system": row.PromptHashes.System, "user": row.PromptHashes.User, "tools": row.PromptHashes.Tools} {
		if hash == "" {
			continue
		}
		p, err := s.stepPromptRepo.Get(r.Context(), hash)
		if err != nil {
			if errors.Is(err, persistence.ErrNotFound) {
				// Pruned, or never landed: say so per part rather than 404 the whole step.
				resp.Parts[part] = ""
				continue
			}
			respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to read prompt part")
			return
		}
		resp.Parts[part] = p.Body
	}
	respondJSON(w, http.StatusOK, resp)
}

// stepSubresourcePath parses "/steps/{step}/{sub}" out of the remaining
// path under /api/v1/executions/{id}; sub is "prompt", "exchanges", "input"
// or "result"; ok is false for any other shape.
func stepSubresourcePath(remaining string) (stepID, sub string, ok bool) {
	rest, found := strings.CutPrefix(remaining, "/steps/")
	if !found {
		return "", "", false
	}
	step, tail, found := strings.Cut(rest, "/")
	if !found || step == "" {
		return "", "", false
	}
	tail = strings.TrimSuffix(tail, "/")
	switch tail {
	case "prompt", "exchanges", "input", "result":
	default:
		return "", "", false
	}
	return step, tail, true
}
