package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// claudeContentBlock is the local alias for an Anthropic content block.
// Hoisted to package scope so package-level helpers (the multimodal
// converter, in particular) can return it without redeclaring the
// alias inside buildRequestBody.
type claudeContentBlock = map[string]any

// convertOpenAIBlocksToAnthropic translates our OpenAI-shaped multimodal
// content blocks into Anthropic's content block format. The two key
// differences:
//
//   - text:   identical shape on both sides ({type:"text", text}).
//   - image:  OpenAI uses {type:"image_url", image_url:{url}}; Anthropic
//     uses {type:"image", source:{type, media_type, data}|{type:"url", url}}.
//
// Inline data URLs ("data:image/jpeg;base64,...") are parsed into the
// {type:"base64", media_type, data} source shape. Remote URLs become
// {type:"url", url}. Malformed data URLs return a descriptive error
// rather than passing junk through to the API.
func convertOpenAIBlocksToAnthropic(blocks []ContentBlock) ([]claudeContentBlock, error) {
	out := make([]claudeContentBlock, 0, len(blocks))
	for i, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text == "" {
				continue
			}
			out = append(out, claudeContentBlock{"type": "text", "text": b.Text})
		case "image_url":
			if b.ImageURL == nil || b.ImageURL.URL == "" {
				return nil, fmt.Errorf("block %d: image_url block missing url", i)
			}
			source, err := imageURLToAnthropicSource(b.ImageURL.URL)
			if err != nil {
				return nil, fmt.Errorf("block %d: %w", i, err)
			}
			out = append(out, claudeContentBlock{"type": "image", "source": source})
		default:
			return nil, fmt.Errorf("block %d: unsupported content block type %q", i, b.Type)
		}
	}
	return out, nil
}

// imageURLToAnthropicSource splits a data URL into Anthropic's
// {type, media_type, data} triple, or wraps a plain http(s) URL into
// {type:"url", url}. Anything else is rejected — a file:// URL, for
// instance, would leak host paths to the upstream and almost certainly
// reflects a caller bug.
func imageURLToAnthropicSource(url string) (map[string]any, error) {
	const dataPrefix = "data:"
	if strings.HasPrefix(url, dataPrefix) {
		// data:<mediatype>;base64,<data>
		rest := url[len(dataPrefix):]
		comma := strings.Index(rest, ",")
		if comma < 0 {
			return nil, fmt.Errorf("malformed data URL (missing comma)")
		}
		header, data := rest[:comma], rest[comma+1:]
		// header is "<mediatype>[;base64]" — Anthropic only accepts base64.
		parts := strings.Split(header, ";")
		if len(parts) < 2 || parts[len(parts)-1] != "base64" {
			return nil, fmt.Errorf("data URL must be base64-encoded")
		}
		mediaType := parts[0]
		if mediaType == "" {
			return nil, fmt.Errorf("data URL missing media type")
		}
		return map[string]any{
			"type":       "base64",
			"media_type": mediaType,
			"data":       data,
		}, nil
	}
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return map[string]any{"type": "url", "url": url}, nil
	}
	preview := url
	if len(preview) > 32 {
		preview = preview[:32] + "..."
	}
	return nil, fmt.Errorf("image url must be data: or http(s):// (got %q)", preview)
}

// buildRequestBody is the context-free entry point retained for
// existing call sites that don't carry a Go context (older tests
// and helpers). It delegates to buildRequestBodyCtx with a
// background context so the json_schema enforcement codepath sees
// the no-schema branch and behaves identically to its pre-item-7
// shape.
func (c *ClaudeSubscriptionClient) buildRequestBody(messages []Message, tools []Tool, acct claudeAccountInfo, sessionID string) ([]byte, error) {
	return c.buildRequestBodyCtx(context.Background(), messages, tools, acct, sessionID)
}

