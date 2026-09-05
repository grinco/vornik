package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"vornik.io/vornik/internal/persistence"
)

// ExchangeRow is one recorded model exchange as the API returns it
// (llm-exchange record/replay design §7). Request and Response are the stored
// bodies, raw, so a client can hand them on unchanged.
type ExchangeRow struct {
	Seq              int             `json:"seq"`
	Iteration        *int            `json:"iteration"`
	Model            string          `json:"model"`
	RequestHash      string          `json:"request_hash"`
	PromptTokens     int             `json:"prompt_tokens"`
	CompletionTokens int             `json:"completion_tokens"`
	DurationMs       int             `json:"duration_ms"`
	Redactions       int             `json:"redactions"`
	FinishReason     string          `json:"finish_reason"`
	RecordedAt       string          `json:"recorded_at"`
	Request          json.RawMessage `json:"request"`
	Response         json.RawMessage `json:"response"`
}

// ExchangesResponse is GET /executions/{id}/steps/{step}/exchanges.
type ExchangesResponse struct {
	ExecutionID string        `json:"execution_id"`
	StepID      string        `json:"step_id"`
	Exchanges   []ExchangeRow `json:"exchanges"`
}

// recordingLine is one line of the exported JSONL recording (design §5.1) —
// the file internal/llmreplay serves.
type recordingLine struct {
	Seq         int             `json:"seq"`
	Iteration   *int            `json:"iteration"`
	RequestHash string          `json:"request_hash"`
	Redactions  int             `json:"redactions"`
	Request     json.RawMessage `json:"request"`
	Response    json.RawMessage `json:"response"`
	Usage       struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// GetExecutionStepExchanges serves the step's recorded exchanges in seq
// order, as JSON or — with ?format=jsonl — as the recording file a replay
// server loads. An unrecorded step is an empty list, not a 404: the project
// may not have opted in.
func (s *Server) GetExecutionStepExchanges(w http.ResponseWriter, r *http.Request, executionID, stepID string) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	if stepID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "stepId is required")
		return
	}
	if s.executionRepo == nil || s.llmExchangeRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "EXCHANGE_STORE_UNAVAILABLE", "exchange store not wired into API server")
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
	rows, err := s.llmExchangeRepo.ListByStep(r.Context(), executionID, stepID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to list exchanges")
		return
	}
	if r.URL.Query().Get("format") == "jsonl" {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		for _, x := range rows {
			line := recordingLine{Seq: x.Seq, Iteration: x.Iteration, RequestHash: x.RequestHash, Redactions: x.Redactions,
				Request: rawOrNull(x.RequestJSON), Response: rawOrNull(x.ResponseJSON)}
			line.Usage.PromptTokens, line.Usage.CompletionTokens = x.PromptTokens, x.CompletionTokens
			_ = enc.Encode(line)
		}
		return
	}
	resp := ExchangesResponse{ExecutionID: executionID, StepID: stepID, Exchanges: make([]ExchangeRow, 0, len(rows))}
	for _, x := range rows {
		resp.Exchanges = append(resp.Exchanges, ExchangeRow{
			Seq: x.Seq, Iteration: x.Iteration, Model: x.Model, RequestHash: x.RequestHash,
			PromptTokens: x.PromptTokens, CompletionTokens: x.CompletionTokens, DurationMs: x.DurationMs, Redactions: x.Redactions,
			FinishReason: finishReasonOf(x.ResponseJSON),
			RecordedAt:   x.RecordedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			Request:      rawOrNull(x.RequestJSON), Response: rawOrNull(x.ResponseJSON),
		})
	}
	respondJSON(w, http.StatusOK, resp)
}

func rawOrNull(body string) json.RawMessage {
	if json.Valid([]byte(body)) {
		return json.RawMessage(body)
	}
	return json.RawMessage("null")
}

// finishReasonOf reads choices[0].finish_reason from a stored response, or
// "error" for a recorded provider error, or "" when neither parses.
func finishReasonOf(responseJSON string) string {
	var v struct {
		Error   *json.RawMessage `json:"error"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(responseJSON), &v) != nil {
		return ""
	}
	if v.Error != nil {
		return "error"
	}
	if len(v.Choices) > 0 {
		return v.Choices[0].FinishReason
	}
	return ""
}
