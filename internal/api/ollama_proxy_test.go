// Coverage for the Ollama-compatibility surface. Exercises every
// public handler in ollama_proxy.go against a stubbed
// chat.Provider, including the NDJSON streaming framing.

package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
)

// streamingStub satisfies chat.Provider with scriptable streaming.
// CompleteWithToolsStream invokes the callback with each canned
// chunk in order, accumulating to mimic the real
// "callback receives accumulated text" contract.
type streamingStub struct {
	model     string
	chunks    []string
	err       error
	finalResp *chat.ChatResponse
}

func (s *streamingStub) Complete(_ context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
	return s.finalResp, s.err
}
func (s *streamingStub) CompleteWithTools(_ context.Context, _ []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	return s.finalResp, s.err
}
func (s *streamingStub) CompleteWithToolsStream(_ context.Context, _ []chat.Message, _ []chat.Tool, onText chat.StreamCallback) (*chat.ChatResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	accumulated := ""
	for _, c := range s.chunks {
		accumulated += c
		if onText != nil {
			onText(accumulated)
		}
	}
	return s.finalResp, nil
}
func (s *streamingStub) Model() string              { return s.model }
func (s *streamingStub) SetMetrics(_ *chat.Metrics) {}

// TestOllamaRoot_BannerOnSlash pins the GET / response that
// Ollama-native clients use as a server-alive probe.
func TestOllamaRoot_BannerOnSlash(t *testing.T) {
	s := NewServer(WithChatProvider(openaiStub{model: "x"}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	s.OllamaRoot(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if rr.Body.String() != "Ollama is running" {
		t.Errorf("body = %q, want %q", rr.Body.String(), "Ollama is running")
	}
}

// TestOllamaRoot_404ForOtherPaths confirms the handler acts as
// the api mux's 404 sink for paths that don't have a specific
// route. Without this any unhandled URL would return the banner
// (which would mask bugs).
func TestOllamaRoot_404ForOtherPaths(t *testing.T) {
	s := NewServer(WithChatProvider(openaiStub{model: "x"}))
	req := httptest.NewRequest(http.MethodGet, "/somewhere/else", nil)
	rr := httptest.NewRecorder()
	s.OllamaRoot(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

// TestOllamaShow_AdvertisesToolsCapability is the regression
// test for the 2026-05-16 Home Assistant config-flow failure.
// HA calls POST /api/show after listing models to verify the
// model supports tool calling; the response must carry
// "completion" + "tools" in the capabilities array or the
// config flow refuses to save.
func TestOllamaShow_AdvertisesToolsCapability(t *testing.T) {
	s := NewServer(WithChatProvider(openaiStub{model: "x"}))
	body := bytes.NewBufferString(`{"model":"google/gemini-2.5-flash"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/show", body)
	rr := httptest.NewRecorder()
	s.OllamaShow(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var resp ollamaShowResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	hasTools, hasCompletion := false, false
	for _, c := range resp.Capabilities {
		if c == "tools" {
			hasTools = true
		}
		if c == "completion" {
			hasCompletion = true
		}
	}
	if !hasTools {
		t.Errorf("capabilities = %v, want 'tools' (HA config flow gates on this)", resp.Capabilities)
	}
	if !hasCompletion {
		t.Errorf("capabilities = %v, want 'completion'", resp.Capabilities)
	}
}

// TestOllamaShow_AdvertisesVisionForMultimodalModels — when the
// model name matches a known multimodal pattern (gemini,
// claude, gpt-4o, llava, *-vl, etc.) the response should also
// list "vision" so vision-aware clients enable image support.
func TestOllamaShow_AdvertisesVisionForMultimodalModels(t *testing.T) {
	s := NewServer(WithChatProvider(openaiStub{model: "x"}))
	cases := []struct {
		model      string
		wantVision bool
	}{
		{"google/gemini-2.5-flash", true},
		{"anthropic/claude-opus-4-7", true},
		{"openai/gpt-4o", true},
		{"openai/gpt-5", true},
		{"text-only-model", false},
		{"openai/gpt-3.5-turbo", false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			body := bytes.NewBufferString(`{"model":"` + tc.model + `"}`)
			req := httptest.NewRequest(http.MethodPost, "/api/show", body)
			rr := httptest.NewRecorder()
			s.OllamaShow(rr, req)
			var resp ollamaShowResponse
			_ = json.Unmarshal(rr.Body.Bytes(), &resp)
			gotVision := false
			for _, c := range resp.Capabilities {
				if c == "vision" {
					gotVision = true
				}
			}
			if gotVision != tc.wantVision {
				t.Errorf("model %q: vision in caps = %v, want %v (caps=%v)",
					tc.model, gotVision, tc.wantVision, resp.Capabilities)
			}
		})
	}
}

// TestOllamaShow_AcceptsLegacyNameField — older Ollama clients
// send {"name": "..."} instead of {"model": "..."}. Both must
// resolve.
func TestOllamaShow_AcceptsLegacyNameField(t *testing.T) {
	s := NewServer(WithChatProvider(openaiStub{model: "x"}))
	body := bytes.NewBufferString(`{"name":"my-model"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/show", body)
	rr := httptest.NewRecorder()
	s.OllamaShow(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}

// TestOllamaShow_EmptyBodyTolerated — some probe paths send no
// body. The handler should still respond with a default
// envelope rather than 400.
func TestOllamaShow_EmptyBodyTolerated(t *testing.T) {
	s := NewServer(WithChatProvider(openaiStub{model: "x"}))
	req := httptest.NewRequest(http.MethodPost, "/api/show", nil)
	rr := httptest.NewRecorder()
	s.OllamaShow(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}

func TestOllamaShow_RejectsOversizedBody(t *testing.T) {
	s := NewServer(WithLogger(zerolog.Nop()))
	req := httptest.NewRequest(http.MethodPost, "/api/show", strings.NewReader(strings.Repeat("x", 64*1024+1)))
	rr := httptest.NewRecorder()
	s.OllamaShow(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", rr.Code, rr.Body.String())
	}
}

// TestOllamaShow_RejectsNonPost
func TestOllamaShow_RejectsNonPost(t *testing.T) {
	s := NewServer(WithChatProvider(openaiStub{model: "x"}))
	req := httptest.NewRequest(http.MethodGet, "/api/show", nil)
	rr := httptest.NewRecorder()
	s.OllamaShow(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

// TestFamilyForModelID + TestIsVisionModel pin the pure helpers.
func TestFamilyForModelID(t *testing.T) {
	cases := map[string]string{
		"":                          "vornik",
		"raw-model":                 "vornik",
		"google/gemini-2.5-flash":   "google",
		"anthropic.claude-opus-4-7": "anthropic",
	}
	for in, want := range cases {
		if got := familyForModelID(in); got != want {
			t.Errorf("familyForModelID(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestOllamaVersion_ReturnsVersionEnvelope pins the JSON shape
// clients gate on. Newer Ollama clients refuse to talk to a
// server that reports a too-old version; pin a recent string.
func TestOllamaVersion_ReturnsVersionEnvelope(t *testing.T) {
	s := NewServer(WithChatProvider(openaiStub{model: "x"}))
	req := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	rr := httptest.NewRecorder()
	s.OllamaVersion(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v.Version == "" {
		t.Error("version is empty")
	}
}

// TestOllamaTags_ReturnsModelsList pins the GET /api/tags
// envelope shape Open WebUI consumes. Names round-trip from the
// internal model list verbatim so an operator sees the same id
// in both surfaces.
func TestOllamaTags_ReturnsModelsList(t *testing.T) {
	prov := modelListingStub{
		openaiStub: openaiStub{model: "gpt-4"},
		models: []chat.ModelInfo{
			{ID: "anthropic.claude-opus-4-7", Provider: "anthropic"},
			{ID: "openai.gpt-4", Provider: "openai"},
		},
	}
	s := NewServer(WithChatProvider(prov))
	cfg := &config.Config{}
	cfg.API.AuthEnabled = false
	handler := SetupRoutes(s, cfg)

	req := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var resp ollamaTagsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Models) != 2 {
		t.Fatalf("got %d models, want 2", len(resp.Models))
	}
	gotNames := map[string]bool{
		resp.Models[0].Name: true,
		resp.Models[1].Name: true,
	}
	if !gotNames["anthropic.claude-opus-4-7"] || !gotNames["openai.gpt-4"] {
		t.Errorf("names = %v, want both provider-prefixed ids", gotNames)
	}
}

// TestOllamaTags_NoChatProvider503 — without a chat provider
// wired the endpoint 503s, matching the rest of the chat
// surface.
func TestOllamaTags_NoChatProvider503(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	rr := httptest.NewRecorder()
	s.OllamaTags(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

// TestOllamaChat_NonStreamingHappyPath verifies the request →
// internal → response translation. Sends stream:false to get a
// single JSON body, validates the Ollama envelope shape, and
// confirms the inbound messages reached the stub provider with
// the right Role/Content.
func TestOllamaChat_NonStreamingHappyPath(t *testing.T) {
	stub := &streamingStub{
		model:     "vornik-stub",
		finalResp: buildOllamaOKResponse("vornik-stub", "hello back"),
	}
	s := NewServer(WithChatProvider(stub))
	body := bytes.NewBufferString(`{"model":"any-model","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", body)
	rr := httptest.NewRecorder()
	s.OllamaChat(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var resp ollamaChatResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Done {
		t.Error("done must be true on non-streaming response")
	}
	if resp.Message.Content != "hello back" {
		t.Errorf("content = %q, want %q", resp.Message.Content, "hello back")
	}
	if resp.Message.Role != "assistant" {
		t.Errorf("role = %q, want assistant", resp.Message.Role)
	}
}

// TestOllamaChat_StreamingEmitsNDJSON exercises the default-
// streaming path (stream field absent — Ollama default is true).
// Verifies each chunk arrives as its own JSON line + the final
// done:true frame closes the stream.
func TestOllamaChat_StreamingEmitsNDJSON(t *testing.T) {
	stub := &streamingStub{
		model:     "vornik-stub",
		chunks:    []string{"Hello", " there", "!"},
		finalResp: buildOllamaOKResponse("vornik-stub", "Hello there!"),
	}
	s := NewServer(WithChatProvider(stub))
	body := bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", body)
	rr := httptest.NewRecorder()
	s.OllamaChat(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("Content-Type = %q, want application/x-ndjson", ct)
	}

	// Decode line by line and verify the framing.
	scanner := bufio.NewScanner(rr.Body)
	var chunks []ollamaChatResponse
	for scanner.Scan() {
		var c ollamaChatResponse
		if err := json.Unmarshal(scanner.Bytes(), &c); err != nil {
			t.Fatalf("decode line %q: %v", scanner.Text(), err)
		}
		chunks = append(chunks, c)
	}
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want at least 2 (content + final done)", len(chunks))
	}
	if !chunks[len(chunks)-1].Done {
		t.Error("last chunk must have done:true")
	}
	// All chunks except the last should have done:false.
	for i, c := range chunks[:len(chunks)-1] {
		if c.Done {
			t.Errorf("chunk %d unexpectedly has done:true", i)
		}
	}
	// Concatenate deltas and verify they match the input.
	var assembled string
	for _, c := range chunks {
		assembled += c.Message.Content
	}
	if assembled != "Hello there!" {
		t.Errorf("assembled stream = %q, want %q", assembled, "Hello there!")
	}
}

// TestOllamaChat_StreamErrorEmitsErrorFrame — when the provider
// errors mid-stream, the handler must close the stream cleanly
// with a final done:true + done_reason carrying the error. Open
// WebUI hangs on a half-open response without this.
func TestOllamaChat_StreamErrorEmitsErrorFrame(t *testing.T) {
	stub := &streamingStub{
		model: "stub",
		err:   errors.New("upstream rate limit"),
	}
	s := NewServer(WithChatProvider(stub))
	body := bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", body)
	rr := httptest.NewRecorder()
	s.OllamaChat(rr, req)

	// Status was already 200 by the time the error hit (headers
	// flushed at stream start). The error must come through as
	// the final NDJSON frame.
	scanner := bufio.NewScanner(rr.Body)
	var last ollamaChatResponse
	for scanner.Scan() {
		_ = json.Unmarshal(scanner.Bytes(), &last)
	}
	if !last.Done {
		t.Error("error frame must have done:true")
	}
	if !strings.Contains(last.DoneReason, "upstream rate limit") {
		t.Errorf("DoneReason = %q, should mention upstream error", last.DoneReason)
	}
}

// TestOllamaChat_EmptyMessages400 pins the validation.
func TestOllamaChat_EmptyMessages400(t *testing.T) {
	s := NewServer(WithChatProvider(openaiStub{model: "x"}))
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewBufferString(`{"messages":[]}`))
	rr := httptest.NewRecorder()
	s.OllamaChat(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

// TestOllamaGenerate_NonStreamingHappyPath covers /api/generate
// (legacy single-prompt endpoint). The handler should translate
// system + prompt → a two-message conversation.
func TestOllamaGenerate_NonStreamingHappyPath(t *testing.T) {
	stub := &streamingStub{
		model:     "vornik-stub",
		finalResp: buildOllamaOKResponse("vornik-stub", "ok response"),
	}
	s := NewServer(WithChatProvider(stub))
	body := bytes.NewBufferString(`{"model":"any","stream":false,"system":"sys","prompt":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	rr := httptest.NewRecorder()
	s.OllamaGenerate(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var resp ollamaGenerateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Done || resp.Response != "ok response" {
		t.Errorf("response = %+v, want done:true response:'ok response'", resp)
	}
}

// TestOllamaGenerate_EmptyPrompt400 — legacy endpoint requires
// a non-empty prompt.
func TestOllamaGenerate_EmptyPrompt400(t *testing.T) {
	s := NewServer(WithChatProvider(openaiStub{model: "x"}))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", bytes.NewBufferString(`{"prompt":""}`))
	rr := httptest.NewRecorder()
	s.OllamaGenerate(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

// TestOllamaChat_RejectsNonPost pins the method gate.
func TestOllamaChat_RejectsNonPost(t *testing.T) {
	s := NewServer(WithChatProvider(openaiStub{model: "x"}))
	req := httptest.NewRequest(http.MethodGet, "/api/chat", nil)
	rr := httptest.NewRecorder()
	s.OllamaChat(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

// TestTranslateChatResponseToOllama exercises the pure helper.
// Verifies the field mapping (choices[0] → message, usage →
// counters) and the started-at duration calc.
func TestTranslateChatResponseToOllama(t *testing.T) {
	resp := buildOllamaOKResponse("model-x", "answer")
	resp.Usage.PromptTokens = 10
	resp.Usage.CompletionTokens = 20
	got := translateChatResponseToOllama(resp, "fallback-model", twoSecondsAgo())
	if got.Model != "model-x" {
		t.Errorf("Model: got %q, want model-x (response wins over fallback)", got.Model)
	}
	if got.Message.Content != "answer" {
		t.Errorf("Content: got %q, want answer", got.Message.Content)
	}
	if got.PromptEvalCount != 10 || got.EvalCount != 20 {
		t.Errorf("counters: got %d/%d, want 10/20", got.PromptEvalCount, got.EvalCount)
	}
	if got.TotalDuration <= 0 {
		t.Errorf("TotalDuration: got %d, want positive (since startedAt was in the past)", got.TotalDuration)
	}
}

// TestTranslateChatResponseToOllama_NilResp — defensive: a nil
// upstream response shouldn't crash, just return the fallback
// model + zero-valued envelope.
func TestTranslateChatResponseToOllama_NilResp(t *testing.T) {
	got := translateChatResponseToOllama(nil, "fallback", twoSecondsAgo())
	if got.Model != "fallback" {
		t.Errorf("Model: got %q, want fallback", got.Model)
	}
	if !got.Done {
		t.Error("Done must be true on the final translation")
	}
}

// TestTruncateOllamaErr — long error strings get clipped so they
// don't blow up Open WebUI's inline error rendering.
func TestTruncateOllamaErr(t *testing.T) {
	short := "short error"
	if got := truncateOllamaErr(short); got != short {
		t.Errorf("short pass-through: got %q", got)
	}
	long := strings.Repeat("x", 500)
	got := truncateOllamaErr(long)
	if len(got) > 245 {
		t.Errorf("long err: got %d chars, want clipped to ~241", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("long err: should end with ellipsis, got %q", got[len(got)-5:])
	}
}

// TestTranslateOllamaMessagesToInternal_BindsToolResultID is
// the regression test for the 2026-05-16 multi-turn tool-call
// failure. The Ollama wire carries no tool_call_id on tool-
// result messages, but providers (Vertex/Gemini, Bedrock)
// require one. The translator must walk the conversation and
// stamp each tool-result with the fabricated ID from the
// preceding assistant tool_calls.
func TestTranslateOllamaMessagesToInternal_BindsToolResultID(t *testing.T) {
	wire := []ollamaChatMessage{
		{Role: "user", Content: "what time is it?"},
		{Role: "assistant", ToolCalls: []ollamaToolCall{
			{Function: ollamaToolFunction{Name: "current_time", Arguments: json.RawMessage(`{}`)}},
		}},
		{Role: "tool", ToolName: "current_time", Content: "2026-05-16T12:00:00Z"},
	}
	out := translateOllamaMessagesToInternal(wire)
	if len(out) != 3 {
		t.Fatalf("got %d messages, want 3", len(out))
	}
	if len(out[1].ToolCalls) != 1 {
		t.Fatalf("assistant message lost tool_calls")
	}
	assistantCallID := out[1].ToolCalls[0].ID
	if assistantCallID == "" {
		t.Fatal("assistant tool_call must have a fabricated ID")
	}
	if out[2].ToolCallID != assistantCallID {
		t.Errorf("tool-result ToolCallID = %q, want %q (matching assistant's call)",
			out[2].ToolCallID, assistantCallID)
	}
	if out[2].Name != "current_time" {
		t.Errorf("tool-result Name = %q, want current_time", out[2].Name)
	}
}

// TestTranslateOllamaMessagesToInternal_ParallelToolCalls — two
// parallel calls to the same tool should bind in order. The
// first tool-result gets the first call's ID, the second gets
// the second's. Avoids cross-binding on multi-fanout flows.
func TestTranslateOllamaMessagesToInternal_ParallelToolCalls(t *testing.T) {
	wire := []ollamaChatMessage{
		{Role: "assistant", ToolCalls: []ollamaToolCall{
			{Function: ollamaToolFunction{Name: "lookup", Arguments: json.RawMessage(`{"q":"a"}`)}},
			{Function: ollamaToolFunction{Name: "lookup", Arguments: json.RawMessage(`{"q":"b"}`)}},
		}},
		{Role: "tool", ToolName: "lookup", Content: "result-a"},
		{Role: "tool", ToolName: "lookup", Content: "result-b"},
	}
	out := translateOllamaMessagesToInternal(wire)
	if len(out[0].ToolCalls) != 2 {
		t.Fatalf("got %d tool_calls, want 2", len(out[0].ToolCalls))
	}
	idA := out[0].ToolCalls[0].ID
	idB := out[0].ToolCalls[1].ID
	if out[1].ToolCallID != idA {
		t.Errorf("first result: got %q, want first call's ID %q", out[1].ToolCallID, idA)
	}
	if out[2].ToolCallID != idB {
		t.Errorf("second result: got %q, want second call's ID %q", out[2].ToolCallID, idB)
	}
}

// TestTranslateOllamaMessagesToInternal_OrphanToolResult — when
// a client sends a tool result without a preceding assistant
// tool_call (replay / partial conversation), the translator
// must still emit a non-empty ToolCallID so downstream providers
// don't 400 on the empty field.
func TestTranslateOllamaMessagesToInternal_OrphanToolResult(t *testing.T) {
	wire := []ollamaChatMessage{
		{Role: "user", Content: "hi"},
		{Role: "tool", ToolName: "orphan", Content: "stray"},
	}
	out := translateOllamaMessagesToInternal(wire)
	if out[1].ToolCallID == "" {
		t.Error("orphan tool-result must still have a synthesized ToolCallID")
	}
	if out[1].Name != "orphan" {
		t.Errorf("Name = %q, want orphan", out[1].Name)
	}
}

// TestTranslateOllamaMessagesToInternal_MissingToolName —
// defensive: a tool-result without a name field still produces
// a usable message rather than a blank-name 400 downstream.
func TestTranslateOllamaMessagesToInternal_MissingToolName(t *testing.T) {
	wire := []ollamaChatMessage{
		{Role: "tool", Content: "result"},
	}
	out := translateOllamaMessagesToInternal(wire)
	if out[0].Name != "tool" {
		t.Errorf("missing name fallback: got %q, want 'tool'", out[0].Name)
	}
	if out[0].ToolCallID == "" {
		t.Error("missing name + no prior call: ID must still be non-empty")
	}
}

// TestOllamaToolCalls_RoundTrip_RequestSide pins the inbound
// translation: an Ollama client sends tool_calls with no
// id/type and arguments-as-object. The internal chat.ToolCall
// shape needs id + type + string-arguments, so the decoder
// fabricates id/type and re-encodes the arguments.
func TestOllamaToolCalls_RoundTrip_RequestSide(t *testing.T) {
	in := []ollamaToolCall{
		{Function: ollamaToolFunction{
			Name:      "get_weather",
			Arguments: json.RawMessage(`{"city":"Prague","unit":"c"}`),
		}},
	}
	out := ollamaToolCallsToInternal(in)
	if len(out) != 1 {
		t.Fatalf("got %d calls, want 1", len(out))
	}
	if out[0].ID == "" {
		t.Error("internal ToolCall.ID must be non-empty (downstream gates require it)")
	}
	if out[0].Type != "function" {
		t.Errorf("Type = %q, want function", out[0].Type)
	}
	if out[0].Function.Name != "get_weather" {
		t.Errorf("Function.Name = %q", out[0].Function.Name)
	}
	if out[0].Function.Arguments != `{"city":"Prague","unit":"c"}` {
		t.Errorf("Function.Arguments = %q (must round-trip as JSON string)", out[0].Function.Arguments)
	}
}

// TestOllamaToolCalls_EmptyArgumentsDefaultsToObject — when an
// Ollama client omits arguments entirely, encode as "{}" so the
// downstream provider gets a parseable object.
func TestOllamaToolCalls_EmptyArgumentsDefaultsToObject(t *testing.T) {
	in := []ollamaToolCall{
		{Function: ollamaToolFunction{Name: "noop"}},
	}
	out := ollamaToolCallsToInternal(in)
	if out[0].Function.Arguments != "{}" {
		t.Errorf("empty args: got %q, want {}", out[0].Function.Arguments)
	}
}

// TestOllamaToolCalls_RoundTrip_ResponseSide pins the outbound
// translation: providers return chat.ToolCall with id+type and
// arguments-as-string. The Ollama wire wants no id/type and
// arguments as a JSON object. Both columns translate.
func TestOllamaToolCalls_RoundTrip_ResponseSide(t *testing.T) {
	in := []chat.ToolCall{{
		ID:   "call_abc",
		Type: "function",
		Function: chat.FunctionCall{
			Name:      "get_weather",
			Arguments: `{"city":"Prague"}`,
		},
	}}
	out := internalToolCallsToOllama(in)
	if len(out) != 1 {
		t.Fatalf("got %d calls, want 1", len(out))
	}
	if out[0].Function.Name != "get_weather" {
		t.Errorf("Name = %q", out[0].Function.Name)
	}
	// Arguments must be a JSON object on the wire, not a string.
	var parsed map[string]string
	if err := json.Unmarshal(out[0].Function.Arguments, &parsed); err != nil {
		t.Fatalf("Arguments not a JSON object: %v (raw=%s)", err, out[0].Function.Arguments)
	}
	if parsed["city"] != "Prague" {
		t.Errorf("parsed Arguments = %v", parsed)
	}
}

// TestOllamaToolCalls_MalformedArgsBecomeNull — when the
// provider returns an arguments string that isn't valid JSON,
// the encoder emits null on the wire. Surfaces the bad arg to
// the client as "tool got no arguments" rather than dropping
// the whole tool call.
func TestOllamaToolCalls_MalformedArgsBecomeNull(t *testing.T) {
	in := []chat.ToolCall{{
		ID:       "call_x",
		Type:     "function",
		Function: chat.FunctionCall{Name: "broken", Arguments: "not-json"},
	}}
	out := internalToolCallsToOllama(in)
	if len(out) != 1 {
		t.Fatalf("got %d", len(out))
	}
	if string(out[0].Function.Arguments) != "null" {
		t.Errorf("malformed args: got %s, want null", out[0].Function.Arguments)
	}
}

// TestOllamaToolCalls_EmptyArgsBecomeObject — internal callers
// can leave Arguments as "" (Bedrock's converter does for some
// no-arg tools). Emit "{}" so the client sees a valid object.
func TestOllamaToolCalls_EmptyArgsBecomeObject(t *testing.T) {
	in := []chat.ToolCall{{
		Function: chat.FunctionCall{Name: "noop", Arguments: ""},
	}}
	out := internalToolCallsToOllama(in)
	if string(out[0].Function.Arguments) != "{}" {
		t.Errorf("empty args: got %s, want {}", out[0].Function.Arguments)
	}
}

// TestOllamaToolCalls_NilAndEmptyInputs pin the defensive shape.
// nil/empty inputs return nil so the JSON omitempty tag elides
// the tool_calls field from the wire entirely.
func TestOllamaToolCalls_NilAndEmptyInputs(t *testing.T) {
	if ollamaToolCallsToInternal(nil) != nil {
		t.Error("ollamaToolCallsToInternal(nil) must return nil")
	}
	if ollamaToolCallsToInternal([]ollamaToolCall{}) != nil {
		t.Error("ollamaToolCallsToInternal([]) must return nil")
	}
	if internalToolCallsToOllama(nil) != nil {
		t.Error("internalToolCallsToOllama(nil) must return nil")
	}
	if internalToolCallsToOllama([]chat.ToolCall{}) != nil {
		t.Error("internalToolCallsToOllama([]) must return nil")
	}
}

// TestOllamaChat_EmitsCorrectToolCallShape is the end-to-end
// regression test: a provider response carrying tool_calls in
// the internal shape must reach the wire as Ollama-shaped
// tool_calls. Open WebUI rejects responses where the
// arguments field is a string, and was the original failure
// mode the operator hit.
func TestOllamaChat_EmitsCorrectToolCallShape(t *testing.T) {
	resp := &chat.ChatResponse{Model: "stub"}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{
		Message: chat.Message{
			Role: "assistant",
			ToolCalls: []chat.ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: chat.FunctionCall{
					Name:      "current_time",
					Arguments: `{"tz":"UTC"}`,
				},
			}},
		},
		FinishReason: "tool_calls",
	})

	stub := &streamingStub{model: "stub", finalResp: resp}
	s := NewServer(WithChatProvider(stub))
	body := bytes.NewBufferString(`{"stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", body)
	rr := httptest.NewRecorder()
	s.OllamaChat(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	// Re-decode the wire response RAW so we can assert on the
	// JSON shape rather than going through ollamaChatResponse
	// (which would silently re-serialise).
	var wire struct {
		Message struct {
			ToolCalls []struct {
				ID       *string `json:"id"`
				Type     *string `json:"type"`
				Function struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &wire); err != nil {
		t.Fatalf("decode wire: %v — body=%s", err, rr.Body.String())
	}
	if len(wire.Message.ToolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1 (raw=%s)", len(wire.Message.ToolCalls), rr.Body.String())
	}
	tc := wire.Message.ToolCalls[0]
	if tc.ID != nil {
		t.Errorf("tool_call.id must be absent on Ollama wire (Open WebUI rejects when present); got %q", *tc.ID)
	}
	if tc.Type != nil {
		t.Errorf("tool_call.type must be absent on Ollama wire; got %q", *tc.Type)
	}
	// Arguments must be a JSON object, not a string. Re-decode to
	// confirm it parses as an object.
	var args map[string]any
	if err := json.Unmarshal(tc.Function.Arguments, &args); err != nil {
		t.Fatalf("arguments must be a JSON object on the wire, got string-ish: %v (raw=%s)", err, tc.Function.Arguments)
	}
	if args["tz"] != "UTC" {
		t.Errorf("arguments parsed = %v, want tz=UTC", args)
	}
}

// TestOllamaChat_RoundTripsToolResultFromClient covers the
// inbound side: a client sending a tool result back as
// role:"tool" with name + content must reach the internal
// Message in the right shape. The stub captures what landed.
func TestOllamaChat_RoundTripsToolResultFromClient(t *testing.T) {
	captured := &capturingStub{
		streamingStub: streamingStub{
			model:     "stub",
			finalResp: buildOllamaOKResponse("stub", "ok"),
		},
	}
	s := NewServer(WithChatProvider(captured))
	body := bytes.NewBufferString(`{
		"stream":false,
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"","tool_calls":[{"function":{"name":"f","arguments":{"x":1}}}]},
			{"role":"tool","name":"f","content":"42"}
		]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", body)
	rr := httptest.NewRecorder()
	s.OllamaChat(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if len(captured.captured) != 3 {
		t.Fatalf("captured %d messages, want 3", len(captured.captured))
	}
	assistant := captured.captured[1]
	if len(assistant.ToolCalls) != 1 {
		t.Errorf("assistant message lost tool_calls: %+v", assistant)
	}
	if assistant.ToolCalls[0].Function.Arguments != `{"x":1}` {
		t.Errorf("assistant tool args round-trip = %q", assistant.ToolCalls[0].Function.Arguments)
	}
	toolMsg := captured.captured[2]
	if toolMsg.Role != "tool" || toolMsg.Name != "f" || toolMsg.Content != "42" {
		t.Errorf("tool message round-trip lost fields: %+v", toolMsg)
	}
}

// capturingStub records every message the proxy forwards so
// translation tests can assert on the internal Message shape.
type capturingStub struct {
	streamingStub
	captured []chat.Message
}

func (c *capturingStub) CompleteWithTools(_ context.Context, msgs []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	c.captured = append(c.captured[:0], msgs...)
	return c.finalResp, c.err
}

// buildOllamaOKResponse fabricates a ChatResponse like Bedrock
// would return for a happy-path completion.
func buildOllamaOKResponse(model, content string) *chat.ChatResponse {
	resp := &chat.ChatResponse{Model: model}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{
		Message:      chat.Message{Role: "assistant", Content: content},
		FinishReason: "stop",
	})
	return resp
}

func twoSecondsAgo() time.Time {
	return time.Now().Add(-2 * time.Second)
}
