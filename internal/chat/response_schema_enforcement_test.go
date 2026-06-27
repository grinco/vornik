// Coverage for provider-side JSON Schema enforcement — item 7 of
// https://docs.vornik.io
//
// The wiring traverses three layers:
//
//   1. The executor stamps task.json.config.responseSchema +
//      config.responseFormat (= "json_schema") for any role with an
//      outputSchema. The agent entrypoint reads them and posts a
//      typed response_format directive to the chat-proxy
//      (verified end-to-end by the response_format_test.go suite
//      on the executor side).
//   2. The chat-proxy lifts the directive off the ChatRequest body
//      and stamps it on ctx via WithRequestResponseFormatStruct
//      (verified by api/chat_proxy tests).
//   3. Each Provider pulls the directive off ctx in its complete
//      path and serialises it onto the upstream wire body. This
//      file pins the per-provider behaviour at layer 3 — without it
//      the schema would silently die between chat-proxy and
//      upstream LLM.
//
// Three providers, three wire shapes:
//
//   - *Client (generic OpenAI-compat: Vertex, bedrock-access-gateway,
//     Ollama-with-tools, /v1/chat/completions self-hosted): emit
//     response_format on the JSON body verbatim. Native to the
//     OpenAI Chat Completions spec.
//   - Bedrock Converse: already implemented via synthetic emit_response
//     tool + ToolChoice forcing — verified by bedrock_toolchoice_*
//     tests. Touched here only to add a regression assertion that
//     the json_schema path doesn't surface a free-form fallback.
//   - Anthropic (claude_subscription): tool-use forcing — define a
//     synthetic emit_result tool whose input_schema IS the role's
//     schema, force tool_choice to that tool. Strongest portable
//     enforcement on Anthropic since the Messages API has no
//     native response_format field.

package chat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClient_Complete_PropagatesResponseFormatJSONSchemaFromContext
// — the non-streaming Complete path (and CompleteWithTools, which
// goes through the same doComplete dispatcher) must include the
// response_format directive on the outbound wire body when ctx
// carries one. Pre-fix the field was unset on Complete/CompleteWithTools
// even though the field exists on ChatRequest, so an OpenAI-compat
// upstream (bedrock-access-gateway in particular) received the call
// without any schema directive — gateway falls back to free-form
// output, model emits prose, validator fails. Item 7 of the
// deterministic-output-schema delivery plan.
func TestClient_Complete_PropagatesResponseFormatJSONSchemaFromContext(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"{}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "k", "m")
	schema := &ResponseFormat{
		Type: "json_schema",
		JSONSchema: &ResponseJSONSchema{
			Name:   "writer_result",
			Schema: json.RawMessage(`{"type":"object","properties":{"writing":{"type":"object"}},"required":["writing"],"additionalProperties":false}`),
			Strict: true,
		},
	}
	ctx := WithRequestResponseFormatStruct(context.Background(), schema)

	if _, err := client.Complete(ctx, []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(capturedBody, &got); err != nil {
		t.Fatalf("upstream request body invalid: %v\n%s", err, capturedBody)
	}
	rf, ok := got["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response_format absent on upstream body; got: %s", capturedBody)
	}
	if rf["type"] != "json_schema" {
		t.Errorf("response_format.type = %v, want json_schema", rf["type"])
	}
	js, ok := rf["json_schema"].(map[string]any)
	if !ok {
		t.Fatalf("response_format.json_schema absent or wrong type: %#v", rf["json_schema"])
	}
	if js["name"] != "writer_result" {
		t.Errorf("json_schema.name = %v, want writer_result", js["name"])
	}
	if js["schema"] == nil {
		t.Errorf("json_schema.schema must round-trip a non-nil body")
	}
	if js["strict"] != true {
		t.Errorf("json_schema.strict = %v, want true", js["strict"])
	}
}

