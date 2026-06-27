package chat

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCodexSubscription_BuildRequestBody walks the full message-role
// switch (system, user, assistant text, assistant tool calls, tool
// result), the image-block guard, and the tools encoding.
func TestCodexSubscription_BuildRequestBody(t *testing.T) {
	c := NewCodexSubscriptionClient("gpt-5.4-mini",
		WithCodexSubscriptionEffortLevel("medium"))

	msgs := []Message{
		{Role: "system", Content: "system 1"},
		{Role: "system", Content: "system 2"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "thinking...", ToolCalls: []ToolCall{
			{
				ID: "call-1",
				Function: FunctionCall{
					Name:      "search",
					Arguments: `{"q":"foo"}`,
				},
			},
		}},
		{Role: "tool", ToolCallID: "call-1", Content: "result-text"},
	}
	tools := []Tool{
		{Type: "function", Function: ToolFunction{
			Name:        "search",
			Description: "search docs",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		}},
		// Tool with empty parameters → defaults to {}.
		{Type: "function", Function: ToolFunction{Name: "noop"}},
	}

	body, err := c.buildRequestBody(msgs, tools)
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}

	// Parse a generic view to assert the shape.
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed["model"] != "gpt-5.4-mini" {
		t.Errorf("model = %v", parsed["model"])
	}
	if instr, _ := parsed["instructions"].(string); !strings.Contains(instr, "system 1") || !strings.Contains(instr, "system 2") {
		t.Errorf("instructions missing system prompts: %q", instr)
	}
	if reasoning, ok := parsed["reasoning"].(map[string]any); !ok || reasoning["effort"] != "medium" {
		t.Errorf("reasoning block missing or wrong: %+v", parsed["reasoning"])
	}
	inputs, _ := parsed["input"].([]any)
	if len(inputs) < 4 {
		t.Errorf("expected 4 input rows, got %d", len(inputs))
	}
	// First should be user.
	if user, ok := inputs[0].(map[string]any); !ok || user["role"] != "user" {
		t.Errorf("first input not user: %+v", inputs[0])
	}
	// One of them should be a function_call.
	hasFC := false
	for _, in := range inputs {
		if m, ok := in.(map[string]any); ok && m["type"] == "function_call" {
			hasFC = true
		}
	}
	if !hasFC {
		t.Error("expected at least one function_call input row")
	}

	// tools[] must be present.
	parsedTools, _ := parsed["tools"].([]any)
	if len(parsedTools) != 2 {
		t.Errorf("expected 2 tools, got %d", len(parsedTools))
	}
}

// TestCodexSubscription_BuildRequestBody_ImageBlockGuard rejects messages
// with image blocks since the Codex Responses API doesn't support them.
func TestCodexSubscription_BuildRequestBody_ImageBlockGuard(t *testing.T) {
	c := NewCodexSubscriptionClient("gpt-5.4-mini")
	msgs := []Message{
		{Role: "user", Blocks: []ContentBlock{
			{Type: "image_url", ImageURL: &ImageURLContent{URL: "data:image/png;base64,abc"}},
		}},
	}
	if _, err := c.buildRequestBody(msgs, nil); err == nil {
		t.Error("image block should be rejected")
	}
}

// TestCodexSubscription_BuildRequestBody_UnknownRole rejects roles
// outside the supported set.
func TestCodexSubscription_BuildRequestBody_UnknownRole(t *testing.T) {
	c := NewCodexSubscriptionClient("gpt-5.4-mini")
	msgs := []Message{{Role: "operator", Content: "hi"}}
	if _, err := c.buildRequestBody(msgs, nil); err == nil {
		t.Error("unknown role should error")
	}
}

// TestCodexSubscription_BuildRequestBody_NoTools_NoInstructions ensures
// the body still serialises when only a single user turn is provided.
func TestCodexSubscription_BuildRequestBody_NoTools_NoInstructions(t *testing.T) {
	c := NewCodexSubscriptionClient("gpt-5.4-mini")
	msgs := []Message{{Role: "user", Content: "hi"}}
	body, err := c.buildRequestBody(msgs, nil)
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := parsed["instructions"]; ok {
		t.Error("instructions should be absent without system prompt")
	}
	if _, ok := parsed["tools"]; ok {
		t.Error("tools should be absent")
	}
}
