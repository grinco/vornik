package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/llmreplay"
	"vornik.io/vornik/internal/persistence"
)

// ExchangeRedactor rewrites a body through the secret-redaction seam and
// reports how many findings it replaced. Wired from the same detector the
// step-prompt store uses (auditredact.StepPrompts.RedactText); nil records
// bodies as they are.
type ExchangeRedactor func(body string) (string, int)

// WithLLMExchangeRepository wires the store the chat proxy records agent
// steps' model exchanges into, for projects that opted in
// (llm-exchange record/replay design §2–§3).
func WithLLMExchangeRepository(repo persistence.LLMExchangeRepository) ServerOption {
	return func(s *Server) { s.llmExchangeRepo = repo }
}

// WithExchangeRedactor wires the redaction seam every recorded body passes
// through before storage (design §4).
func WithExchangeRedactor(fn ExchangeRedactor) ServerOption {
	return func(s *Server) { s.exchangeRedactor = fn }
}

// Request headers the container sends beside the execution id (LLD 09 §4,
// "request headers"): the step the loop is in and its iteration counter.
const (
	headerStepID    = "X-Vornik-Step-ID"
	headerIteration = "X-Vornik-Iteration"
)

// projectRecordsExchanges reads the opt-in from the registry snapshot, per
// request, so a hot reload takes effect at the step's next model call.
func (s *Server) projectRecordsExchanges(projectID string) bool {
	if s.projectRegistry == nil || projectID == "" {
		return false
	}
	p := s.projectRegistry.GetProject(projectID)
	return p != nil && p.Recording.LLMExchanges
}

// recordLLMExchange stores one exchange of an opted-in execution. Best-effort
// like every other recorder in the proxy's telemetry path: a failure is a
// counter and a log line, never a failed completion. A provider error is
// recorded with the error envelope as its response, because a replay of a
// step that hit a 429 must reproduce the 429.
func (s *Server) recordLLMExchange(ctx context.Context, r *http.Request, executionID string, req chat.ChatRequest, resp *chat.ChatResponse, provErr error, dur time.Duration) {
	if s.llmExchangeRepo == nil {
		return
	}
	stepID := r.Header.Get(headerStepID)
	if stepID == "" {
		// An image that predates the header: nothing to key the row on.
		s.apiMetrics.RecordLLMExchangeRecordFailed("no_step")
		return
	}
	canonical, hash, err := llmreplay.Canonical(req)
	if err != nil {
		s.logger.Warn().Err(err).Str("execution_id", executionID).Msg("llm exchange: canonicalise request failed")
		s.apiMetrics.RecordLLMExchangeRecordFailed("canonical")
		return
	}
	var respJSON []byte
	if provErr != nil {
		respJSON, _ = json.Marshal(map[string]any{"error": map[string]any{"message": provErr.Error()}})
	} else {
		respJSON, err = json.Marshal(resp)
		if err != nil {
			s.apiMetrics.RecordLLMExchangeRecordFailed("marshal")
			return
		}
	}
	reqBody, respBody := string(canonical), string(respJSON)
	redactions := 0
	if s.exchangeRedactor != nil {
		var n int
		reqBody, n = s.exchangeRedactor(reqBody)
		redactions += n
		respBody, n = s.exchangeRedactor(respBody)
		redactions += n
	}
	if redactions > 0 {
		// The hash names the STORED bytes (design §4).
		hash = llmreplay.Hash([]byte(reqBody))
	}
	x := &persistence.LLMExchange{
		ExecutionID:  executionID,
		StepID:       stepID,
		RequestHash:  hash,
		RequestJSON:  reqBody,
		ResponseJSON: respBody,
		Model:        req.Model,
		DurationMs:   int(dur / time.Millisecond),
		Redactions:   redactions,
	}
	if v := r.Header.Get(headerIteration); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			x.Iteration = &n
		}
	}
	if resp != nil {
		if resp.Model != "" {
			x.Model = resp.Model
		}
		x.PromptTokens = resp.Usage.PromptTokens
		x.CompletionTokens = resp.Usage.CompletionTokens
	}
	if err := s.llmExchangeRepo.Record(ctx, x); err != nil {
		s.logger.Warn().Err(err).Str("execution_id", executionID).Str("step", stepID).Msg("llm exchange: record failed")
		s.apiMetrics.RecordLLMExchangeRecordFailed("store")
	}
}
