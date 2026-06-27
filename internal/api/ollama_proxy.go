package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/chat"
)

// Ollama-compatibility layer (2026-05-16). Translates Ollama's
// request/response shapes to/from the internal chat.Provider
// surface so Ollama-native clients (Open WebUI, LobeChat, the
// Ollama CLI, generic "OpenAI-compatible OR Ollama" tools) can
// reach vornik without modification.
//
// Why a separate layer instead of "set base_url to /v1"? Many
// Ollama clients hardcode /api/tags + /api/chat paths and the
// {model, message, done} response envelope; they don't speak
// OpenAI's /v1/chat/completions + {choices:[...]} shape. The
// translation cost is small (each request decodes / re-encodes
// once), so a parallel surface is cheaper than asking operators
// to rewrite their client configs.
//
// Endpoint coverage in this slice:
//   - GET  /            → "Ollama is running" banner (some
//                          clients ping this before any call)
//   - GET  /api/version → fake version (we report a canonical
//                          modern-Ollama version so clients
//                          don't gate on a too-old version
//                          number)
//   - GET  /api/tags    → translated model list from /v1/models
//   - POST /api/chat    → chat completion (stream + non-stream)
//   - POST /api/generate → single-prompt completion (mostly a
//                          shim over /api/chat for legacy clients)
//
// Embeddings (/api/embed, /api/embeddings) are out of scope —
// the daemon's chat surface doesn't ship an embedder.

// ollamaTagsResponse is the GET /api/tags wire shape. Clients
// (especially Open WebUI) check `models[].name` for the "select
// model" dropdown.
type ollamaTagsResponse struct {
	Models []ollamaTagEntry `json:"models"`
}

type ollamaTagEntry struct {
	Name       string             `json:"name"`
	Model      string             `json:"model"`
	ModifiedAt string             `json:"modified_at"`
	Size       int64              `json:"size"`
	Digest     string             `json:"digest"`
	Details    ollamaModelDetails `json:"details"`
}

// ollamaModelDetails is the synthetic "details" block. Real
// Ollama populates these from the GGUF metadata; we don't have
// that, so we fill in plausible defaults and let the family
// label echo the provider (e.g. "anthropic", "bedrock"). Open
// WebUI uses families to group entries in the dropdown — keeping
// per-provider grouping is operator-friendly.
type ollamaModelDetails struct {
	ParentModel       string   `json:"parent_model"`
	Format            string   `json:"format"`
	Family            string   `json:"family"`
	Families          []string `json:"families"`
	ParameterSize     string   `json:"parameter_size"`
	QuantizationLevel string   `json:"quantization_level"`
}

// ollamaChatRequest is the POST /api/chat wire shape. Field set
// is a strict subset of Ollama's docs — we honour what the
// translation actually needs.
type ollamaChatRequest struct {
	Model    string                 `json:"model"`
	Messages []ollamaChatMessage    `json:"messages"`
	Stream   *bool                  `json:"stream,omitempty"` // pointer: nil ≠ false (Ollama default is true)
	Options  map[string]interface{} `json:"options,omitempty"`
	Tools    []chat.Tool            `json:"tools,omitempty"`
	Format   interface{}            `json:"format,omitempty"` // "json" or json-schema object
}

type ollamaChatMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
	Images    []string         `json:"images,omitempty"`
	// ToolName carries the function name when the client sends a
	// tool RESULT back (role:"tool"). OpenAI's wire uses
	// tool_call_id; Ollama uses name. Honour both on decode.
	ToolName string `json:"name,omitempty"`
}

// ollamaToolCall is the wire shape Ollama uses for assistant
// tool calls and for client-sent tool results. Differs from
// chat.ToolCall in two operationally significant ways:
//
//  1. No top-level id / type fields. Ollama doesn't carry a tool
//     call ID; clients correlate by position. (Real Ollama
//     servers sometimes emit an empty id for compat — we accept
//     either on input and emit none on output to match the
//     reference behaviour.)
//
//  2. Arguments is a JSON object on the wire, not a JSON-encoded
//     string. OpenAI inherits the string form from its function-
//     calling spec; Ollama parses ahead-of-time so clients don't
//     have to. Both encode and decode paths translate.
type ollamaToolCall struct {
	Function ollamaToolFunction `json:"function"`
}

type ollamaToolFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"` // JSON OBJECT on the wire
}

// ollamaChatResponse is the non-streaming /api/chat reply. The
// streaming variant emits this shape line-by-line with
// done:false until the final chunk which carries done:true plus
// the eval-count statistics.
type ollamaChatResponse struct {
	Model              string            `json:"model"`
	CreatedAt          string            `json:"created_at"`
	Message            ollamaChatMessage `json:"message"`
	Done               bool              `json:"done"`
	DoneReason         string            `json:"done_reason,omitempty"`
	TotalDuration      int64             `json:"total_duration,omitempty"`
	LoadDuration       int64             `json:"load_duration,omitempty"`
	PromptEvalCount    int               `json:"prompt_eval_count,omitempty"`
	PromptEvalDuration int64             `json:"prompt_eval_duration,omitempty"`
	EvalCount          int               `json:"eval_count,omitempty"`
	EvalDuration       int64             `json:"eval_duration,omitempty"`
}

// ollamaGenerateRequest is the POST /api/generate wire shape.
// Legacy single-prompt endpoint — older clients still use it
// instead of /api/chat.
type ollamaGenerateRequest struct {
	Model   string                 `json:"model"`
	Prompt  string                 `json:"prompt"`
	System  string                 `json:"system,omitempty"`
	Stream  *bool                  `json:"stream,omitempty"`
	Options map[string]interface{} `json:"options,omitempty"`
	Format  interface{}            `json:"format,omitempty"`
}

type ollamaGenerateResponse struct {
	Model           string `json:"model"`
	CreatedAt       string `json:"created_at"`
	Response        string `json:"response"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason,omitempty"`
	TotalDuration   int64  `json:"total_duration,omitempty"`
	PromptEvalCount int    `json:"prompt_eval_count,omitempty"`
	EvalCount       int    `json:"eval_count,omitempty"`
}

// ollamaVersionResponse is the GET /api/version reply.
type ollamaVersionResponse struct {
	Version string `json:"version"`
}

// ollamaShowRequest is the POST /api/show wire shape. Clients
// supply the model name as `name` (newer Ollama) or `model`
// (older); accept both on decode.
type ollamaShowRequest struct {
	Model string `json:"model"`
	Name  string `json:"name"`
}

// ollamaShowResponse is the canonical /api/show envelope. Open
// WebUI and Home Assistant's Ollama integration both gate on
// the `capabilities` array — without "tools" in the list, HA's
// config flow refuses to enable the model for tool-calling
// conversation agents. We populate it from what the proxy
// surface actually supports (always: completion + tools; vision
// only when the model id name-matches the multimodal patterns
// real Ollama detects).
//
// Modelfile / parameters / template / system are operator-
// visible fields in `ollama show`'s CLI output; clients tolerate
// empty strings. modelinfo is a free-form metadata blob; an
// empty object is acceptable.
type ollamaShowResponse struct {
	Modelfile    string             `json:"modelfile"`
	Parameters   string             `json:"parameters"`
	Template     string             `json:"template"`
	System       string             `json:"system"`
	Details      ollamaModelDetails `json:"details"`
	ModelInfo    map[string]any     `json:"model_info"`
	Capabilities []string           `json:"capabilities"`
}

// OllamaRoot serves GET / with the "Ollama is running" banner.
// Some clients hit this before any other endpoint to confirm
// the server is reachable.
func (s *Server) OllamaRoot(w http.ResponseWriter, r *http.Request) {
	// Only respond on the exact root path — let / 404 routes
	// (typos, accidental requests) keep their default behaviour.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "Ollama is running")
}