// buildRequestBodyCtx converts our OpenAI-Chat-Completions shape
// into an Anthropic Messages API request body. Key shape differences
// from the Chat Completions / Responses format this package also
// speaks:
//
//   - system prompt → top-level `system` string (not a message role)
//   - user/assistant text → messages[].content[] content blocks, with
//     {type:"text", text} parts
//   - assistant tool calls → messages[].content[] parts of
//     {type:"tool_use", id, name, input}
//   - tool results → user-role messages[] with content[] containing
//     {type:"tool_result", tool_use_id, content}
//   - tools[].function.{name,description,parameters} → top-level
//     tools[] items with {name, description, input_schema}
//
// Anthropic requires strict role alternation (user → assistant →
// user → …). Consecutive same-role messages and orphan tool_result
// messages need collapsing or the server 400s. We handle that by
// emitting one message per role boundary, merging content blocks
// from sequential entries of the same role.
//
// acct + sessionID feed the metadata.user_id field the CLI also sends.
// Skipping this field triggers the abuse-filter 429s that look like
// rate-limit_error but return no reset headers — the server uses the
// stringified blob to bucket OAuth traffic against the subscription.
//
// Item 7 of https://docs.vornik.io: when ctx
// carries a json_schema response_format directive, register a
// synthetic emit_result tool whose input_schema IS the role's JSON
// Schema and force tool_choice to that tool. Anthropic's Messages
// API has no native response_format field; tool-use forcing is the
// strongest portable structured-output guarantee on this provider
// because Anthropic validates tool_use.input against the declared
// input_schema before returning the call. The response unwrap
// (callers reading Message.Content) is handled by the SSE parser,
// which surfaces the synthetic tool's input as text when finish
// reason is "tool_use" on the emit_result tool — see
// claude_subscription_sse.go for the unwrap rule.
func (c *ClaudeSubscriptionClient) buildRequestBodyCtx(ctx context.Context, messages []Message, tools []Tool, acct claudeAccountInfo, sessionID string) ([]byte, error) {
	type contentBlock = claudeContentBlock
	type msg struct {
		Role    string         `json:"role"`
		Content []contentBlock `json:"content"`
	}

	maxTokens := c.maxTokens
	if maxTokens <= 0 {
		maxTokens = claudeDefaultMaxTokens
	}

	body := map[string]any{
		"model":      c.model,
		"stream":     true,
		"max_tokens": maxTokens,
	}
	if c.thinkingBudget > 0 {
		body["thinking"] = map[string]any{
			"type":          "enabled",
			"budget_tokens": c.thinkingBudget,
		}
	}

	// metadata.user_id matches the CLI's JSON-stringified
	// {device_id, account_uuid, session_id} blob exactly. Field
	// order matches the CLI output — Go map iteration is random,
	// so we build via json.Marshal of an anonymous struct to pin
	// it. (Server doesn't care about order today, but matching
	// avoids a free regression if a future anti-abuse revision
	// starts caring.)
	userIDBlob, err := json.Marshal(struct {
		DeviceID    string `json:"device_id"`
		AccountUUID string `json:"account_uuid"`
		SessionID   string `json:"session_id"`
	}{
		DeviceID:    acct.UserID,
		AccountUUID: acct.AccountUUID,
		SessionID:   sessionID,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal metadata.user_id: %w", err)
	}
	body["metadata"] = map[string]any{"user_id": string(userIDBlob)}

	var systemParts []string
	var out []msg
	// appendRole merges content into the tail message when its role
	// matches, preserving Anthropic's alternation invariant when the
	// caller hands us e.g. two assistant-tagged entries in a row
	// (assistant text followed by a tool call is our most common case).
	appendRole := func(role string, blocks []contentBlock) {
		if len(blocks) == 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, msg{Role: role, Content: blocks})
	}

	for _, m := range messages {
		switch m.Role {
		case "system":
			// System messages must be text-only — Anthropic doesn't
			// accept images in the system field. Reject loudly rather
			// than silently dropping image input from a misrouted
			// message; the caller should put images on user turns.
			if len(m.Blocks) > 0 {
				for _, b := range m.Blocks {
					if b.Type == "image_url" {
						return nil, fmt.Errorf("claude subscription: image blocks are not allowed in system messages")
					}
				}
				if t := m.EffectiveText(); t != "" {
					systemParts = append(systemParts, t)
				}
			} else if m.Content != "" {
				systemParts = append(systemParts, m.Content)
			}
		case "user":
			if len(m.Blocks) > 0 {
				blocks, err := convertOpenAIBlocksToAnthropic(m.Blocks)
				if err != nil {
					return nil, fmt.Errorf("claude subscription: user message: %w", err)
				}
				if len(blocks) > 0 {
					appendRole("user", blocks)
				}
			} else if m.Content != "" {
				appendRole("user", []contentBlock{{"type": "text", "text": m.Content}})
			}
		case "assistant":
			var blocks []contentBlock
			if len(m.Blocks) > 0 {
				converted, err := convertOpenAIBlocksToAnthropic(m.Blocks)
				if err != nil {
					return nil, fmt.Errorf("claude subscription: assistant message: %w", err)
				}
				blocks = append(blocks, converted...)
			} else if m.Content != "" {
				blocks = append(blocks, contentBlock{"type": "text", "text": m.Content})
			}
			for _, tc := range m.ToolCalls {
				// Arguments arrive as a JSON-encoded string; Anthropic
				// wants the parsed object under `input`. A blank or
				// "null" string means no-arg tool — pass `{}`.
				var input any
				argStr := tc.Function.Arguments
				if argStr == "" || argStr == "null" {
					input = map[string]any{}
				} else if err := json.Unmarshal([]byte(argStr), &input); err != nil {
					return nil, fmt.Errorf("tool_call %q: arguments are not valid JSON: %w",
						tc.Function.Name, err)
				}
				blocks = append(blocks, contentBlock{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Function.Name,
					"input": input,
				})
			}
			appendRole("assistant", blocks)
		case "tool":
			// Tool results are user-role messages in Messages API.
			// Content can be a plain string or a list of content parts;
			// we pass string through verbatim (the tool layer already
			// formatted it). tool_use_id links back to the assistant's
			// tool_use id — both sides must agree or the server errors.
			appendRole("user", []contentBlock{{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     m.Content,
			}})
		default:
			return nil, fmt.Errorf("claude subscription: unsupported message role %q", m.Role)
		}
	}

	// system is sent as an array of text blocks, with the Claude Code
	// identity string as the mandatory first block. The abuse filter
	// at Anthropic's edge checks this prefix when the token is an
	// OAuth bearer (sk-ant-oat*) — omitting it produces the terse
	// 429 "rate_limit_error" with no retry-after headers, even when
	// the subscription has plenty of quota. Confirmed via reverse-
	// engineering of claude-cli 2.1.114 and corroborated by three
	// independent cloaking-proxy projects (Claude-Cloak,
	// claude-code-proxy, openclaw-billing-proxy).
	sysBlocks := []map[string]any{
		{"type": "text", "text": claudeIdentitySystemPrompt},
	}
	for _, p := range systemParts {
		sysBlocks = append(sysBlocks, map[string]any{"type": "text", "text": p})
	}
	// Phase C — Anthropic prompt-prefix cache (cache_control).
	// When the request stamped a CacheStrategy in ctx and the
	// resolved-cache decision says "cache the system block", mark
	// the LAST system block with {"cache_control":{"type":
	// "ephemeral"}}. Anthropic caches everything from the start of
	// the system field up to (and including) the marked block —
	// 5-minute TTL by default. Subsequent calls with byte-identical
	// system bytes serve from cache, billed at 10% of the input
	// rate. The identity prelude + operator system parts are all
	// stable per-role, so the entire system block is cache-eligible.
	if strategy := CacheStrategyFromContext(ctx); shouldCacheSystemFromInput(messages, strategy) && len(sysBlocks) > 0 {
		sysBlocks[len(sysBlocks)-1]["cache_control"] = map[string]any{"type": "ephemeral"}
	}
	body["system"] = sysBlocks
	body["messages"] = out

	// Item 7 of https://docs.vornik.io —
	// json_schema enforcement via synthetic emit_result tool.
	// When ctx carries a typed response_format with a schema body,
	// append a synthetic tool whose input_schema IS the role's
	// schema and force tool_choice to that tool. Anthropic
	// validates tool_use.input against the declared input_schema
	// before returning the call to the client, so a malformed
	// response shape literally cannot reach us. The synthetic
	// tool is appended (not replaced) so any caller-supplied tools
	// (file_read / file_write / run_shell / memory_search) remain
	// available to the model on intermediate turns; the forcing
	// only constrains the FINAL turn that produces the result
	// JSON.
	//
	// Pre-fix this entire branch was missing: every Anthropic
	// call with a role outputSchema went out as free-form text
	// with no provider-side enforcement and the post-validator
	// caught shape violations only after the model had already
	// emitted prose.
	var emitResultName string
	rfStruct := ResponseFormatStructFromContext(ctx)
	if rfStruct != nil && rfStruct.Type == "json_schema" && rfStruct.JSONSchema != nil && len(rfStruct.JSONSchema.Schema) > 0 {
		emitResultName = strings.TrimSpace(rfStruct.JSONSchema.Name)
		if emitResultName == "" {
			emitResultName = "emit_result"
		}
	}

	// Anthropic's tool shape drops the Chat Completions `function`
	// wrapper. name/description/parameters become top-level on the
	// tool object, with `parameters` renamed to `input_schema`.
	convertedTools := make([]map[string]any, 0, len(tools)+1)
	for _, t := range tools {
		params := t.Function.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		convertedTools = append(convertedTools, map[string]any{
			"name":         t.Function.Name,
			"description":  t.Function.Description,
			"input_schema": params,
		})
	}
	if emitResultName != "" {
		desc := rfStruct.JSONSchema.Description
		if desc == "" {
			desc = "Emit your final answer by calling this tool. The arguments object IS your response and is validated against the schema."
		}
		convertedTools = append(convertedTools, map[string]any{
			"name":         emitResultName,
			"description":  desc,
			"input_schema": json.RawMessage(rfStruct.JSONSchema.Schema),
		})
		// Force tool_choice to the synthetic tool. The model MUST
		// call emit_result on its final turn; intermediate tool
		// calls (file_read etc.) are still permitted because
		// tool_choice=specific tool doesn't preclude the model
		// from calling other tools on preceding turns — it just
		// pins where the model lands on the answer-producing turn.
		body["tool_choice"] = map[string]any{
			"type": "tool",
			"name": emitResultName,
		}
	}
	if len(convertedTools) > 0 {
		body["tools"] = convertedTools
	}

	return json.Marshal(body)
}

