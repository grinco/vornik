// Regression coverage for the Ollama <-> internal translation
// layer in ollama_proxy.go, focused on the failure modes that
// produced operator-visible incidents:
//
//   - assistant content silently dropped ("client gets no
//     response") — content must round-trip VERBATIM through
//     translateChatResponseToOllama, including alongside
//     tool_calls and for content that looks like markup/JSON.
//   - finish_reason -> done_reason mapping for the non-"stop"
//     reasons (tool_calls / length / content_filter) that
//     agentic clients gate on; empty -> defaults to "stop".
//   - usage token counts copied onto the envelope.
//   - nil / empty ChatResponse -> safe done:true envelope (no
//     panic, no empty Model).
//   - tool_calls round-trip in both directions, and the
//     tool_call_id backfill-by-name logic (the Gemini "Expected
//     tool_call_id" fix) including two parallel calls to the same
//     tool resolved by position.
//
// These complement (do not duplicate) the helper tests already in
// ollama_proxy_test.go. All new helpers carry the "xl" prefix.

package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
)

// xlBuildResponse fabricates a chat.ChatResponse with one choice
// carrying the given content, tool_calls, and finish_reason. It
// mirrors the anonymous Choices struct shape so translation tests
// can exercise non-"stop" finish reasons (buildOllamaOKResponse is
// hard-wired to "stop").
func xlBuildResponse(model, content, finishReason string, tcs []chat.ToolCall) *chat.ChatResponse {
	resp := &chat.ChatResponse{Model: model}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{
		Message: chat.Message{
			Role:      "assistant",
			Content:   content,
			ToolCalls: tcs,
		},
		FinishReason: finishReason,
	})
	return resp
}

// TestXLTranslateResponse_ContentNeverDropped is the core
// regression guard for the "client gets no response" symptom: the
// assistant content the provider returned must appear VERBATIM on
// the Ollama envelope's message.content. We cover plain text plus
// content that itself contains JSON / newlines / markup, which is
// where naive re-encoding regressions tend to mangle or blank the
// field.
func TestXLTranslateResponse_ContentNeverDropped(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"plain", "the answer is 42"},
		{"empty_is_preserved_not_panicking", ""},
		{"unicode", "héllo · 世界 · 🌍"},
		{"json_looking", `{"nested":"value","n":1}`},
		{"multiline_markdown", "line one\n\n```go\nfmt.Println(\"x\")\n```\nline two"},
		{"leading_trailing_space", "   spaced   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := xlBuildResponse("m", tc.content, "stop", nil)
			got := translateChatResponseToOllama(resp, "fallback", twoSecondsAgo())
			if got.Message.Content != tc.content {
				t.Errorf("content mangled: got %q, want %q (verbatim)", got.Message.Content, tc.content)
			}
			if got.Message.Role != "assistant" {
				t.Errorf("role = %q, want assistant", got.Message.Role)
			}
			// And it must survive a JSON marshal/unmarshal round-trip
			// (the actual wire path) without being dropped.
			b, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var wire ollamaChatResponse
			if err := json.Unmarshal(b, &wire); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if wire.Message.Content != tc.content {
				t.Errorf("content lost over the wire: got %q, want %q", wire.Message.Content, tc.content)
			}
		})
	}
}

// TestXLTranslateResponse_ContentPreservedAlongsideToolCalls — a
// response that carries BOTH text and a tool_call must keep the
// text. Some providers (Claude) interleave a natural-language
// preamble with a tool call; dropping the content here is the
// "model went silent before calling the tool" symptom.
func TestXLTranslateResponse_ContentPreservedAlongsideToolCalls(t *testing.T) {
	resp := xlBuildResponse("m", "Let me check the time for you.", "tool_calls", []chat.ToolCall{{
		ID:   "call_1",
		Type: "function",
		Function: chat.FunctionCall{
			Name:      "current_time",
			Arguments: `{"tz":"UTC"}`,
		},
	}})
	got := translateChatResponseToOllama(resp, "fallback", twoSecondsAgo())
	if got.Message.Content != "Let me check the time for you." {
		t.Errorf("content dropped when tool_calls present: got %q", got.Message.Content)
	}
	if len(got.Message.ToolCalls) != 1 {
		t.Fatalf("got %d tool_calls, want 1", len(got.Message.ToolCalls))
	}
	if got.Message.ToolCalls[0].Function.Name != "current_time" {
		t.Errorf("tool name = %q", got.Message.ToolCalls[0].Function.Name)
	}
	// Args must be an object on the wire shape, not a string.
	var args map[string]any
	if err := json.Unmarshal(got.Message.ToolCalls[0].Function.Arguments, &args); err != nil {
		t.Fatalf("tool args not a JSON object: %v", err)
	}
	if args["tz"] != "UTC" {
		t.Errorf("args = %v, want tz=UTC", args)
	}
}

