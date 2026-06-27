// Coverage for the prompt-shim helpers and a few small bedrock-convert
// branches that the existing test suite doesn't drive. Each fn is
// pure-data — no network, no subprocess — so the tests are cheap.

package chat

import (
	"encoding/json"
	"strings"
	"testing"
)

// renderToolCallsForHistory has three branches:
//   - empty slice → empty string
//   - exactly one call → unwrapped envelope
//   - many calls → JSON array of {name, arguments}

func TestRenderToolCallsForHistory_Empty(t *testing.T) {
	if got := renderToolCallsForHistory(nil); got != "" {
		t.Errorf("empty: got %q, want \"\"", got)
	}
}

func TestRenderToolCallsForHistory_Single(t *testing.T) {
	calls := []ToolCall{{
		ID:       "x",
		Type:     "function",
		Function: FunctionCall{Name: "search", Arguments: `{"q":"foo"}`},
	}}
	got := renderToolCallsForHistory(calls)
	if !strings.Contains(got, `"tool_call"`) {
		t.Errorf("single-call missing envelope: %q", got)
	}
	if !strings.Contains(got, `"search"`) {
		t.Errorf("single-call missing name: %q", got)
	}
	if !strings.Contains(got, `{"q":"foo"}`) {
		t.Errorf("single-call missing args: %q", got)
	}
}

func TestRenderToolCallsForHistory_Many(t *testing.T) {
	calls := []ToolCall{
		{Function: FunctionCall{Name: "a", Arguments: `{}`}},
		{Function: FunctionCall{Name: "b", Arguments: `{"k":1}`}},
	}
	got := renderToolCallsForHistory(calls)
	if !strings.HasPrefix(got, "[") || !strings.HasSuffix(got, "]") {
		t.Errorf("many: not wrapped in []: %q", got)
	}
	if !strings.Contains(got, `"a"`) || !strings.Contains(got, `"b"`) {
		t.Errorf("many: tool names missing: %q", got)
	}
}

func TestRenderToolCallsForHistory_EmptyArgsCoalescedToObj(t *testing.T) {
	// coalesceJSON behaviour — empty args → "{}".
	calls := []ToolCall{{Function: FunctionCall{Name: "f", Arguments: ""}}}
	got := renderToolCallsForHistory(calls)
	if !strings.Contains(got, `"arguments":{}`) {
		t.Errorf("empty args not coalesced to {}: %q", got)
	}
}

// flattenMessageText:
//   - Content set → returned verbatim
//   - Content empty + Blocks → text concatenation
//   - non-text blocks skipped

func TestFlattenMessageText_ContentPath(t *testing.T) {
	if got := flattenMessageText(Message{Content: "hi"}); got != "hi" {
		t.Errorf("Content path: got %q, want hi", got)
	}
}

func TestFlattenMessageText_BlocksPath(t *testing.T) {
	got := flattenMessageText(Message{Blocks: []ContentBlock{
		{Type: "text", Text: "a"},
		{Type: "image_url"},
		{Type: "text", Text: "b"},
	}})
	if got != "a\n\nb" {
		t.Errorf("Blocks path: got %q, want \"a\\n\\nb\"", got)
	}
}

func TestFlattenMessageText_AllNonText(t *testing.T) {
	got := flattenMessageText(Message{Blocks: []ContentBlock{
		{Type: "image_url"},
		{Type: "document_url"},
	}})
	if got != "" {
		t.Errorf("all-non-text: got %q, want empty", got)
	}
}

// appendJSONOnlySystemHint appends a system message to the slice and
// MUST not mutate the input slice (callers re-use it).
func TestAppendJSONOnlySystemHint_AppendsAndPreservesInput(t *testing.T) {
	in := []Message{{Role: "user", Content: "x"}}
	out := appendJSONOnlySystemHint(in)
	if len(out) != 2 || out[1].Role != "system" {
		t.Errorf("append shape wrong: %+v", out)
	}
	if !strings.Contains(out[1].Content, "JSON") {
		t.Errorf("hint missing JSON cue: %q", out[1].Content)
	}
	// Input slice unchanged.
	if len(in) != 1 {
		t.Errorf("input slice mutated: %+v", in)
	}
}