// OllamaVersion serves GET /api/version. We report a recent
// Ollama version string so clients don't refuse to talk to a
// "too old" server. Bumping this when client compat shifts is
// cheaper than bumping a real Ollama binary.
func (s *Server) OllamaVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ollamaVersionResponse{Version: "0.5.0-vornik"})
}

// OllamaShow serves POST /api/show. Returns synthetic model
// details for any known model id, including a capabilities
// array carrying "completion" + "tools" so clients (Home
// Assistant's Ollama integration in particular) accept the
// model for tool-calling. Vision support is advertised when
// the model id matches the multimodal patterns real Ollama
// uses — gpt-4-vision, claude-*-vision, gemini-*-vision,
// llava, *-vl, etc.
//
// Real Ollama populates this from GGUF metadata; we don't have
// that, so the modelfile / parameters / template / system
// fields are empty strings (clients tolerate this — they're
// operator-visible content, not gates).
//
// 2026-05-16 — added after Home Assistant's Ollama config flow
// reported "404 page not found" trying to enable a model.
// `client.show(model)` is called after `client.list()` to
// validate capabilities; without this endpoint the model picker
// refused to save.
func (s *Server) OllamaShow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := readLimitedBody(w, r, 64*1024)
	if err != nil {
		respondError(w, http.StatusBadRequest, "READ_FAILED", err.Error())
		return
	}
	var req ollamaShowRequest
	// Tolerate empty body — some probe paths don't send one.
	_ = json.Unmarshal(body, &req)
	model := req.Model
	if model == "" {
		model = req.Name
	}
	// Synthesize details. We don't actually need to validate the
	// model exists — HA's config flow accepts any well-shaped
	// response and gates on the capabilities array; rejecting an
	// unknown model would block legitimate use cases (operator
	// trying a model they're about to wire).
	family := familyForModelID(model)
	resp := ollamaShowResponse{
		Details: ollamaModelDetails{
			Format:            "api",
			Family:            family,
			Families:          []string{family},
			ParameterSize:     "n/a",
			QuantizationLevel: "n/a",
		},
		ModelInfo:    map[string]any{},
		Capabilities: capabilitiesForModelID(model),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// familyForModelID returns the synthetic family name for an
// Ollama tag response based on the model id's prefix. Matches
// the prefix routing chat.Router does so the dropdown groups
// the same way the proxy actually dispatches.
func familyForModelID(model string) string {
	if model == "" {
		return "vornik"
	}
	if idx := strings.Index(model, "/"); idx > 0 {
		return model[:idx]
	}
	if idx := strings.Index(model, "."); idx > 0 {
		return model[:idx]
	}
	return "vornik"
}

// capabilitiesForModelID returns the capability list a client
// gates on. "completion" + "tools" are always advertised — every
// supported provider speaks both. Vision is advertised only
// when the model id matches a known multimodal naming pattern
// so an operator trying to use vision on a text-only model
// doesn't get cryptic provider 400s.
func capabilitiesForModelID(model string) []string {
	caps := []string{"completion", "tools"}
	if isVisionModel(model) {
		caps = append(caps, "vision")
	}
	return caps
}

func isVisionModel(model string) bool {
	m := strings.ToLower(model)
	// Open-coded list mirrors what real Ollama's GGUF metadata
	// would surface: anything with -vision, -vl, llava, gemini
	// (all gemini models are multimodal as of 2.x), or claude-*
	// (Anthropic vision-by-default since Sonnet 3.5).
	patterns := []string{"vision", "-vl", "llava", "gemini", "claude", "gpt-4o", "gpt-5"}
	for _, p := range patterns {
		if strings.Contains(m, p) {
			return true
		}
	}
	return false
}

// OllamaTags serves GET /api/tags. Walks the same provider model
// list /v1/models walks, then translates each ModelInfo into
// Ollama's tag shape. The model name is preserved verbatim
// (e.g. "anthropic.claude-opus-4-7") so the operator picks the
// same identifier in both Ollama and OpenAI clients.
func (s *Server) OllamaTags(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.chatProvider == nil {
		respondError(w, http.StatusServiceUnavailable, "CHAT_NOT_CONFIGURED",
			"chat provider not configured")
		return
	}

	models := s.collectModelsForOllama(r.Context())
	now := time.Now().UTC().Format(time.RFC3339Nano)
	resp := ollamaTagsResponse{Models: make([]ollamaTagEntry, 0, len(models))}
	for _, m := range models {
		family := m.provider
		if family == "" {
			family = "vornik"
		}
		resp.Models = append(resp.Models, ollamaTagEntry{
			Name:       m.id,
			Model:      m.id,
			ModifiedAt: now,
			// Real Ollama reports GGUF blob size; we don't have
			// one, so use 0. Open WebUI tolerates this.
			Size:   0,
			Digest: "",
			Details: ollamaModelDetails{
				Format:            "api",
				Family:            family,
				Families:          []string{family},
				ParameterSize:     "n/a",
				QuantizationLevel: "n/a",
			},
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// ollamaModelRow is the (id, provider) pair we need from the
// provider's model list; kept separate from openAIModelEntry so
// the two translations stay independent.
type ollamaModelRow struct {
	id, provider string
}

// collectModelsForOllama is the narrow ModelLister fan-out used
// by /api/tags. Mirrors the discovery logic in ListModels but
// only takes the (id, provider) pairs the tags shape needs.
func (s *Server) collectModelsForOllama(ctx context.Context) []ollamaModelRow {
	var out []ollamaModelRow

	push := func(provider string, ms []chat.ModelInfo) {
		for _, m := range ms {
			p := provider
			if m.Provider != "" {
				p = m.Provider
			}
			out = append(out, ollamaModelRow{id: m.ID, provider: p})
		}
	}

	if r1, ok := s.chatProvider.(*chat.Router); ok {
		res := r1.ListModels(ctx)
		for prov, ms := range res.Providers {
			push(prov, ms)
		}
		return out
	}
	if q, ok := s.chatProvider.(chat.ModelAggregator); ok {
		if agg, ok := q.ListModelsAggregated(ctx); ok {
			for prov, ms := range agg.Providers {
				push(prov, ms)
			}
			return out
		}
		if ms, err := q.ListModels(ctx); err == nil {
			push("chat", ms)
		}
		return out
	}
	if lister, ok := s.chatProvider.(chat.ModelLister); ok {
		if ms, err := lister.ListModels(ctx); err == nil {
			push("chat", ms)
		}
	}
	return out
}

// OllamaChat serves POST /api/chat. Decodes the Ollama request,
// translates to chat.ChatRequest, dispatches via chat.Provider,
// and writes back either a single JSON document (stream:false)
// or a stream of NDJSON-framed chunks (stream:true or unset —
// Ollama's default is stream:true).
func (s *Server) OllamaChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.chatProvider == nil {
		respondError(w, http.StatusServiceUnavailable, "CHAT_NOT_CONFIGURED",
			"chat provider not configured")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxChatProxyBodyBytes+1))
	if err != nil {
		respondError(w, http.StatusBadRequest, "READ_FAILED", err.Error())
		return
	}
	if int64(len(body)) > maxChatProxyBodyBytes {
		respondError(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE",
			fmt.Sprintf("request body exceeds %d bytes", maxChatProxyBodyBytes))
		return
	}

	var req ollamaChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		respondError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	if len(req.Messages) == 0 {
		respondError(w, http.StatusBadRequest, "EMPTY_MESSAGES",
			"messages must be a non-empty array")
		return
	}

	// Ollama's default is stream:true. Honour the explicit
	// stream:false to match curl/script users who want a single
	// JSON blob.
	stream := true
	if req.Stream != nil {
		stream = *req.Stream
	}

	// Translate messages → chat.Message. The conversion is
	// near-trivial (same role/content shape) plus tool_calls
	// pass-through; images are dropped because the OpenAI-shaped
	// chat surface routes them via the multipart upload path the
	// agent harness owns, not via inline base64 strings.
	provider := s.chatProvider
	if req.Model != "" {
		if o, ok := provider.(chat.ModelOverridable); ok {
			provider = o.WithModel(req.Model)
		}
	}
	internal := translateOllamaMessagesToInternal(req.Messages)

	model := req.Model
	if model == "" {
		model = provider.Model()
	}
	startedAt := time.Now()

	if !stream {
		resp, err := provider.CompleteWithTools(r.Context(), internal, req.Tools)
		if err != nil {
			s.logger.Warn().Err(err).Str("model", req.Model).
				Msg("ollama chat: provider returned error")
			respondError(w, http.StatusBadGateway, "PROVIDER_ERROR", err.Error())
			return
		}
		if resp == nil {
			respondError(w, http.StatusBadGateway, "PROVIDER_ERROR", "provider returned nil response")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(translateChatResponseToOllama(resp, model, startedAt))
		// Telemetry fire-and-forget AFTER the response is flushed — these
		// are blocking Postgres writes that must not stall the client (see
		// chat_proxy.go ChatCompletions). Detached context: r.Context()
		// cancels on handler return. De-dup guard inside the recorders
		// skips agent-originated calls (task/execution header set).
		telemetryCtx := context.WithoutCancel(r.Context())
		cost := s.computeChatCallCost(req.Model, resp)
		go func() {
			defer func() {
				if rec := recover(); rec != nil {
					s.logger.Error().Interface("panic", rec).Msg("ollama chat: telemetry goroutine panicked")
				}
			}()
			s.recordChatAPIUsage(telemetryCtx, r, req.Model, resp)
			s.recordChatAPIAudit(telemetryCtx, r, startedAt, req.Model, internal, resp, cost)
		}()
		return
	}

	// Streaming path. Ollama uses NDJSON (one JSON document per
	// line) over a regular Content-Type: application/x-ndjson
	// response. Each line is `{... done:false}` for content
	// deltas; the final line is `{... done:true}` with stats.
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")

	prev := ""
	onText := func(accumulated string) {
		// Provider callback fires with the full accumulated text
		// each time. Ollama wants per-chunk deltas, so we diff
		// against the last emit.
		if !strings.HasPrefix(accumulated, prev) {
			// Streaming text rewrote past content — emit a full
			// reset by sending the suffix from the longest common
			// prefix.
			prev = ""
		}
		delta := accumulated[len(prev):]
		if delta == "" {
			return
		}
		prev = accumulated
		chunk := ollamaChatResponse{
			Model:     model,
			CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
			Message: ollamaChatMessage{
				Role:    "assistant",
				Content: delta,
			},
			Done: false,
		}
		buf, _ := json.Marshal(chunk)
		_, _ = w.Write(buf)
		_, _ = w.Write([]byte("\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}

	resp, err := provider.CompleteWithToolsStream(r.Context(), internal, req.Tools, onText)
	if err != nil {
		// Mid-stream error: emit a final NDJSON line with
		// done:true + done_reason carrying the error text so
		// Ollama clients show the failure inline instead of
		// hanging on a half-open response.
		final := ollamaChatResponse{
			Model:      model,
			CreatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
			Message:    ollamaChatMessage{Role: "assistant"},
			Done:       true,
			DoneReason: "error: " + truncateOllamaErr(err.Error()),
		}
		buf, _ := json.Marshal(final)
		_, _ = w.Write(buf)
		_, _ = w.Write([]byte("\n"))
		if flusher != nil {
			flusher.Flush()
		}
		return
	}

	// Final chunk with stats + tool calls (if any) + done:true.
	final := translateChatResponseToOllama(resp, model, startedAt)
	// In the stream path the per-chunk loop already sent the
	// content deltas, so the closing message body is empty — but
	// the tool_calls + stats need to ride the final frame in
	// Ollama's native shape (no id/type, arguments-as-object).
	final.Message.Content = ""
	if resp != nil && len(resp.Choices) > 0 {
		final.Message.ToolCalls = internalToolCallsToOllama(resp.Choices[0].Message.ToolCalls)
	}
	final.Done = true
	if final.DoneReason == "" {
		final.DoneReason = "stop"
	}
	buf, _ := json.Marshal(final)
	_, _ = w.Write(buf)
	_, _ = w.Write([]byte("\n"))
	if flusher != nil {
		flusher.Flush()
	}

	// Telemetry fire-and-forget AFTER the closing frame is flushed, so
	// the blocking Postgres writes don't delay the stream's done:true
	// frame (see chat_proxy.go). Detached context: r.Context() cancels
	// on handler return. De-dup guard inside the recorders skips
	// agent-originated calls.
	telemetryCtx := context.WithoutCancel(r.Context())
	cost := s.computeChatCallCost(req.Model, resp)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error().Interface("panic", rec).Msg("ollama chat: telemetry goroutine panicked")
			}
		}()
		s.recordChatAPIUsage(telemetryCtx, r, req.Model, resp)
		s.recordChatAPIAudit(telemetryCtx, r, startedAt, req.Model, internal, resp, cost)
	}()
}

// OllamaGenerate serves POST /api/generate — the legacy
// single-prompt endpoint. Shim translates the prompt + system
// fields into a two-message chat conversation and dispatches
// through the same path.
func (s *Server) OllamaGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.chatProvider == nil {
		respondError(w, http.StatusServiceUnavailable, "CHAT_NOT_CONFIGURED",
			"chat provider not configured")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxChatProxyBodyBytes+1))
	if err != nil {
		respondError(w, http.StatusBadRequest, "READ_FAILED", err.Error())
		return
	}
	if int64(len(body)) > maxChatProxyBodyBytes {
		respondError(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE",
			fmt.Sprintf("request body exceeds %d bytes", maxChatProxyBodyBytes))
		return
	}

	var req ollamaGenerateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		respondError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	if req.Prompt == "" {
		respondError(w, http.StatusBadRequest, "EMPTY_PROMPT",
			"prompt must not be empty")
		return
	}

	stream := true
	if req.Stream != nil {
		stream = *req.Stream
	}

	provider := s.chatProvider
	if req.Model != "" {
		if o, ok := provider.(chat.ModelOverridable); ok {
			provider = o.WithModel(req.Model)
		}
	}

	internal := make([]chat.Message, 0, 2)
	if req.System != "" {
		internal = append(internal, chat.Message{Role: "system", Content: req.System})
	}
	internal = append(internal, chat.Message{Role: "user", Content: req.Prompt})

	model := req.Model
	if model == "" {
		model = provider.Model()
	}
	startedAt := time.Now()

	if !stream {
		resp, err := provider.CompleteWithTools(r.Context(), internal, nil)
		if err != nil {
			respondError(w, http.StatusBadGateway, "PROVIDER_ERROR", err.Error())
			return
		}
		// Cost metric. Agent calls (task/execution header set)
		// are skipped to avoid double-billing — same as the
		// chat-completions + /api/chat paths.
		s.recordChatAPIUsage(r.Context(), r, req.Model, resp)
		s.recordChatAPIAudit(r.Context(), r, startedAt, req.Model, internal, resp,
			s.computeChatCallCost(req.Model, resp))
		w.Header().Set("Content-Type", "application/json")
		out := ollamaGenerateResponse{
			Model:      model,
			CreatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
			Done:       true,
			DoneReason: "stop",
		}
		if resp != nil && len(resp.Choices) > 0 {
			out.Response = resp.Choices[0].Message.Content
			out.PromptEvalCount = resp.Usage.PromptTokens
			out.EvalCount = resp.Usage.CompletionTokens
		}
		out.TotalDuration = time.Since(startedAt).Nanoseconds()
		_ = json.NewEncoder(w).Encode(out)
		return
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")

	prev := ""
	onText := func(accumulated string) {
		if !strings.HasPrefix(accumulated, prev) {
			prev = ""
		}
		delta := accumulated[len(prev):]
		if delta == "" {
			return
		}
		prev = accumulated
		chunk := ollamaGenerateResponse{
			Model:     model,
			CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
			Response:  delta,
			Done:      false,
		}
		buf, _ := json.Marshal(chunk)
		_, _ = w.Write(buf)
		_, _ = w.Write([]byte("\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}

	resp, err := provider.CompleteWithToolsStream(r.Context(), internal, nil, onText)
	if err != nil {
		final := ollamaGenerateResponse{
			Model:      model,
			CreatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
			Done:       true,
			DoneReason: "error: " + truncateOllamaErr(err.Error()),
		}
		buf, _ := json.Marshal(final)
		_, _ = w.Write(buf)
		_, _ = w.Write([]byte("\n"))
		if flusher != nil {
			flusher.Flush()
		}
		return
	}

	// Cost metric for the streaming generate path. Same de-dup
	// guard as the non-streaming branch.
	s.recordChatAPIUsage(r.Context(), r, req.Model, resp)
	s.recordChatAPIAudit(r.Context(), r, startedAt, req.Model, internal, resp,
		s.computeChatCallCost(req.Model, resp))

	final := ollamaGenerateResponse{
		Model:         model,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339Nano),
		Done:          true,
		DoneReason:    "stop",
		TotalDuration: time.Since(startedAt).Nanoseconds(),
	}
	if resp != nil {
		final.PromptEvalCount = resp.Usage.PromptTokens
		final.EvalCount = resp.Usage.CompletionTokens
	}
	buf, _ := json.Marshal(final)
	_, _ = w.Write(buf)
	_, _ = w.Write([]byte("\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

// translateChatResponseToOllama maps an OpenAI-shaped
// ChatResponse to Ollama's chat-response envelope. Used by both
// the non-streaming path (full final body) and the streaming
// path (final done:true frame).
func translateChatResponseToOllama(resp *chat.ChatResponse, model string, startedAt time.Time) ollamaChatResponse {
	out := ollamaChatResponse{
		Model:         model,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339Nano),
		Done:          true,
		DoneReason:    "stop",
		TotalDuration: time.Since(startedAt).Nanoseconds(),
	}
	if resp == nil {
		return out
	}
	if resp.Model != "" {
		out.Model = resp.Model
	}
	out.PromptEvalCount = resp.Usage.PromptTokens
	out.EvalCount = resp.Usage.CompletionTokens
	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		out.Message = ollamaChatMessage{
			Role:      "assistant",
			Content:   choice.Message.Content,
			ToolCalls: internalToolCallsToOllama(choice.Message.ToolCalls),
		}
		if choice.FinishReason != "" {
			out.DoneReason = choice.FinishReason
		}
	}
	return out
}

// truncateOllamaErr keeps the inline error short enough that
// clients rendering it inline don't blow up their UI. 240 chars
// matches the Telegram bot's humanize cap.
func truncateOllamaErr(s string) string {
	const max = 240
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// translateOllamaMessagesToInternal walks the wire-shape message
// list and produces the internal chat.Message list the provider
// expects. The interesting part is tool-result attribution:
// Ollama doesn't carry a tool_call_id on the wire (the protocol
// matches by position/name), but Vertex/Gemini/Bedrock providers
// require one — without it Gemini returns "Expected input to
// contain field: 'tool_call_id'" (operator-observed 2026-05-16).
//
// We backfill the ID by walking the conversation: every assistant
// message's tool_calls get fabricated IDs (`ollama-call-<idx>-<name>`)
// as before, and we remember those IDs keyed by tool name. When
// we see a subsequent tool-result message, we look up the ID by
// name and stamp it on the message. Ambiguity (two parallel calls
// to the same tool) is broken by position — second tool result
// with the same name uses the second ID.
func translateOllamaMessagesToInternal(in []ollamaChatMessage) []chat.Message {
	out := make([]chat.Message, 0, len(in))
	// pending[name] = queue of tool_call_ids for that name awaiting
	// a tool-result message. Popping from the front matches "first
	// in flight, first to return" semantics.
	pending := map[string][]string{}
	for _, m := range in {
		msg := chat.Message{
			Role:      m.Role,
			Content:   m.Content,
			ToolCalls: ollamaToolCallsToInternal(m.ToolCalls),
		}
		// Record fabricated IDs from assistant tool_calls so the
		// next tool-result message can resolve them by name.
		if m.Role == "assistant" && len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				pending[tc.Function.Name] = append(pending[tc.Function.Name], tc.ID)
			}
		}
		// Tool-result message: stamp ToolCallID + Name so the
		// downstream provider can correlate the result with the
		// originating call.
		if m.Role == "tool" {
			name := m.ToolName
			if name == "" {
				// Some Ollama clients put the name in Content as
				// JSON or in a non-standard field; fall back to
				// "tool" so the request still flows rather than
				// 500-ing on missing attribution.
				name = "tool"
			}
			msg.Name = name
			if ids := pending[name]; len(ids) > 0 {
				msg.ToolCallID = ids[0]
				pending[name] = ids[1:]
			} else {
				// No prior assistant call we can match — synthesize
				// a deterministic ID so the wire shape stays valid
				// even when the client sent a tool result without
				// a preceding tool call (unusual but possible on
				// replay flows).
				msg.ToolCallID = "ollama-orphan-" + name
			}
		}
		out = append(out, msg)
	}
	return out
}

// ollamaToolCallsToInternal converts the Ollama wire shape (no
// id/type, arguments-as-object) to the internal chat.ToolCall
// shape (id+type populated, arguments as a JSON-encoded string).
// The id is fabricated because internal callers downstream
// (Bedrock converter, gates) expect a non-empty correlation
// token; we use a deterministic-ish suffix from the function
// name + index so the same conversation re-replays predictably.
func ollamaToolCallsToInternal(in []ollamaToolCall) []chat.ToolCall {
	if len(in) == 0 {
		return nil
	}
	out := make([]chat.ToolCall, 0, len(in))
	for i, tc := range in {
		// Re-encode the object arguments as a string so the
		// internal FunctionCall.Arguments contract (always
		// JSON-string) holds.
		args := "{}"
		if len(tc.Function.Arguments) > 0 {
			args = string(tc.Function.Arguments)
		}
		out = append(out, chat.ToolCall{
			ID:   fmt.Sprintf("ollama-call-%d-%s", i, tc.Function.Name),
			Type: "function",
			Function: chat.FunctionCall{
				Name:      tc.Function.Name,
				Arguments: args,
			},
		})
	}
	return out
}

// internalToolCallsToOllama is the inverse: convert
// chat.ToolCall (id+type+string-args) to Ollama's wire shape
// (no id/type, args-as-object). Open WebUI / LobeChat reject
// responses where arguments is a string ("Object expected").
// On unparseable arguments we emit a JSON null so the shape
// still validates — the alternative (silently dropping the
// call) is worse for the operator's debugging.
func internalToolCallsToOllama(in []chat.ToolCall) []ollamaToolCall {
	if len(in) == 0 {
		return nil
	}
	out := make([]ollamaToolCall, 0, len(in))
	for _, tc := range in {
		var args json.RawMessage
		if tc.Function.Arguments == "" {
			args = json.RawMessage("{}")
		} else if json.Valid([]byte(tc.Function.Arguments)) {
			args = json.RawMessage(tc.Function.Arguments)
		} else {
			// Provider returned a malformed arguments string —
			// emit null rather than a parse-trap object. Clients
			// surface this as "tool got no arguments" rather than
			// hanging on a parse error.
			args = json.RawMessage("null")
		}
		out = append(out, ollamaToolCall{
			Function: ollamaToolFunction{
				Name:      tc.Function.Name,
				Arguments: args,
			},
		})
	}
	return out
}
