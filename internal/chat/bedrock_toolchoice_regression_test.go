package chat

import (
	"context"
	"encoding/json"
	"testing"

	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// TestBuildConverseInput_DoesNotForceToolChoiceWithJSONObject — the
// regression observed 2026-05-08. Pre-fix, when responseFormat was
// "json_object" AND tools were offered, the converter set
// ToolChoice=any, which means Bedrock forces stop_reason: tool_use
// on every turn. The agentic loop's exit condition
// (finish_reason != "tool_calls") never fires and the model burns
// its full iteration budget without ever emitting a final
// result.json. This caused the assistant-swarm researcher
// (outputSchema → effectiveResponseFormat = "json_object") to
// loop 28 turns and fail schema validation.
//
// The contract: tools offered + json_object set → ToolChoice MUST
// remain unset (Bedrock default = auto). The model decides when
// to stop calling tools and emit the final answer. The
// prompt-injected schema text + post-validation provide the
// shape contract without blocking termination.
//
// json_schema's distinct path (synthetic emit_response tool) is
// covered separately and still correctly pins ToolChoice to the
// synthetic tool — there the tool IS the final answer carrier.
func TestBuildConverseInput_DoesNotForceToolChoiceWithJSONObject(t *testing.T) {
	p := &BedrockProvider{model: "test-model", region: "us-east-1"}

	tools := []Tool{
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "memory_search",
				Description: "Search project memory",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
			},
		},
	}
	msgs := []Message{
		{Role: "system", Content: "you are a researcher"},
		{Role: "user", Content: "search for X"},
	}

	ctx := WithRequestResponseFormat(context.Background(), "json_object")
	in, err := p.buildConverseInput(ctx, msgs, tools)
	if err != nil {
		t.Fatalf("buildConverseInput: %v", err)
	}
	if in.ToolConfig == nil {
		t.Fatalf("ToolConfig must be set when tools are passed")
	}
	if in.ToolConfig.ToolChoice != nil {
		t.Errorf("ToolChoice must be nil (Bedrock default = auto) when json_object + tools — pre-fix this was forced to ToolChoiceMemberAny, blocking final answer emission. got %T", in.ToolConfig.ToolChoice)
	}
	if len(in.ToolConfig.Tools) != 1 {
		t.Errorf("got %d tools in config, want 1", len(in.ToolConfig.Tools))
	}
}

// TestBuildConverseInput_NoToolChoiceWithoutJSONObject — flip-side:
// when no responseFormat is set, ToolChoice must also stay nil.
// Verifies we didn't accidentally introduce forcing on the
// no-format path.
func TestBuildConverseInput_NoToolChoiceWithoutJSONObject(t *testing.T) {
	p := &BedrockProvider{model: "test-model", region: "us-east-1"}

	tools := []Tool{{
		Type: "function",
		Function: ToolFunction{
			Name:       "noop",
			Parameters: json.RawMessage(`{"type":"object"}`),
		},
	}}
	msgs := []Message{{Role: "user", Content: "hi"}}

	in, err := p.buildConverseInput(context.Background(), msgs, tools)
	if err != nil {
		t.Fatalf("buildConverseInput: %v", err)
	}
	if in.ToolConfig != nil && in.ToolConfig.ToolChoice != nil {
		t.Errorf("no-format path must leave ToolChoice unset; got %T", in.ToolConfig.ToolChoice)
	}
}

// TestBuildConverseInput_JSONSchemaStillPinsToolChoice — the
// json_schema path is functionally distinct: the synthetic
// emit_response tool IS the final answer carrier, so pinning
// ToolChoice to that specific tool is correct (the model
// doesn't need a free-form text exit). Pin the contract so
// removing the json_object forcing didn't accidentally
// regress json_schema's enforcement.
func TestBuildConverseInput_JSONSchemaStillPinsToolChoice(t *testing.T) {
	p := &BedrockProvider{model: "test-model", region: "us-east-1"}

	schema := &ResponseFormat{
		Type: "json_schema",
		JSONSchema: &ResponseJSONSchema{
			Name:   "researcher_output",
			Schema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`),
			Strict: true,
		},
	}
	ctx := WithRequestResponseFormatStruct(context.Background(), schema)

	in, err := p.buildConverseInput(ctx, []Message{{Role: "user", Content: "go"}}, nil)
	if err != nil {
		t.Fatalf("buildConverseInput: %v", err)
	}
	if in.ToolConfig == nil || in.ToolConfig.ToolChoice == nil {
		t.Fatalf("json_schema must pin ToolChoice (synthetic emit_response tool); got nil")
	}
	specific, ok := in.ToolConfig.ToolChoice.(*bedrocktypes.ToolChoiceMemberTool)
	if !ok {
		t.Fatalf("json_schema ToolChoice must be ToolChoiceMemberTool (specific tool), got %T", in.ToolConfig.ToolChoice)
	}
	if specific.Value.Name == nil || *specific.Value.Name == "" {
		t.Errorf("specific tool name must be set on the synthetic emit tool")
	}
}
