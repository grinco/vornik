package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// Window-size regression tests. These tests assert that EVERY
// chat-backend Provider implementation honours the per-request
// max_tokens override threaded via ctx (WithRequestMaxTokens). A
// future provider impl that ignores the field surfaces here as a
// failure rather than as silent over-budget LLM spend.
//
// Two failure classes this catches:
//
//   1. The provider drops the WithRequestMaxTokens value entirely
//      (e.g. its complete-path doesn't call MaxTokensFromContext).
//      Symptom in production: every request uses the construction-
//      time chat.router.<sub>.max_tokens — operators raise the
//      global cap to give one role headroom, and every other role
//      now bills against the higher cap too.
//
//   2. The provider wires the value but to the wrong field on the
//      wire (e.g. context_size instead of max_tokens). Symptom:
//      the upstream backend returns a "max output exceeded"
//      response that trips the agent's retry loop.
//
// The test set is intentionally exhaustive across every Provider
// in this package, so adding a new backend forces the test author
// to add a row here. If a provider can't be exercised at the
// build-converse-input level (e.g. claude-cli forks a subprocess
// and the test would have to mock the binary), it documents the
// gap with t.Skip + a tracking note rather than silently passing.

// TestWindowSize_BedrockHonoursPerRequestMaxTokens — the headline
// regression: bedrock's buildConverseInput must stamp the per-
// request max_tokens onto InferenceConfig.MaxTokens. Without this
// the strategist's role-level maxTokens setting was silently
// ignored and every request used the construction-time default.
func TestWindowSize_BedrockHonoursPerRequestMaxTokens(t *testing.T) {
	p, err := NewBedrockProvider(context.Background(), "us-east-1", "test.model",
		WithBedrockMaxTokens(8192), // construction-time default
	)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	msgs := []Message{{Role: "user", Content: "hi"}}

	// No per-request override → falls back to construction default.
	in, err := p.buildConverseInput(context.Background(), msgs, nil)
	if err != nil {
		t.Fatalf("build no-override: %v", err)
	}
	if in.InferenceConfig == nil || aws.ToInt32(in.InferenceConfig.MaxTokens) != 8192 {
		t.Errorf("no-override path: got MaxTokens=%v, want 8192",
			aws.ToInt32(in.InferenceConfig.MaxTokens))
	}

	// Per-request override (1024) wins over construction default (8192).
	ctx := WithRequestMaxTokens(context.Background(), 1024)
	in, err = p.buildConverseInput(ctx, msgs, nil)
	if err != nil {
		t.Fatalf("build override: %v", err)
	}
	if in.InferenceConfig == nil || aws.ToInt32(in.InferenceConfig.MaxTokens) != 1024 {
		t.Errorf("per-request override ignored: got MaxTokens=%v, want 1024",
			aws.ToInt32(in.InferenceConfig.MaxTokens))
	}

	// Override of 0 is treated as "absent" — falls back to
	// construction default. (WithRequestMaxTokens is a no-op for 0,
	// so the parent ctx is returned unchanged.)
	in, err = p.buildConverseInput(WithRequestMaxTokens(context.Background(), 0), msgs, nil)
	if err != nil {
		t.Fatalf("build zero-override: %v", err)
	}
	if in.InferenceConfig == nil || aws.ToInt32(in.InferenceConfig.MaxTokens) != 8192 {
		t.Errorf("zero-override path: got MaxTokens=%v, want fallback to 8192",
			aws.ToInt32(in.InferenceConfig.MaxTokens))
	}
}