// TestXLTranslateResponse_FinishReasonToDoneReason pins the
// finish_reason -> done_reason mapping for every reason agentic
// clients gate on. A client deciding "do I execute a tool?" reads
// done_reason=="tool_calls"; a regression that always returns
// "stop" silently breaks tool execution.
func TestXLTranslateResponse_FinishReasonToDoneReason(t *testing.T) {
	cases := []struct {
		finishReason   string
		wantDoneReason string
	}{
		{"stop", "stop"},
		{"tool_calls", "tool_calls"},
		{"length", "length"},
		{"content_filter", "content_filter"},
		{"", "stop"}, // empty defaults to "stop"
	}
	for _, tc := range cases {
		name := tc.finishReason
		if name == "" {
			name = "empty_defaults_stop"
		}
		t.Run(name, func(t *testing.T) {
			resp := xlBuildResponse("m", "x", tc.finishReason, nil)
			got := translateChatResponseToOllama(resp, "fallback", twoSecondsAgo())
			if got.DoneReason != tc.wantDoneReason {
				t.Errorf("finish_reason %q -> done_reason %q, want %q",
					tc.finishReason, got.DoneReason, tc.wantDoneReason)
			}
			if !got.Done {
				t.Error("Done must be true on the final translation")
			}
		})
	}
}

// TestXLTranslateResponse_UsageCopied — the prompt/completion
// token counts must be copied onto the envelope so client-side
// spend dashboards (and Open WebUI's token readout) populate.
func TestXLTranslateResponse_UsageCopied(t *testing.T) {
	resp := xlBuildResponse("m", "x", "stop", nil)
	resp.Usage.PromptTokens = 123
	resp.Usage.CompletionTokens = 456
	got := translateChatResponseToOllama(resp, "fallback", twoSecondsAgo())
	if got.PromptEvalCount != 123 {
		t.Errorf("PromptEvalCount = %d, want 123", got.PromptEvalCount)
	}
	if got.EvalCount != 456 {
		t.Errorf("EvalCount = %d, want 456", got.EvalCount)
	}
}

// TestXLTranslateResponse_ModelFallbackWhenResponseEmpty — when
// the provider's response carries no Model, the caller-supplied
// fallback model is used; a non-empty response Model wins.
func TestXLTranslateResponse_ModelFallbackWhenResponseEmpty(t *testing.T) {
	t.Run("empty_response_model_uses_fallback", func(t *testing.T) {
		resp := xlBuildResponse("", "x", "stop", nil)
		got := translateChatResponseToOllama(resp, "fallback-model", twoSecondsAgo())
		if got.Model != "fallback-model" {
			t.Errorf("Model = %q, want fallback-model", got.Model)
		}
	})
	t.Run("response_model_wins", func(t *testing.T) {
		resp := xlBuildResponse("real-model", "x", "stop", nil)
		got := translateChatResponseToOllama(resp, "fallback-model", twoSecondsAgo())
		if got.Model != "real-model" {
			t.Errorf("Model = %q, want real-model", got.Model)
		}
	})
}

// TestXLTranslateResponse_NoChoicesSafeEnvelope — a response with
// an empty Choices slice (not nil resp, but no completion choice)
// must still produce a safe done:true envelope with an empty
// message rather than panicking on Choices[0].
func TestXLTranslateResponse_NoChoicesSafeEnvelope(t *testing.T) {
	resp := &chat.ChatResponse{Model: "m"}
	got := translateChatResponseToOllama(resp, "fallback", twoSecondsAgo())
	if !got.Done {
		t.Error("Done must be true")
	}
	if got.DoneReason != "stop" {
		t.Errorf("DoneReason = %q, want default stop", got.DoneReason)
	}
	if got.Message.Content != "" {
		t.Errorf("Content = %q, want empty (no choice)", got.Message.Content)
	}
}