// syntheticEmitResultName returns the tool name buildRequestBodyCtx
// registered for the json_schema enforcement path, or "" when no
// json_schema directive is on ctx (no unwrap needed). Centralised
// so the request builder and the response unwrapper agree on which
// tool to look for — drifting names would surface as a silent
// pass-through of the model's tool_call to the agent harness's
// dispatcher, which would then 404 on the unknown tool.
func syntheticEmitResultName(ctx context.Context) string {
	rf := ResponseFormatStructFromContext(ctx)
	if rf == nil || rf.Type != "json_schema" || rf.JSONSchema == nil {
		return ""
	}
	name := strings.TrimSpace(rf.JSONSchema.Name)
	if name == "" {
		name = "emit_result"
	}
	return name
}

// unwrapEmitResultToolCall rewrites the response in-place when the
// model emitted exactly one tool_call matching the registered
// synthetic emit_result tool. The tool's arguments — already
// validated by Anthropic against the declared input_schema — become
// the visible Message.Content, ToolCalls is cleared, and
// FinishReason flips from "tool_calls" to "stop". The agent
// harness then reads Content as a plain JSON reply and the
// downstream gates (validateRequiredOutputKeys, plausibility) run
// on it unchanged.
//
// Mutates the response only when the unwrap is unambiguous: the
// response must have a single Choice whose Message has a single
// ToolCall named `name`. Multi-tool-call responses (the model
// called file_read mid-turn before emitting the final tool_call)
// are NOT unwrapped — the agent harness's tool dispatch loop runs
// next iteration and the emit_result call lands on a subsequent
// turn after the file_read result. The unwrap path then fires on
// that later turn.
//
// Returns true when an unwrap happened (for test assertions and
// debug logging). No-op when name is "", out is nil, or the
// response doesn't match the unwrap-eligible shape.
func unwrapEmitResultToolCall(out *ChatResponse, name string) bool {
	if out == nil || name == "" {
		return false
	}
	if len(out.Choices) != 1 {
		return false
	}
	choice := &out.Choices[0]
	if len(choice.Message.ToolCalls) != 1 {
		return false
	}
	tc := choice.Message.ToolCalls[0]
	if tc.Function.Name != name {
		return false
	}
	// Arguments arrive as a JSON-encoded string (the Function.Arguments
	// contract). Surface them verbatim as the assistant content so
	// the agent harness's jq-based result.json extractor parses the
	// reply as-is.
	args := tc.Function.Arguments
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	// Preserve any text the model emitted alongside the tool call
	// (rare with tool_choice=specific tool, but Anthropic's spec
	// allows the model to emit a text block before its tool_use).
	// Concatenating keeps that text reachable to the caller; the
	// jq parser tolerates a JSON object preceded by free-form text
	// only loosely, so we put the JSON first and the text after as
	// a fallback comment-style suffix that doesn't break parsing.
	if choice.Message.Content != "" {
		choice.Message.Content = args + "\n\n" + choice.Message.Content
	} else {
		choice.Message.Content = args
	}
	choice.Message.ToolCalls = nil
	choice.FinishReason = "stop"
	return true
}