// TestClient_CompleteWithTools_PropagatesResponseFormatJSONObject
// — when the role declares output validation but no outputSchema
// (item 8's json_object default), the typed shorthand still has to
// reach upstream. CompleteWithTools is the dispatcher's hot path,
// so verifying it independently of Complete pins the contract
// across both entry points.
func TestClient_CompleteWithTools_PropagatesResponseFormatJSONObject(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"{}"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "k", "m")
	ctx := WithRequestResponseFormatStruct(context.Background(), &ResponseFormat{Type: "json_object"})

	if _, err := client.CompleteWithTools(ctx, []Message{{Role: "user", Content: "hi"}}, nil); err != nil {
		t.Fatalf("CompleteWithTools: %v", err)
	}

	var got map[string]any
	_ = json.Unmarshal(capturedBody, &got)
	rf, ok := got["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response_format absent; got: %s", capturedBody)
	}
	if rf["type"] != "json_object" {
		t.Errorf("response_format.type = %v, want json_object", rf["type"])
	}
	if _, hasSchema := rf["json_schema"]; hasSchema {
		t.Errorf("json_object directive must not carry json_schema body; got: %#v", rf["json_schema"])
	}
}

// TestClient_Complete_OmitsResponseFormatWhenAbsent — the back-compat
// invariant: a request without a context-stamped response_format must
// emit a body with no response_format field at all (omitempty contract).
// Some upstreams (Ollama-direct, older self-hosted gateways) reject
// requests with response_format set to a value they don't understand;
// the previous-cycle stream-path bug serialized type:"" which broke
// these endpoints. Pin both shapes of "absent".
func TestClient_Complete_OmitsResponseFormatWhenAbsent(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "k", "m")
	if _, err := client.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if strings.Contains(string(capturedBody), `"response_format"`) {
		t.Errorf("response_format must be omitted when ctx has none; body: %s", capturedBody)
	}
}

// TestClaudeSubscription_BuildRequestBody_JSONSchemaForcesEmitResultTool
// — Anthropic's Messages API has no response_format field. The
// strongest portable enforcement is tool-use: register a synthetic
// emit_result tool whose input_schema IS the role's schema, then
// force tool_choice to that specific tool. The model literally
// cannot return non-conforming output — Anthropic validates the
// tool_use.input against the declared input_schema before returning
// the call. The response unwrap reads the tool_use.input back into
// Message.Content so the agent harness sees a free-form JSON reply
// without needing to recognise the synthetic tool.
//
// This is the green for item 7's Anthropic path. Pre-fix
// buildRequestBody ignored ResponseFormatStructFromContext entirely
// — every Anthropic call with a role outputSchema went out as
// free-form text, no schema enforcement.
func TestClaudeSubscription_BuildRequestBody_JSONSchemaForcesEmitResultTool(t *testing.T) {
	c := NewClaudeSubscriptionClient("claude-sonnet-4-6")
	schema := &ResponseFormat{
		Type: "json_schema",
		JSONSchema: &ResponseJSONSchema{
			Name:   "writer_result",
			Schema: json.RawMessage(`{"type":"object","properties":{"writing":{"type":"object"}},"required":["writing"],"additionalProperties":false}`),
			Strict: true,
		},
	}
	ctx := WithRequestResponseFormatStruct(context.Background(), schema)
	raw, err := c.buildRequestBodyCtx(ctx,
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		claudeAccountInfo{},
		"sess",
	)
	if err != nil {
		t.Fatalf("buildRequestBodyCtx: %v", err)
	}

	var body struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
		ToolChoice struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"tool_choice"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("invalid request body: %v\n%s", err, raw)
	}
	if len(body.Tools) == 0 {
		t.Fatalf("expected synthetic emit_result tool registered; got no tools")
	}
	// emit_result name uses the schema's Name. Find it.
	var found bool
	for _, tl := range body.Tools {
		if tl.Name == "writer_result" || tl.Name == "emit_result" {
			found = true
			// Anthropic uses input_schema; the raw schema should
			// reach it verbatim (modulo JSON whitespace) so the
			// model sees the exact constraints.
			var got map[string]any
			if err := json.Unmarshal(tl.InputSchema, &got); err != nil {
				t.Errorf("input_schema not parseable: %v\n%s", err, tl.InputSchema)
			}
			if got["type"] != "object" {
				t.Errorf("input_schema.type = %v, want object", got["type"])
			}
		}
	}
	if !found {
		t.Errorf("synthetic emit_result tool not registered; tools=%+v", body.Tools)
	}
	if body.ToolChoice.Type != "tool" {
		t.Errorf("tool_choice.type = %q, want %q (forced specific tool)",
			body.ToolChoice.Type, "tool")
	}
	if body.ToolChoice.Name == "" {
		t.Error("tool_choice.name must name the synthetic emit_result tool")
	}
}