// TestXLTranslateResponse_NilSafeEnvelope — a nil response must
// not panic and must return a done:true envelope on the fallback
// model. (Complements the existing _NilResp test by also pinning
// DoneReason and that the duration field is populated.)
func TestXLTranslateResponse_NilSafeEnvelope(t *testing.T) {
	got := translateChatResponseToOllama(nil, "fb", time.Now().Add(-time.Second))
	if got.Model != "fb" {
		t.Errorf("Model = %q, want fb", got.Model)
	}
	if !got.Done || got.DoneReason != "stop" {
		t.Errorf("got Done=%v DoneReason=%q, want true/stop", got.Done, got.DoneReason)
	}
	if got.TotalDuration <= 0 {
		t.Errorf("TotalDuration = %d, want positive", got.TotalDuration)
	}
}

// TestXLToInternal_ContentAndRolesPreservedVerbatim — every
// message's role and content must survive the inbound translation
// unchanged. The translator's "interesting" job is tool-result
// attribution; this guards that it doesn't disturb ordinary
// system/user/assistant text in the process.
func TestXLToInternal_ContentAndRolesPreservedVerbatim(t *testing.T) {
	wire := []ollamaChatMessage{
		{Role: "system", Content: "you are a helpful assistant"},
		{Role: "user", Content: "what's 2+2?\nshow work"},
		{Role: "assistant", Content: "It is 4."},
	}
	out := translateOllamaMessagesToInternal(wire)
	if len(out) != 3 {
		t.Fatalf("got %d messages, want 3", len(out))
	}
	for i := range wire {
		if out[i].Role != wire[i].Role {
			t.Errorf("msg %d role = %q, want %q", i, out[i].Role, wire[i].Role)
		}
		if out[i].Content != wire[i].Content {
			t.Errorf("msg %d content = %q, want %q (verbatim)", i, out[i].Content, wire[i].Content)
		}
	}
}

