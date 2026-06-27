package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	bedrockdoc "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// TestSyntheticEmitResponseName_FromContext mirrors the Anthropic-side
// syntheticEmitResultName helper: the unwrap key must match the name
// the request builder pinned via ToolChoice. Drift between the two
// would leave the synthetic tool call in Message.ToolCalls and the
// agent harness would 404 on emit_response.
func TestSyntheticEmitResponseName_FromContext(t *testing.T) {
	cases := []struct {
		name string
		rf   *ResponseFormat
		want string
	}{
		{
			name: "no response_format on context",
			rf:   nil,
			want: "",
		},
		{
			name: "json_object (no json_schema)",
			rf:   &ResponseFormat{Type: "json_object"},
			want: "",
		},
		{
			name: "json_schema with explicit name",
			rf: &ResponseFormat{Type: "json_schema", JSONSchema: &ResponseJSONSchema{
				Name:   "writer_result",
				Schema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`),
			}},
			want: "writer_result",
		},
		{
			name: "json_schema without name defaults to emit_response",
			rf: &ResponseFormat{Type: "json_schema", JSONSchema: &ResponseJSONSchema{
				Name:   "",
				Schema: json.RawMessage(`{"type":"object"}`),
			}},
			want: "emit_response",
		},
		{
			name: "json_schema with whitespace-only name still defaults",
			rf: &ResponseFormat{Type: "json_schema", JSONSchema: &ResponseJSONSchema{
				Name:   "   ",
				Schema: json.RawMessage(`{"type":"object"}`),
			}},
			want: "emit_response",
		},
		{
			name: "json_schema without JSONSchema body returns empty",
			rf:   &ResponseFormat{Type: "json_schema"},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.rf != nil {
				ctx = WithRequestResponseFormatStruct(ctx, tc.rf)
			}
			got := syntheticEmitResponseName(ctx)
			if got != tc.want {
				t.Fatalf("syntheticEmitResponseName = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestUnwrapEmitResponseToolCall_RewritesContent — the headline
// contract. A Bedrock response that contains a single emit_response
// tool call must have its tool input lifted into Message.Content as
// the JSON string, ToolCalls cleared, and FinishReason flipped from
// "tool_calls" to "stop" so the agent harness's tool dispatcher
// doesn't 404 on the synthetic tool.
func TestUnwrapEmitResponseToolCall_RewritesContent(t *testing.T) {
	args := `{"message":"hello","approved":true}`
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
						ID:   "tu_123",
						Type: "function",
						Function: FunctionCall{
							Name:      "emit_response",
							Arguments: args,
						},
					}},
				},
				FinishReason: "tool_calls",
			},
		},
	}

	ok := unwrapEmitResponseToolCall(resp, "emit_response")
	if !ok {
		t.Fatal("expected unwrap to succeed for a clean single-tool response")
	}
	if resp.Choices[0].Message.Content != args {
		t.Errorf("Content not lifted from tool args:\n  got  = %q\n  want = %q",
			resp.Choices[0].Message.Content, args)
	}
	if len(resp.Choices[0].Message.ToolCalls) != 0 {
		t.Errorf("ToolCalls not cleared: %+v", resp.Choices[0].Message.ToolCalls)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("FinishReason not flipped: got %q want %q", resp.Choices[0].FinishReason, "stop")
	}
}

// TestUnwrapEmitResponseToolCall_PreservesExistingContent — when the
// model emits both text and the emit_response call, the text is
// preserved as a suffix so nothing is lost. Anthropic's spec allows
// this; same for Bedrock.
func TestUnwrapEmitResponseToolCall_PreservesExistingContent(t *testing.T) {
	args := `{"x":1}`
	resp := &ChatResponse{
		Choices: []struct {
			Index        int     `json:"index"`
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		}{
			{
				Index: 0,
				Message: Message{
					Role:    "assistant",
					Content: "here is my answer",
					ToolCalls: []ToolCall{{
						Function: FunctionCall{Name: "emit_response", Arguments: args},
					}},
				},
				FinishReason: "tool_calls",
			},
		},
	}
	if !unwrapEmitResponseToolCall(resp, "emit_response") {
		t.Fatal("expected unwrap to succeed")
	}
	// JSON args land FIRST so the agent harness's jq parser sees a top-
	// level JSON object; the prior text becomes a suffix.
	if !strings.HasPrefix(resp.Choices[0].Message.Content, args) {
		t.Errorf("expected Content to start with JSON args; got %q", resp.Choices[0].Message.Content)
	}
	if !strings.Contains(resp.Choices[0].Message.Content, "here is my answer") {
		t.Errorf("pre-existing content dropped: %q", resp.Choices[0].Message.Content)
	}
}

// TestUnwrapEmitResponseToolCall_EmptyArgsFallsBackToObject — an
// empty arguments string would produce invalid downstream JSON. The
// unwrap path substitutes `{}` so the agent harness still parses.
func TestUnwrapEmitResponseToolCall_EmptyArgsFallsBackToObject(t *testing.T) {
	resp := &ChatResponse{
		Choices: []struct {
			Index        int     `json:"index"`
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		}{
			{
				Message: Message{
					ToolCalls: []ToolCall{{
						Function: FunctionCall{Name: "emit_response", Arguments: ""},
					}},
				},
				FinishReason: "tool_calls",
			},
		},
	}
	if !unwrapEmitResponseToolCall(resp, "emit_response") {
		t.Fatal("expected unwrap to succeed with empty args")
	}
	if resp.Choices[0].Message.Content != "{}" {
		t.Errorf("empty args should fall back to {}, got %q", resp.Choices[0].Message.Content)
	}
}

// TestUnwrapEmitResponseToolCall_GuardConditions — the unwrap must
// no-op when the response shape isn't unambiguous (multiple tool
// calls, wrong tool name, nil out, empty name). Mis-firing here
// would silently drop legitimate tool calls.
func TestUnwrapEmitResponseToolCall_GuardConditions(t *testing.T) {
	cases := []struct {
		name string
		resp *ChatResponse
		tool string
	}{
		{
			name: "nil response",
			resp: nil,
			tool: "emit_response",
		},
		{
			name: "empty name",
			resp: &ChatResponse{
				Choices: []struct {
					Index        int     `json:"index"`
					Message      Message `json:"message"`
					FinishReason string  `json:"finish_reason"`
				}{
					{Message: Message{ToolCalls: []ToolCall{{Function: FunctionCall{Name: "emit_response", Arguments: "{}"}}}}},
				},
			},
			tool: "",
		},
		{
			name: "wrong tool name",
			resp: &ChatResponse{
				Choices: []struct {
					Index        int     `json:"index"`
					Message      Message `json:"message"`
					FinishReason string  `json:"finish_reason"`
				}{
					{Message: Message{ToolCalls: []ToolCall{{Function: FunctionCall{Name: "file_read", Arguments: "{}"}}}}},
				},
			},
			tool: "emit_response",
		},
		{
			name: "two tool calls — intermediate file_read + emit_response",
			resp: &ChatResponse{
				Choices: []struct {
					Index        int     `json:"index"`
					Message      Message `json:"message"`
					FinishReason string  `json:"finish_reason"`
				}{
					{Message: Message{ToolCalls: []ToolCall{
						{Function: FunctionCall{Name: "file_read", Arguments: "{}"}},
						{Function: FunctionCall{Name: "emit_response", Arguments: `{"x":1}`}},
					}}},
				},
			},
			tool: "emit_response",
		},
		{
			name: "no choices",
			resp: &ChatResponse{Choices: nil},
			tool: "emit_response",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok := unwrapEmitResponseToolCall(tc.resp, tc.tool)
			if ok {
				t.Fatalf("expected unwrap to no-op, but it fired")
			}
			// Verify nothing got mutated when the function returned false.
			if tc.resp != nil && len(tc.resp.Choices) > 0 && tc.resp.Choices[0].Message.Content != "" {
				t.Errorf("guard fired but Content was set: %q", tc.resp.Choices[0].Message.Content)
			}
		})
	}
}

// TestBedrockOutputToChatResponse_UnwrapsRecordedEmitResponse drives
// the full flow that the BedrockProvider.complete() path goes through
// when a Converse response carries a synthetic emit_response tool
// call. The recorded Bedrock-shaped output (ConverseOutputMember
// Message containing a ToolUseBlock named "emit_response") goes
// through bedrockOutputToChatResponse first (which surfaces the
// tool call as Message.ToolCalls), then through
// unwrapEmitResponseToolCall — the same composition complete() does.
//
// The end state must satisfy three properties:
//  1. Message.Content carries the tool's input JSON verbatim.
//  2. Message.ToolCalls no longer contains the synthetic call.
//  3. FinishReason flips from "tool_calls" to "stop" so the
//     downstream dispatcher exits its loop cleanly.
//
// This is the regression test for the "agent 404s on emit_response"
// failure mode the unwrap exists to prevent.
func TestBedrockOutputToChatResponse_UnwrapsRecordedEmitResponse(t *testing.T) {
	// Build a Bedrock output that matches what Bedrock Converse
	// emits when ToolChoice forced a SpecificToolChoice on
	// emit_response. We use the SDK types directly so the test
	// catches any drift in bedrockOutputToChatResponse's
	// extractToolCallsFromContent path.
	args := map[string]any{
		"message":  "task succeeded",
		"approved": true,
	}
	out := newRecordedEmitResponseBedrockOutput(t, "emit_response", args)
	resp := bedrockOutputToChatResponse(out, "bedrock.test-model", bedrockStopReasonToolUse(), nil, "req-1")

	// Pre-unwrap: tool call should be present (that's the failure
	// mode the unwrap exists to fix).
	if len(resp.Choices) != 1 || len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("expected exactly one tool call before unwrap, got %+v",
			resp.Choices)
	}

	// Same composition complete() does: surface the tool name from
	// ctx, call the unwrap. Since this test bypasses complete()'s
	// ctx-plumbing, simulate by passing the known synthetic name.
	if !unwrapEmitResponseToolCall(resp, "emit_response") {
		t.Fatal("expected unwrap to succeed on the recorded response")
	}

	// (1) Content is the JSON-encoded args.
	wantJSON, _ := json.Marshal(args)
	if resp.Choices[0].Message.Content != string(wantJSON) {
		t.Errorf("Content not the tool input JSON:\n  got  = %q\n  want = %q",
			resp.Choices[0].Message.Content, string(wantJSON))
	}
	// (2) ToolCalls cleared.
	if len(resp.Choices[0].Message.ToolCalls) != 0 {
		t.Errorf("ToolCalls not cleared after unwrap: %+v",
			resp.Choices[0].Message.ToolCalls)
	}
	// (3) FinishReason flipped.
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("FinishReason not flipped: got %q want %q",
			resp.Choices[0].FinishReason, "stop")
	}
}

// TestBedrockResponse_UnwrapMatchesAnthropicSymmetry — pin the
// invariant the unwrap exists to enforce: a Bedrock response carrying
// a single emit_response tool call is observationally identical to a
// regular Anthropic JSON response post-unwrap. Without this symmetry,
// the agent harness would treat the two paths differently (one
// dispatches a tool it can't execute; the other parses JSON).
func TestBedrockResponse_UnwrapMatchesAnthropicSymmetry(t *testing.T) {
	args := `{"message":"approved","approved":true}`
	resp := &ChatResponse{
		Choices: []struct {
			Index        int     `json:"index"`
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		}{
			{
				Message: Message{
					Role: "assistant",
					ToolCalls: []ToolCall{{
						ID:       "tu_x",
						Type:     "function",
						Function: FunctionCall{Name: "emit_response", Arguments: args},
					}},
				},
				FinishReason: "tool_calls",
			},
		},
	}
	if !unwrapEmitResponseToolCall(resp, "emit_response") {
		t.Fatal("expected unwrap to succeed")
	}
	// Same final shape Anthropic's unwrapEmitResultToolCall produces.
	if resp.Choices[0].Message.Content != args {
		t.Errorf("Bedrock unwrap not symmetric with Anthropic: got %q want %q",
			resp.Choices[0].Message.Content, args)
	}
	if len(resp.Choices[0].Message.ToolCalls) != 0 {
		t.Errorf("ToolCalls not cleared (asymmetric with Anthropic): %+v",
			resp.Choices[0].Message.ToolCalls)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("FinishReason not flipped (asymmetric with Anthropic): %q",
			resp.Choices[0].FinishReason)
	}
}

// newRecordedEmitResponseBedrockOutput builds a ConverseOutput shaped
// like a Bedrock response that carried a single emit_response tool
// call (the shape buildJSONSchemaEnforcementTool + ToolChoice pin
// produce in production). Lifted into a helper so the integration
// test reads as a contract rather than fixture-wiring detail.
func newRecordedEmitResponseBedrockOutput(t *testing.T, toolName string, args map[string]any) *bedrocktypes.ConverseOutputMemberMessage {
	t.Helper()
	return &bedrocktypes.ConverseOutputMemberMessage{
		Value: bedrocktypes.Message{
			Role: bedrocktypes.ConversationRoleAssistant,
			Content: []bedrocktypes.ContentBlock{
				&bedrocktypes.ContentBlockMemberToolUse{
					Value: bedrocktypes.ToolUseBlock{
						ToolUseId: aws.String("tu_recorded_1"),
						Name:      aws.String(toolName),
						Input:     bedrockdoc.NewLazyDocument(args),
					},
				},
			},
		},
	}
}

// bedrockStopReasonToolUse names the StopReason value the SDK
// returns when the model lands on a tool_use turn. Wrapped in a
// helper to keep the integration test legible (the SDK constant is
// verbose).
func bedrockStopReasonToolUse() bedrocktypes.StopReason {
	return bedrocktypes.StopReasonToolUse
}
