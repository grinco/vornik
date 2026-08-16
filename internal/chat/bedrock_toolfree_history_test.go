package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// Regression 2026-08-16: Bedrock Converse rejected every tool-free
// finalisation turn with
//
//	ValidationException: The toolConfig field must be defined when using
//	toolUse and toolResult content blocks.
//
// buildConverseInput attached ToolConfig only when the caller passed tools,
// but openAIMessagesToBedrockWithCache always emits toolUse/toolResult blocks
// from history. The agent's schema-finalisation turn (entrypoint.sh) sends
// tools:[] after a tool phase, so every Bedrock-backed role with an
// outputSchema that landed in the json_object branch produced no result.json.
//
// Reproduced against the live production daemon at 2026-08-16 16:17:53 with
// tool_count=0 against 4 messages carrying tool blocks.
func toolHistoryMessages() []Message {
	return []Message{
		{Role: "user", Content: "Run the test suite."},
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID:   "call_1",
			Type: "function",
			Function: FunctionCall{
				Name:      "run_shell",
				Arguments: `{"command":"go test ./..."}`,
			},
		}}},
		{Role: "tool", ToolCallID: "call_1", Content: "ok  vornik.io/vornik  1.2s"},
		{Role: "user", Content: "Do not call tools. Produce the final answer now."},
	}
}

func TestBuildConverseInput_ToolFreeRequestWithToolHistory_HasNoToolBlocks(t *testing.T) {
	p := &BedrockProvider{model: "zai.glm-5"}

	ctx := WithRequestResponseFormat(context.Background(), "json_object")
	input, err := p.buildConverseInput(ctx, toolHistoryMessages(), nil)
	if err != nil {
		t.Fatalf("buildConverseInput: %v", err)
	}

	// Either ToolConfig is supplied, or no tool blocks survive. We take the
	// latter: supplying tool definitions leaves ToolChoice at Bedrock's
	// default (auto), which would let the model call a tool on the one turn
	// whose entire purpose is to be tool-free.
	if input.ToolConfig != nil {
		t.Fatal("tool-free request must not reintroduce ToolConfig")
	}

	for i, m := range input.Messages {
		if len(m.Content) == 0 {
			t.Errorf("message %d has empty content — Bedrock rejects this", i)
		}
		for j, b := range m.Content {
			switch b.(type) {
			case *bedrocktypes.ContentBlockMemberToolUse:
				t.Errorf("message %d block %d is still a ToolUse block", i, j)
			case *bedrocktypes.ContentBlockMemberToolResult:
				t.Errorf("message %d block %d is still a ToolResult block", i, j)
			}
		}
	}

	// The exchange must survive as readable text, not be silently discarded —
	// the finalisation turn has to know what it ran and what came back.
	var all strings.Builder
	for _, m := range input.Messages {
		for _, b := range m.Content {
			if tb, ok := b.(*bedrocktypes.ContentBlockMemberText); ok {
				all.WriteString(tb.Value)
				all.WriteString("\n")
			}
		}
	}
	if !strings.Contains(all.String(), "run_shell") {
		t.Errorf("flattened history lost the tool name; got:\n%s", all.String())
	}
	if !strings.Contains(all.String(), "vornik.io/vornik") {
		t.Errorf("flattened history lost the tool result; got:\n%s", all.String())
	}
}

// The normal agentic path must be untouched: when tools ARE offered, the tool
// blocks stay and ToolConfig is attached.
func TestBuildConverseInput_WithTools_KeepsToolBlocks(t *testing.T) {
	p := &BedrockProvider{model: "zai.glm-5"}

	tools := []Tool{{
		Type: "function",
		Function: ToolFunction{
			Name:        "run_shell",
			Description: "run a shell command",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`),
		},
	}}

	input, err := p.buildConverseInput(context.Background(), toolHistoryMessages(), tools)
	if err != nil {
		t.Fatalf("buildConverseInput: %v", err)
	}
	if input.ToolConfig == nil {
		t.Fatal("tool-bearing request must carry ToolConfig")
	}

	var sawToolUse, sawToolResult bool
	for _, m := range input.Messages {
		for _, b := range m.Content {
			switch b.(type) {
			case *bedrocktypes.ContentBlockMemberToolUse:
				sawToolUse = true
			case *bedrocktypes.ContentBlockMemberToolResult:
				sawToolResult = true
			}
		}
	}
	if !sawToolUse || !sawToolResult {
		t.Errorf("tool blocks must be preserved when tools are offered (toolUse=%v toolResult=%v)",
			sawToolUse, sawToolResult)
	}
}

// An assistant turn carrying ONLY a tool call must not flatten to an empty
// content array — Bedrock rejects a message with no content blocks, which
// would trade one 400 for another.
func TestFlattenToolBlocks_NeverProducesEmptyContent(t *testing.T) {
	msgs := []bedrocktypes.Message{{
		Role: bedrocktypes.ConversationRoleAssistant,
		Content: []bedrocktypes.ContentBlock{
			&bedrocktypes.ContentBlockMemberToolUse{
				Value: bedrocktypes.ToolUseBlock{},
			},
		},
	}}

	out := flattenToolBlocks(msgs)
	if len(out) != 1 {
		t.Fatalf("message count changed: got %d want 1", len(out))
	}
	if len(out[0].Content) == 0 {
		t.Fatal("flattening produced an empty content array")
	}
	if _, ok := out[0].Content[0].(*bedrocktypes.ContentBlockMemberText); !ok {
		t.Fatalf("expected a text block, got %T", out[0].Content[0])
	}
}