// codex packMessages — tests the helper directly, building a request
// from a small conversation history.
func TestCodexCLI_PackMessages_PromptCarriesLatestUser(t *testing.T) {
	c := NewCodexCLIClient("gpt-5.4-mini")
	req, err := c.packMessages([]Message{
		{Role: "system", Content: "be helpful"},
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "first answer"},
		{Role: "user", Content: "follow-up?"},
	}, nil)
	if err != nil {
		t.Fatalf("packMessages: %v", err)
	}
	if !strings.Contains(req.prompt, "follow-up?") {
		t.Errorf("prompt missing latest user msg; got %q", req.prompt)
	}
	if !strings.Contains(req.prompt, "be helpful") {
		t.Errorf("prompt missing system instruction; got %q", req.prompt)
	}
}

func TestCodexCLI_PackMessages_ToolsInjectedIntoPrompt(t *testing.T) {
	c := NewCodexCLIClient("gpt-5.4-mini")
	tools := []Tool{{Type: "function", Function: ToolFunction{Name: "ping"}}}
	req, err := c.packMessages([]Message{
		{Role: "system", Content: "be precise"},
		{Role: "user", Content: "hi"},
	}, tools)
	if err != nil {
		t.Fatalf("packMessages: %v", err)
	}
	if !strings.Contains(req.prompt, "ping") {
		t.Errorf("tool catalog missing; got %q", req.prompt)
	}
	if !strings.Contains(req.prompt, "tool_call") {
		t.Errorf("shim protocol missing; got %q", req.prompt)
	}
}

// codex packMessages with an empty message slice returns ErrEmptyMessages.
func TestCodexCLI_PackMessages_EmptyRejected(t *testing.T) {
	c := NewCodexCLIClient("gpt-5.4-mini")
	_, err := c.packMessages(nil, nil)
	if err == nil {
		t.Error("expected ErrEmptyMessages, got nil")
	}
}

// coalesceJSON: empty/whitespace → "{}", anything else passes through
// (including the literal "null" — the helper is a presence check, not
// a JSON value normaliser).
func TestCoalesceJSON_EmptyToObject(t *testing.T) {
	if got := coalesceJSON(""); got != "{}" {
		t.Errorf("empty: got %q, want {}", got)
	}
	if got := coalesceJSON("   "); got != "{}" {
		t.Errorf("whitespace: got %q, want {}", got)
	}
	if got := coalesceJSON(`{"k":1}`); got != `{"k":1}` {
		t.Errorf("passthrough: got %q", got)
	}
}

// Sanity test that buildJSONSchemaEnforcementTool emits a valid
// Tool with a parseable JSON schema body. Drives the synthetic
// schema-enforcement tool path the bedrock provider uses to coerce
// structured outputs.
func TestBuildJSONSchemaEnforcementTool_Wellformed(t *testing.T) {
	rfs := &ResponseJSONSchema{
		Name:   "AnswerSchema",
		Strict: true,
		Schema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}}}`),
	}
	tool, toolName, err := buildJSONSchemaEnforcementTool(rfs)
	if err != nil {
		t.Fatalf("buildJSONSchemaEnforcementTool: %v", err)
	}
	if tool.Type != "function" {
		t.Errorf("Type: got %q, want function", tool.Type)
	}
	if toolName == "" {
		t.Error("toolName: got empty, want non-empty")
	}
	// Ensure schema round-trips cleanly through JSON.
	var got map[string]any
	if err := json.Unmarshal(tool.Function.Parameters, &got); err != nil {
		t.Fatalf("tool.Parameters not JSON: %v", err)
	}
}