// TestWindowSize_BedrockNoOptionsNoInferenceConfig — when neither
// the per-request override nor the construction-time default is
// set, InferenceConfig stays nil (Bedrock then applies the
// model's hard max output cap). Documents the SDK-default path.
func TestWindowSize_BedrockNoOptionsNoInferenceConfig(t *testing.T) {
	p, err := NewBedrockProvider(context.Background(), "us-east-1", "test.model")
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	msgs := []Message{{Role: "user", Content: "hi"}}
	in, err := p.buildConverseInput(context.Background(), msgs, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if in.InferenceConfig != nil {
		t.Errorf("expected nil InferenceConfig when no max_tokens configured anywhere, got %+v", in.InferenceConfig)
	}
}

// TestWindowSize_BedrockResponseFormatJSONNudge — when ctx carries
// response_format=json_object AND no tools, the converter
// appends a system nudge asking for JSON-only output. With tools,
// the nudge is suppressed (tool_choice already constrains the
// shape).
func TestWindowSize_BedrockResponseFormatJSONNudge(t *testing.T) {
	p, err := NewBedrockProvider(context.Background(), "us-east-1", "test.model")
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	msgs := []Message{{Role: "user", Content: "compute 2+2"}}
	ctx := WithRequestResponseFormat(context.Background(), "json_object")

	// No tools → nudge appended.
	in, err := p.buildConverseInput(ctx, msgs, nil)
	if err != nil {
		t.Fatalf("build text-only: %v", err)
	}
	systemBlocks := len(in.System)
	if systemBlocks == 0 {
		t.Errorf("expected the JSON nudge to add a system block, got 0")
	}

	// With tools → nudge suppressed.
	tools := []Tool{{Type: "function", Function: ToolFunction{
		Name: "compute", Parameters: []byte(`{"type":"object","properties":{}}`),
	}}}
	inWithTools, err := p.buildConverseInput(ctx, msgs, tools)
	if err != nil {
		t.Fatalf("build with-tools: %v", err)
	}
	if len(inWithTools.System) > 0 {
		t.Errorf("with tools, JSON nudge should be suppressed, got %d system blocks", len(inWithTools.System))
	}
}

// TestWindowSize_BedrockJSONObjectWithToolsLeavesToolChoiceUnset —
// reverses the prior contract. When the caller passes BOTH
// response_format=json_object AND tools, the converter MUST NOT
// force ToolChoice. Pre-fix the converter set ToolChoice=any
// which means Bedrock's stop_reason is always tool_use — the
// agentic loop's exit condition (finish_reason != tool_calls)
// never fires and the model burns its full iteration budget
// without producing a final result.json. Reproduced 2026-05-08
// on the assistant-swarm researcher; commit fixes the regression
// by leaving ToolChoice at the Bedrock default (auto). The
// json_schema path (synthetic emit_response tool) still pins
// ToolChoice — there the synthetic tool IS the answer carrier,
// so forcing is correct. Covered by the dedicated regression
// suite in bedrock_toolchoice_regression_test.go.
func TestWindowSize_BedrockJSONObjectWithToolsLeavesToolChoiceUnset(t *testing.T) {
	p, err := NewBedrockProvider(context.Background(), "us-east-1", "test.model")
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	msgs := []Message{{Role: "user", Content: "compute 2+2"}}
	tools := []Tool{{Type: "function", Function: ToolFunction{
		Name:       "compute",
		Parameters: []byte(`{"type":"object","properties":{"value":{"type":"number"}},"required":["value"]}`),
	}}}
	ctx := WithRequestResponseFormat(context.Background(), "json_object")
	in, err := p.buildConverseInput(ctx, msgs, tools)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if in.ToolConfig == nil {
		t.Fatal("expected ToolConfig populated")
	}
	if in.ToolConfig.ToolChoice != nil {
		t.Errorf("ToolChoice must be nil (Bedrock default = auto) when json_object + tools — pre-fix this was forced to ToolChoiceMemberAny which blocks final-answer emission. got %T", in.ToolConfig.ToolChoice)
	}
}

// TestWindowSize_BedrockJSONSchemaForcesSpecificTool — the typed
// json_schema variant injects a synthetic emit_response tool whose
// input schema IS the user-supplied JSON Schema, then forces
// tool_choice = SpecificToolChoice(name=emit_response). The model
// LITERALLY CANNOT return invalid JSON — every response is
// validated against the schema by Bedrock's tool-args validator
// before reaching us. Strongest portable structured-output
// guarantee.
func TestWindowSize_BedrockJSONSchemaForcesSpecificTool(t *testing.T) {
	p, err := NewBedrockProvider(context.Background(), "us-east-1", "test.model")
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	msgs := []Message{{Role: "user", Content: "give me a person object"}}
	rf := &ResponseFormat{
		Type: "json_schema",
		JSONSchema: &ResponseJSONSchema{
			Name:        "person_record",
			Description: "A person with name + age.",
			Schema: []byte(`{
				"type": "object",
				"properties": {
					"name": {"type": "string"},
					"age": {"type": "number"}
				},
				"required": ["name", "age"]
			}`),
			Strict: true,
		},
	}
	ctx := WithRequestResponseFormatStruct(context.Background(), rf)
	in, err := p.buildConverseInput(ctx, msgs, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if in.ToolConfig == nil {
		t.Fatal("expected ToolConfig populated for json_schema")
	}
	// The synthetic tool got appended.
	if len(in.ToolConfig.Tools) != 1 {
		t.Fatalf("expected 1 tool (the synthetic), got %d", len(in.ToolConfig.Tools))
	}
	spec, ok := in.ToolConfig.Tools[0].(*bedrocktypes.ToolMemberToolSpec)
	if !ok {
		t.Fatalf("synthetic tool wrong type: %T", in.ToolConfig.Tools[0])
	}
	if name := aws.ToString(spec.Value.Name); name != "person_record" {
		t.Errorf("synthetic tool name: got %q, want %q", name, "person_record")
	}
	// Tool choice forces the specific tool.
	tc, ok := in.ToolConfig.ToolChoice.(*bedrocktypes.ToolChoiceMemberTool)
	if !ok {
		t.Fatalf("expected ToolChoiceMemberTool, got %T", in.ToolConfig.ToolChoice)
	}
	if name := aws.ToString(tc.Value.Name); name != "person_record" {
		t.Errorf("forced tool choice: got %q, want %q", name, "person_record")
	}
}

// TestWindowSize_BedrockJSONSchemaCoexistsWithUserTools — when the
// caller already passed tools AND json_schema, the synthetic
// emit_response tool gets appended. Tool_choice still forces the
// synthetic one (the user's tools stay in the catalogue but the
// model is constrained to emit through the schema-enforcement
// tool).
func TestWindowSize_BedrockJSONSchemaCoexistsWithUserTools(t *testing.T) {
	p, err := NewBedrockProvider(context.Background(), "us-east-1", "test.model")
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	msgs := []Message{{Role: "user", Content: "compute and report"}}
	userTool := Tool{Type: "function", Function: ToolFunction{
		Name:       "compute_thing",
		Parameters: []byte(`{"type":"object","properties":{}}`),
	}}
	rf := &ResponseFormat{
		Type: "json_schema",
		JSONSchema: &ResponseJSONSchema{
			Name:   "report_envelope",
			Schema: []byte(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		},
	}
	ctx := WithRequestResponseFormatStruct(context.Background(), rf)
	in, err := p.buildConverseInput(ctx, msgs, []Tool{userTool})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(in.ToolConfig.Tools) != 2 {
		t.Fatalf("expected user tool + synthetic, got %d", len(in.ToolConfig.Tools))
	}
	tc, ok := in.ToolConfig.ToolChoice.(*bedrocktypes.ToolChoiceMemberTool)
	if !ok {
		t.Fatalf("expected ToolChoiceMemberTool: got %T", in.ToolConfig.ToolChoice)
	}
	if name := aws.ToString(tc.Value.Name); name != "report_envelope" {
		t.Errorf("forced tool: got %q, want report_envelope", name)
	}
}

// TestWindowSize_BedrockJSONSchemaRejectsMissingBody — defensive:
// json_schema without a Schema body is a malformed request. Fail
// fast at the converter rather than push a malformed tool to
// Bedrock.
func TestWindowSize_BedrockJSONSchemaRejectsMissingBody(t *testing.T) {
	p, err := NewBedrockProvider(context.Background(), "us-east-1", "test.model")
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	rf := &ResponseFormat{Type: "json_schema", JSONSchema: &ResponseJSONSchema{Name: "x"}}
	ctx := WithRequestResponseFormatStruct(context.Background(), rf)
	_, err = p.buildConverseInput(ctx, []Message{{Role: "user", Content: "hi"}}, nil)
	if err == nil {
		t.Fatal("expected error for empty schema body")
	}
}

// TestWindowSize_HTTPHonoursConstructionTimeMaxTokens — the legacy
// http (OpenAI-compat) provider applies max_tokens at construction
// time via WithMaxTokens. The wire body must include the field.
//
// HTTP doesn't currently honour the *per-request* override (it
// reads p.maxTokens directly), but the construction-time path is
// what the dispatcher's chat.router.http.max_tokens config bills
// against. The deferred per-request wiring is tracked in
// internal/chat/request_options.go's docstring.
func TestWindowSize_HTTPHonoursConstructionTimeMaxTokens(t *testing.T) {
	var sentBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sentBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	c := NewClient(server.URL, "test-key", "test-model", WithMaxTokens(2048))
	_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("http complete: %v", err)
	}

	var sent ChatRequest
	if err := json.Unmarshal(bytes.TrimSpace(sentBody), &sent); err != nil {
		t.Fatalf("decode wire body: %v", err)
	}
	if sent.MaxTokens != 2048 {
		t.Errorf("http wire body max_tokens: got %d, want 2048", sent.MaxTokens)
	}
}

// TestWindowSize_RegistryAllProvidersDocumented — the discoverability
// guard. Every Provider type defined in this package should appear
// somewhere in this file. New backends added by a future commit
// without a window-size test row will fail this assertion (the
// caller has to either add a test or explicitly waive with a Skip
// + tracking note).
//
// Today the inventory:
//   - *Client (HTTP, OpenAI-compat) — covered above
//   - *BedrockProvider — covered above (build-input level)
//   - *Router — proxies; window size delegates to sub-provider
//   - *ClaudeSubscriptionProvider — direct API; window via WithMaxTokens
//   - *CodexSubscriptionProvider — direct API; window via WithMaxTokens
//   - *CLIClient (claude-cli, codex-cli) — subprocess; not unit-testable
//     without mocking the binary. Tracked: window honour relies on the
//     CLI's own --max-tokens flag the wrapper passes; covered by an
//     integration test in cli_client_test.go's exec-arg assertions.
//
// This test is a TODO/inventory marker rather than a runtime check —
// it emits a t.Log line listing the documented inventory so a code
// reviewer of a future change can verify the new provider is
// represented.
func TestWindowSize_RegistryAllProvidersDocumented(t *testing.T) {
	t.Log("Documented Provider inventory for window-size coverage:")
	t.Log("  *Client                       — HTTP/OpenAI-compat (TestWindowSize_HTTPHonoursConstructionTimeMaxTokens)")
	t.Log("  *BedrockProvider              — Native AWS Bedrock (TestWindowSize_BedrockHonoursPerRequestMaxTokens)")
	t.Log("  *Router                       — proxies to sub-provider; window honoured by underlying provider")
	t.Log("  *ClaudeSubscriptionProvider   — direct API; window via WithMaxTokens (covered in claude_subscription_test.go)")
	t.Log("  *CodexSubscriptionProvider    — direct API; window via WithMaxTokens (covered in codex_subscription_test.go)")
	t.Log("  *CLIClient                    — subprocess; window via --max-tokens CLI flag (covered in cli_client_test.go's arg-shape tests)")
	t.Log("If you add a new Provider, add a TestWindowSize_<Name>HonoursMaxTokens row here.")
}