// TestClaudeSubscription_BuildRequestBody_HonorsCustomDescription
// — operators who want the model to see a specific guidance string
// on the synthetic tool (e.g. "Emit the trade plan; do NOT call any
// other tools after this") can set ResponseJSONSchema.Description.
// The request builder must round-trip it; an absent value falls
// back to the generic default. Both paths exercised so a typo in
// the default string surfaces in tests.
func TestClaudeSubscription_BuildRequestBody_HonorsCustomDescription(t *testing.T) {
	c := NewClaudeSubscriptionClient("claude-sonnet-4-6")
	customDesc := "Emit the trade plan; this MUST be your final action."
	ctx := WithRequestResponseFormatStruct(context.Background(),
		&ResponseFormat{
			Type: "json_schema",
			JSONSchema: &ResponseJSONSchema{
				Name:        "trade_plan",
				Description: customDesc,
				Schema:      json.RawMessage(`{"type":"object","required":["plan"]}`),
			},
		})
	raw, err := c.buildRequestBodyCtx(ctx,
		[]Message{{Role: "user", Content: "hi"}}, nil,
		claudeAccountInfo{}, "sess")
	if err != nil {
		t.Fatalf("buildRequestBodyCtx: %v", err)
	}
	if !strings.Contains(string(raw), customDesc) {
		t.Errorf("custom description not propagated; body: %s", raw)
	}
}

