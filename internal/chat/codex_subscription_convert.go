package chat

import (
	"encoding/json"
	"fmt"
)

// buildRequestBody converts our (OpenAI-Chat-Completions-shaped)
// Message + Tool arguments into the Responses API's input + tools
// format. Key differences:
//
//   - system prompt → top-level `instructions` (not a message)
//   - user/assistant text → items with type=message and content[]
//     each containing type=input_text|output_text
//   - tool calls → separate items with type=function_call
//     (name, arguments, call_id)
//   - tool results → items with type=function_call_output
//     (call_id, output)
//   - tools[].function.{name,description,parameters} → top-level
//     tools[] items with type=function and the same three fields
//     hoisted to the parent object
//
// We keep stream=true always: even for non-streaming callers we need
// the SSE event stream to extract usage + tool-call metadata that the
// Responses API only exposes between event types.
func (c *CodexSubscriptionClient) buildRequestBody(messages []Message, tools []Tool) ([]byte, error) {
	body := map[string]any{
		"model":               c.model,
		"stream":              true,
		"store":               false,
		"parallel_tool_calls": false,
		// reasoning.encrypted_content preserves the model's
		// reasoning across tool-call rounds without us having to
		// manage it — we pass it back as a content block. Matches
		// openclaw's default configuration.
		"include": []string{"reasoning.encrypted_content"},
	}
	if c.effortLevel != "" {
		body["reasoning"] = map[string]any{
			"effort":  c.effortLevel,
			"summary": "auto",
		}
	}

	// System prompt comes OUT of the message stream and into
	// top-level `instructions`. Multiple system messages are joined
	// with blank lines — matches how Claude/GPT Chat Completions
	// handle multi-system inputs in practice.
	var instructions []string
	input := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		// The Codex Responses API does not support image input on
		// any role. Reject loudly rather than silently dropping the
		// images; the dispatcher should route image-bearing requests
		// to a vision-capable provider (Vertex Gemini) instead.
		if hasImageBlock(m.Blocks) {
			return nil, fmt.Errorf("codex subscription: image content blocks are not supported by the Codex Responses API; route image-bearing requests to a vision-capable provider")
		}
		switch m.Role {
		case "system":
			if t := messageText(m); t != "" {
				instructions = append(instructions, t)
			}
		case "user":
			text := messageText(m)
			if text == "" {
				continue
			}
			input = append(input, map[string]any{
				"type": "message",
				"role": "user",
				"content": []map[string]any{
					{"type": "input_text", "text": text},
				},
			})
		case "assistant":
			// Assistant turns can carry either prose, tool calls,
			// or both. In our Message shape the text lives in
			// Content and tool invocations in ToolCalls.
			if text := messageText(m); text != "" {
				input = append(input, map[string]any{
					"type": "message",
					"role": "assistant",
					"content": []map[string]any{
						{"type": "output_text", "text": text},
					},
				})
			}
			for _, tc := range m.ToolCalls {
				// Arguments arrive as a json.RawMessage or a
				// plain string depending on upstream. Normalise
				// to a string — that's what the Responses API
				// wants.
				argStr := string(tc.Function.Arguments)
				if argStr == "" || argStr == "null" {
					argStr = "{}"
				}
				input = append(input, map[string]any{
					"type":      "function_call",
					"call_id":   tc.ID,
					"name":      tc.Function.Name,
					"arguments": argStr,
				})
			}
		case "tool":
			// Tool results reference the assistant's call_id and
			// carry the tool's output text. The Responses API
			// expects plain string output; JSON tool results
			// come through as-is (already serialised upstream).
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": m.ToolCallID,
				"output":  m.Content,
			})
		default:
			return nil, fmt.Errorf("codex subscription: unsupported message role %q", m.Role)
		}
	}
	if len(instructions) > 0 {
		body["instructions"] = joinInstructions(instructions)
	}
	body["input"] = input

	// Tool schema lives at top-level tools[] with type=function and
	// name/description/parameters hoisted out of an inner `function`
	// wrapper (which is the Chat Completions shape).
	if len(tools) > 0 {
		converted := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			params := t.Function.Parameters
			if len(params) == 0 {
				// The API rejects missing parameters — pass an
				// empty object schema for no-arg tools.
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			converted = append(converted, map[string]any{
				"type":        "function",
				"name":        t.Function.Name,
				"description": t.Function.Description,
				"parameters":  json.RawMessage(params),
				"strict":      false,
			})
		}
		body["tools"] = converted
		body["tool_choice"] = "auto"
	}

	return json.Marshal(body)
}

func joinInstructions(parts []string) string {
	out := parts[0]
	for _, p := range parts[1:] {
		out += "\n\n" + p
	}
	return out
}

// messageText pulls the plain text content from a message regardless of
// whether it arrived in the Content fast path or as a Blocks array of
// text-only entries. Images must be filtered upstream — see hasImageBlock.
func messageText(m Message) string {
	if len(m.Blocks) > 0 {
		return m.EffectiveText()
	}
	return m.Content
}

// hasImageBlock reports whether any block in the slice is an image_url
// (or an unknown non-text type). Used by Codex/text-only providers to
// reject multimodal payloads before they reach the wire.
func hasImageBlock(blocks []ContentBlock) bool {
	for _, b := range blocks {
		if b.Type != "" && b.Type != "text" {
			return true
		}
	}
	return false
}