// TestXLToInternal_AssistantContentKeptAlongsideToolCalls — an
// assistant message can carry both a textual preamble and
// tool_calls. The translator must keep the content while still
// recording the fabricated IDs for later tool-result attribution.
func TestXLToInternal_AssistantContentKeptAlongsideToolCalls(t *testing.T) {
	wire := []ollamaChatMessage{
		{Role: "assistant", Content: "checking now", ToolCalls: []ollamaToolCall{
			{Function: ollamaToolFunction{Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`)}},
		}},
		{Role: "tool", ToolName: "lookup", Content: "found"},
	}
	out := translateOllamaMessagesToInternal(wire)
	if out[0].Content != "checking now" {
		t.Errorf("assistant content dropped: got %q", out[0].Content)
	}
	if len(out[0].ToolCalls) != 1 {
		t.Fatalf("assistant lost tool_calls")
	}
	// The tool result must bind to the assistant's fabricated ID.
	if out[1].ToolCallID == "" || out[1].ToolCallID != out[0].ToolCalls[0].ID {
		t.Errorf("tool result ID = %q, want assistant call ID %q", out[1].ToolCallID, out[0].ToolCalls[0].ID)
	}
}

// TestXLToInternal_FabricatedIDFormat pins the deterministic
// `ollama-call-<idx>-<name>` ID shape. Downstream gates and the
// Bedrock converter rely on a stable, non-empty correlation token;
// the index prefix is what disambiguates parallel calls.
func TestXLToInternal_FabricatedIDFormat(t *testing.T) {
	wire := []ollamaChatMessage{
		{Role: "assistant", ToolCalls: []ollamaToolCall{
			{Function: ollamaToolFunction{Name: "alpha", Arguments: json.RawMessage(`{}`)}},
			{Function: ollamaToolFunction{Name: "beta", Arguments: json.RawMessage(`{}`)}},
		}},
	}
	out := translateOllamaMessagesToInternal(wire)
	if got := out[0].ToolCalls[0].ID; got != "ollama-call-0-alpha" {
		t.Errorf("first ID = %q, want ollama-call-0-alpha", got)
	}
	if got := out[0].ToolCalls[1].ID; got != "ollama-call-1-beta" {
		t.Errorf("second ID = %q, want ollama-call-1-beta", got)
	}
}

// TestXLToInternal_ParallelSameToolResolvedByPosition is the
// Gemini "Expected tool_call_id" fix's hardest case: two parallel
// calls to the SAME tool. Results must bind first-in-flight /
// first-to-return — result #1 -> call #1's ID, result #2 -> call
// #2's ID — never cross-bound or both to the first.
func TestXLToInternal_ParallelSameToolResolvedByPosition(t *testing.T) {
	wire := []ollamaChatMessage{
		{Role: "assistant", ToolCalls: []ollamaToolCall{
			{Function: ollamaToolFunction{Name: "search", Arguments: json.RawMessage(`{"q":"first"}`)}},
			{Function: ollamaToolFunction{Name: "search", Arguments: json.RawMessage(`{"q":"second"}`)}},
		}},
		{Role: "tool", ToolName: "search", Content: "result-1"},
		{Role: "tool", ToolName: "search", Content: "result-2"},
	}
	out := translateOllamaMessagesToInternal(wire)
	idFirst := out[0].ToolCalls[0].ID
	idSecond := out[0].ToolCalls[1].ID
	if idFirst == idSecond {
		t.Fatalf("parallel calls must have distinct IDs, both = %q", idFirst)
	}
	if out[1].ToolCallID != idFirst {
		t.Errorf("result-1 bound to %q, want first call %q", out[1].ToolCallID, idFirst)
	}
	if out[2].ToolCallID != idSecond {
		t.Errorf("result-2 bound to %q, want second call %q", out[2].ToolCallID, idSecond)
	}
	// Both tool messages carry the resolved name for downstream
	// correlation.
	if out[1].Name != "search" || out[2].Name != "search" {
		t.Errorf("tool-result names = %q/%q, want search/search", out[1].Name, out[2].Name)
	}
}

// TestXLToInternal_InterleavedDistinctTools — multiple distinct
// tools called and resolved out of declaration order still bind by
// name. Guards that the per-name pending queues don't bleed into
// each other.
func TestXLToInternal_InterleavedDistinctTools(t *testing.T) {
	wire := []ollamaChatMessage{
		{Role: "assistant", ToolCalls: []ollamaToolCall{
			{Function: ollamaToolFunction{Name: "weather", Arguments: json.RawMessage(`{}`)}},
			{Function: ollamaToolFunction{Name: "stock", Arguments: json.RawMessage(`{}`)}},
		}},
		// stock result arrives before weather result.
		{Role: "tool", ToolName: "stock", Content: "AAPL 200"},
		{Role: "tool", ToolName: "weather", Content: "sunny"},
	}
	out := translateOllamaMessagesToInternal(wire)
	weatherID := out[0].ToolCalls[0].ID
	stockID := out[0].ToolCalls[1].ID
	if out[1].ToolCallID != stockID {
		t.Errorf("stock result bound to %q, want %q", out[1].ToolCallID, stockID)
	}
	if out[2].ToolCallID != weatherID {
		t.Errorf("weather result bound to %q, want %q", out[2].ToolCallID, weatherID)
	}
}

// TestXLToolCalls_BothDirectionsRoundTrip — full request->internal
// ->response round-trip of a single tool call. Confirms the
// inbound translator fabricates id+type+string-args and the
// outbound translator strips id/type and re-objectifies args,
// landing back at the original object form. (The function name and
// argument values are the load-bearing invariant across the round
// trip.)
func TestXLToolCalls_BothDirectionsRoundTrip(t *testing.T) {
	wireIn := []ollamaToolCall{
		{Function: ollamaToolFunction{
			Name:      "get_weather",
			Arguments: json.RawMessage(`{"city":"Brno","unit":"c"}`),
		}},
	}
	internal := ollamaToolCallsToInternal(wireIn)
	if internal[0].ID == "" || internal[0].Type != "function" {
		t.Fatalf("inbound must fabricate id+type, got id=%q type=%q", internal[0].ID, internal[0].Type)
	}
	if internal[0].Function.Arguments != `{"city":"Brno","unit":"c"}` {
		t.Errorf("inbound args = %q, want JSON string verbatim", internal[0].Function.Arguments)
	}

	wireOut := internalToolCallsToOllama(internal)
	if wireOut[0].Function.Name != "get_weather" {
		t.Errorf("outbound name = %q", wireOut[0].Function.Name)
	}
	// id/type must be absent on the outbound wire shape.
	b, err := json.Marshal(wireOut[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), `"id"`) || strings.Contains(string(b), `"type"`) {
		t.Errorf("outbound wire must not carry id/type: %s", b)
	}
	var args map[string]string
	if err := json.Unmarshal(wireOut[0].Function.Arguments, &args); err != nil {
		t.Fatalf("outbound args not an object: %v", err)
	}
	if args["city"] != "Brno" || args["unit"] != "c" {
		t.Errorf("round-tripped args = %v, want city=Brno unit=c", args)
	}
}