// TestClaudeSubscription_BuildRequestBody_NoSchemaUnchanged — back-
// compat regression guard: when ctx has no json_schema directive,
// buildRequestBody emits the same body it always has. No phantom
// tools, no phantom tool_choice. A bug here would break every
// existing Anthropic-backed role overnight.
func TestClaudeSubscription_BuildRequestBody_NoSchemaUnchanged(t *testing.T) {
	c := NewClaudeSubscriptionClient("claude-sonnet-4-6")
	raw, err := c.buildRequestBodyCtx(context.Background(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		claudeAccountInfo{},
		"sess",
	)
	if err != nil {
		t.Fatalf("buildRequestBodyCtx: %v", err)
	}
	if strings.Contains(string(raw), `"tool_choice"`) {
		t.Errorf("no-schema path must not set tool_choice; body: %s", raw)
	}
	if strings.Contains(string(raw), `"tools"`) {
		t.Errorf("no-schema path must not register synthetic tools; body: %s", raw)
	}
}

// TestClaudeSubscription_BuildRequestBody_JSONObjectDoesNotForceTool
// — the json_object loose directive (item 8) is a Anthropic-no-op:
// Anthropic doesn't have a native response_format field, and we
// don't want to silently elevate json_object to tool-use forcing
// (json_object on a free-form reply is the right behaviour today
// for roles that haven't migrated to outputSchema). Pin that the
// loose form leaves the request body untouched.
func TestClaudeSubscription_BuildRequestBody_JSONObjectDoesNotForceTool(t *testing.T) {
	c := NewClaudeSubscriptionClient("claude-sonnet-4-6")
	ctx := WithRequestResponseFormatStruct(context.Background(),
		&ResponseFormat{Type: "json_object"})
	raw, err := c.buildRequestBodyCtx(ctx,
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		claudeAccountInfo{},
		"sess",
	)
	if err != nil {
		t.Fatalf("buildRequestBodyCtx: %v", err)
	}
	if strings.Contains(string(raw), `"tool_choice"`) {
		t.Errorf("json_object path must not force tool_choice on Anthropic; body: %s", raw)
	}
}

// TestClient_Stream_PropagatesResponseFormatJSONSchemaFromContext —
// the streaming path already wires ResponseFormatStructFromContext
// onto the request literal (see stream.go); pin the full json_schema
// shape end-to-end so a future refactor that drops the field doesn't
// silently regress. Streaming is the dominant production path for
// agent harness LLM calls, so this complements the non-streaming
// coverage above.
func TestClient_Stream_PropagatesResponseFormatJSONSchemaFromContext(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := &Client{
		endpoint:   srv.URL,
		model:      "m",
		httpClient: &http.Client{},
	}
	schema := &ResponseFormat{
		Type: "json_schema",
		JSONSchema: &ResponseJSONSchema{
			Name:   "researcher_result",
			Schema: json.RawMessage(`{"type":"object","required":["research"]}`),
			Strict: true,
		},
	}
	ctx := WithRequestResponseFormatStruct(context.Background(), schema)

	if _, err := client.CompleteWithToolsStream(ctx,
		[]Message{{Role: "user", Content: "hi"}}, nil, nil); err != nil {
		t.Fatalf("CompleteWithToolsStream: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(capturedBody, &got); err != nil {
		t.Fatalf("body parse: %v", err)
	}
	rf, ok := got["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response_format absent from stream body; got: %s", capturedBody)
	}
	if rf["type"] != "json_schema" {
		t.Errorf("stream type = %v, want json_schema", rf["type"])
	}
	js, ok := rf["json_schema"].(map[string]any)
	if !ok {
		t.Fatalf("json_schema body missing on stream: %#v", rf["json_schema"])
	}
	if js["name"] != "researcher_result" {
		t.Errorf("stream schema name = %v, want researcher_result", js["name"])
	}
}

// TestUnwrapEmitResultToolCall covers the response-side mirror of
// the tool-use enforcement: when the model emits a single tool_call
// matching the synthetic emit_result tool, the arguments must
// surface as Message.Content and ToolCalls clears so the agent
// harness's dispatch loop doesn't try to execute a tool it doesn't
// have. Without this unwrap, Anthropic json_schema enforcement
// would surface as an "unknown tool: emit_result" loop on every
// affected role.
func TestUnwrapEmitResultToolCall(t *testing.T) {
	t.Run("single matching tool_call → unwrapped", func(t *testing.T) {
		resp := &ChatResponse{
			Choices: []struct {
				Index        int     `json:"index"`
				Message      Message `json:"message"`
				FinishReason string  `json:"finish_reason"`
			}{
				{
					Index: 0,
					Message: Message{
						Role: "assistant",
						ToolCalls: []ToolCall{{
							ID: "x", Type: "function",
							Function: FunctionCall{
								Name:      "writer_result",
								Arguments: `{"writing":{"written":true}}`,
							},
						}},
					},
					FinishReason: "tool_calls",
				},
			},
		}
		if !unwrapEmitResultToolCall(resp, "writer_result") {
			t.Fatal("expected unwrap to fire on matching name")
		}
		got := resp.Choices[0]
		if got.Message.Content != `{"writing":{"written":true}}` {
			t.Errorf("content not unwrapped; got: %q", got.Message.Content)
		}
		if len(got.Message.ToolCalls) != 0 {
			t.Errorf("tool_calls must be cleared; got: %#v", got.Message.ToolCalls)
		}
		if got.FinishReason != "stop" {
			t.Errorf("finish_reason = %q, want %q", got.FinishReason, "stop")
		}
	})

	t.Run("name mismatch → no-op", func(t *testing.T) {
		resp := &ChatResponse{
			Choices: []struct {
				Index        int     `json:"index"`
				Message      Message `json:"message"`
				FinishReason string  `json:"finish_reason"`
			}{
				{
					Message: Message{
						ToolCalls: []ToolCall{{
							Function: FunctionCall{Name: "file_read", Arguments: `{"path":"x"}`},
						}},
					},
					FinishReason: "tool_calls",
				},
			},
		}
		if unwrapEmitResultToolCall(resp, "writer_result") {
			t.Error("non-matching tool name must not unwrap")
		}
		if resp.Choices[0].Message.Content != "" {
			t.Error("non-matching unwrap must leave content untouched")
		}
		if len(resp.Choices[0].Message.ToolCalls) != 1 {
			t.Error("non-matching unwrap must preserve tool_calls")
		}
	})

	t.Run("multi-tool-call response → no-op (mid-conversation)", func(t *testing.T) {
		// When the model called file_read mid-turn before its
		// emit_result call, the response carries multiple tool_calls.
		// We must NOT unwrap — the agent harness's tool dispatch
		// loop has to process the file_read first; emit_result will
		// arrive on a later turn alone.
		resp := &ChatResponse{
			Choices: []struct {
				Index        int     `json:"index"`
				Message      Message `json:"message"`
				FinishReason string  `json:"finish_reason"`
			}{
				{
					Message: Message{
						ToolCalls: []ToolCall{
							{Function: FunctionCall{Name: "file_read", Arguments: `{"path":"a"}`}},
							{Function: FunctionCall{Name: "writer_result", Arguments: `{}`}},
						},
					},
				},
			},
		}
		if unwrapEmitResultToolCall(resp, "writer_result") {
			t.Error("multi-tool-call response must not be unwrapped")
		}
	})

	t.Run("empty name → no-op", func(t *testing.T) {
		resp := &ChatResponse{}
		if unwrapEmitResultToolCall(resp, "") {
			t.Error("empty name must short-circuit to no-op")
		}
	})

	t.Run("nil response → no-op", func(t *testing.T) {
		if unwrapEmitResultToolCall(nil, "writer_result") {
			t.Error("nil response must short-circuit to no-op")
		}
	})

	t.Run("zero choices → no-op (defensive)", func(t *testing.T) {
		resp := &ChatResponse{}
		if unwrapEmitResultToolCall(resp, "writer_result") {
			t.Error("empty Choices must short-circuit to no-op")
		}
	})

	t.Run("multiple choices → no-op (n-best path)", func(t *testing.T) {
		// Some providers (OpenAI with n>1) return multiple choices;
		// unwrap only applies to the single-choice path the daemon
		// uses today. Document the boundary so a future n>1 caller
		// doesn't get a half-unwrapped result.
		resp := &ChatResponse{
			Choices: []struct {
				Index        int     `json:"index"`
				Message      Message `json:"message"`
				FinishReason string  `json:"finish_reason"`
			}{
				{Message: Message{ToolCalls: []ToolCall{{Function: FunctionCall{Name: "emit_result", Arguments: `{}`}}}}},
				{Message: Message{Content: "alt"}},
			},
		}
		if unwrapEmitResultToolCall(resp, "emit_result") {
			t.Error("multi-choice response must not be unwrapped (single-choice contract)")
		}
	})

	t.Run("empty arguments → unwrap with {} placeholder", func(t *testing.T) {
		// Some Anthropic streams emit a tool_use with no input
		// blocks at all (rare, but observed). The unwrap should
		// still fire and surface an empty object so the agent
		// harness's parser doesn't see an empty string and skip
		// the validate path entirely.
		resp := &ChatResponse{
			Choices: []struct {
				Index        int     `json:"index"`
				Message      Message `json:"message"`
				FinishReason string  `json:"finish_reason"`
			}{{
				Message: Message{
					ToolCalls: []ToolCall{{
						Function: FunctionCall{Name: "emit_result", Arguments: ""},
					}},
				},
			}},
		}
		if !unwrapEmitResultToolCall(resp, "emit_result") {
			t.Fatal("empty-args unwrap should still fire")
		}
		if resp.Choices[0].Message.Content != "{}" {
			t.Errorf("empty-args content = %q, want %q", resp.Choices[0].Message.Content, "{}")
		}
	})

	t.Run("preserves coexisting text content", func(t *testing.T) {
		// Anthropic spec allows a text block before tool_use.
		// Preserve it as a suffix so the operator can still see
		// model prose during debugging without breaking the JSON
		// parser at the head of Content.
		resp := &ChatResponse{
			Choices: []struct {
				Index        int     `json:"index"`
				Message      Message `json:"message"`
				FinishReason string  `json:"finish_reason"`
			}{{
				Message: Message{
					Content: "I will emit my answer now.",
					ToolCalls: []ToolCall{{
						Function: FunctionCall{Name: "emit_result", Arguments: `{"ok":true}`},
					}},
				},
			}},
		}
		if !unwrapEmitResultToolCall(resp, "emit_result") {
			t.Fatal("unwrap should fire")
		}
		got := resp.Choices[0].Message.Content
		// JSON must come first (jq parser tolerates trailing text
		// when the leading char is `{`).
		if !strings.HasPrefix(got, `{"ok":true}`) {
			t.Errorf("content must lead with JSON; got: %q", got)
		}
		if !strings.Contains(got, "I will emit my answer now.") {
			t.Errorf("pre-existing text must be preserved; got: %q", got)
		}
	})
}

// TestSyntheticEmitResultName covers the helper the request builder
// and response unwrapper agree on. A drift between them would
// surface as a silent enforcement bypass — the request would
// register tool "X" while the unwrapper looked for "Y", so the
// model's response would slip through as a tool_call the agent
// harness can't dispatch.
func TestSyntheticEmitResultName(t *testing.T) {
	t.Run("no response_format on ctx → empty", func(t *testing.T) {
		if got := syntheticEmitResultName(context.Background()); got != "" {
			t.Errorf("no-format ctx: got %q, want empty", got)
		}
	})

	t.Run("json_object directive → empty (no unwrap needed)", func(t *testing.T) {
		ctx := WithRequestResponseFormatStruct(context.Background(),
			&ResponseFormat{Type: "json_object"})
		if got := syntheticEmitResultName(ctx); got != "" {
			t.Errorf("json_object ctx: got %q, want empty", got)
		}
	})

	t.Run("json_schema with named schema → uses name", func(t *testing.T) {
		ctx := WithRequestResponseFormatStruct(context.Background(),
			&ResponseFormat{
				Type: "json_schema",
				JSONSchema: &ResponseJSONSchema{
					Name:   "writer_result",
					Schema: json.RawMessage(`{}`),
				},
			})
		if got := syntheticEmitResultName(ctx); got != "writer_result" {
			t.Errorf("named schema: got %q, want writer_result", got)
		}
	})

	t.Run("json_schema with empty name → emit_result default", func(t *testing.T) {
		ctx := WithRequestResponseFormatStruct(context.Background(),
			&ResponseFormat{
				Type:       "json_schema",
				JSONSchema: &ResponseJSONSchema{Schema: json.RawMessage(`{}`)},
			})
		if got := syntheticEmitResultName(ctx); got != "emit_result" {
			t.Errorf("unnamed schema: got %q, want emit_result default", got)
		}
	})
}

// TestClaudeSubscription_Complete_UnwrapsEmitResultEndToEnd is the
// integration-style proof for item 7's Anthropic path: a Complete
// call with ctx-stamped json_schema produces a request that
// registers the synthetic emit_result tool AND a response that
// unwraps the tool_use back into Message.Content. Without both
// halves, the agent harness either sends an enforcement-less
// request OR receives a tool_call it can't dispatch.
func TestClaudeSubscription_Complete_UnwrapsEmitResultEndToEnd(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(credsPath, []byte(`{"claudeAiOauth":{"accessToken":"tok","refreshToken":"r","expiresAt":99999999999999}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		// Mock response: model called emit_result with a valid JSON
		// payload. SSE event ordering matches Anthropic's real shape
		// (message_start → content_block_start with tool_use →
		// input_json_delta chunks → content_block_stop →
		// message_delta with stop_reason=tool_use).
		_, _ = io.WriteString(w,
			"event: message_start\n"+
				`data: {"type":"message_start","message":{"id":"x","model":"claude-sonnet-4-6","usage":{"input_tokens":1,"output_tokens":0}}}`+"\n\n"+
				"event: content_block_start\n"+
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tool_1","name":"writer_result","input":{}}}`+"\n\n"+
				"event: content_block_delta\n"+
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"writing\":{\"written\":true}}"}}`+"\n\n"+
				"event: content_block_stop\n"+
				`data: {"type":"content_block_stop","index":0}`+"\n\n"+
				"event: message_delta\n"+
				`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":12}}`+"\n\n"+
				"event: message_stop\n"+
				`data: {"type":"message_stop"}`+"\n\n",
		)
	}))
	defer srv.Close()
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)

	c := NewClaudeSubscriptionClient("claude-sonnet-4-6",
		WithClaudeSubscriptionAuthPath(credsPath),
	)
	schema := &ResponseFormat{
		Type: "json_schema",
		JSONSchema: &ResponseJSONSchema{
			Name:   "writer_result",
			Schema: json.RawMessage(`{"type":"object","required":["writing"],"properties":{"writing":{"type":"object"}}}`),
			Strict: true,
		},
	}
	ctx := WithRequestResponseFormatStruct(context.Background(), schema)

	resp, err := c.Complete(ctx, []Message{{Role: "user", Content: "go"}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Request side: confirm emit_result tool + tool_choice landed
	// in the upstream request body.
	var sent struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		ToolChoice struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"tool_choice"`
	}
	if err := json.Unmarshal(capturedBody, &sent); err != nil {
		t.Fatalf("parse upstream body: %v\n%s", err, capturedBody)
	}
	foundTool := false
	for _, tl := range sent.Tools {
		if tl.Name == "writer_result" {
			foundTool = true
			break
		}
	}
	if !foundTool {
		t.Errorf("synthetic emit_result tool missing from upstream tools=%+v", sent.Tools)
	}
	if sent.ToolChoice.Type != "tool" || sent.ToolChoice.Name != "writer_result" {
		t.Errorf("tool_choice not pinned to writer_result: %+v", sent.ToolChoice)
	}

	// Response side: the synthetic tool_call was unwrapped into
	// Message.Content; the agent harness sees a regular JSON reply.
	if len(resp.Choices) != 1 {
		t.Fatalf("want 1 choice, got %d", len(resp.Choices))
	}
	msg := resp.Choices[0].Message
	if msg.Content != `{"writing":{"written":true}}` {
		t.Errorf("unwrapped content mismatch: %q", msg.Content)
	}
	if len(msg.ToolCalls) != 0 {
		t.Errorf("ToolCalls must be cleared after unwrap; got: %+v", msg.ToolCalls)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason should flip to stop after unwrap; got: %q",
			resp.Choices[0].FinishReason)
	}
}

// TestClaudeSubscription_BuildRequestBody_JSONSchemaCoexistsWithCallerTools
// — when the caller already passed tools (the agent harness's
// file_read / file_write / run_shell), the synthetic emit_result tool
// must coexist with them, not replace them. Tool_choice still pins
// to emit_result so the model emits the final answer via the
// synthetic tool while having access to the caller's tools for any
// intermediate work the model decides to do. (Anthropic's tool_choice
// = specific tool DOES allow other tools to be called too on
// preceding turns; the forcing only constrains the final turn that
// produces stop_reason=end_turn.)
func TestClaudeSubscription_BuildRequestBody_JSONSchemaCoexistsWithCallerTools(t *testing.T) {
	c := NewClaudeSubscriptionClient("claude-sonnet-4-6")
	callerTools := []Tool{{
		Type: "function",
		Function: ToolFunction{
			Name:        "file_read",
			Description: "Read a file",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		},
	}}
	schema := &ResponseFormat{
		Type: "json_schema",
		JSONSchema: &ResponseJSONSchema{
			Name:   "researcher_result",
			Schema: json.RawMessage(`{"type":"object","required":["research"],"properties":{"research":{"type":"object"}}}`),
		},
	}
	ctx := WithRequestResponseFormatStruct(context.Background(), schema)
	raw, err := c.buildRequestBodyCtx(ctx,
		[]Message{{Role: "user", Content: "hi"}}, callerTools,
		claudeAccountInfo{}, "sess")
	if err != nil {
		t.Fatalf("buildRequestBodyCtx: %v", err)
	}

	var body struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(raw, &body)
	if len(body.Tools) < 2 {
		t.Fatalf("expected >=2 tools (caller + synthetic); got: %+v", body.Tools)
	}
	names := make(map[string]bool, len(body.Tools))
	for _, tl := range body.Tools {
		names[tl.Name] = true
	}
	if !names["file_read"] {
		t.Errorf("caller's file_read tool dropped; tools=%+v", body.Tools)
	}
	if !names["researcher_result"] && !names["emit_result"] {
		t.Errorf("synthetic emit_result tool missing; tools=%+v", body.Tools)
	}
}
