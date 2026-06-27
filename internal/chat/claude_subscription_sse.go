package chat

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// parseClaudeMessagesSSE reads Anthropic's Messages API SSE stream and
// accumulates a ChatResponse in our OpenAI-shaped Choice wrapper. The
// event types we handle:
//
//	message_start          — {message:{id,model,usage:{input_tokens}}}
//	content_block_start    — {index, content_block:{type,id,name,input,text}}
//	                          type="text" | "tool_use" | "thinking"
//	content_block_delta    — {index, delta:{type,text,partial_json}}
//	                          delta.type="text_delta"|"input_json_delta"|
//	                                     "thinking_delta"|"signature_delta"
//	content_block_stop     — {index}
//	message_delta          — {delta:{stop_reason,stop_sequence}, usage:{output_tokens}}
//	message_stop           — terminator
//	ping                   — heartbeat, ignored
//	error                  — terminal; surface verbatim
//
// onText (when non-nil) is called on each accumulated text update.
// Tool-call arguments are NOT streamed to onText — they arrive as
// fragments of JSON and we don't want callers to see partial objects.
// They're parsed once the block closes.
//
// Thinking blocks are discarded — callers see only the visible
// assistant text. The dispatcher's existing tool-calling loop doesn't
// round-trip reasoning (no signature_delta preservation), which
// mirrors the cli-client behavior the HTTP provider replaces.
func parseClaudeMessagesSSE(r io.Reader, onText StreamCallback) (*ChatResponse, error) {
	scanner := bufio.NewScanner(r)
	// Anthropic SSE frames are usually small (<4KB), but a single
	// tool_use with a large input object can push past the default
	// 64KB scanner buffer. 16MB matches the codex provider.
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)

	resp := &ChatResponse{}
	var (
		textBuf       strings.Builder
		stopReason    string
		inflightCalls []ToolCall
	)

	// blocks tracks per-index state so we can reassemble content
	// across the start/delta*/stop sequence. Map rather than slice
	// because Anthropic can send indices non-contiguously in theory
	// (thinking blocks, future block types); the spec is indexed,
	// not positional.
	type blockState struct {
		kind    string // "text" | "tool_use" | "thinking" | ...
		callID  string // tool_use id
		name    string // tool_use name
		argsBuf strings.Builder
	}
	blocks := map[int]*blockState{}

	var currentEvent string
	var dataLines []string

	flush := func() error {
		if len(dataLines) == 0 {
			currentEvent = ""
			return nil
		}
		dataJSON := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		event := currentEvent
		currentEvent = ""

		var envelope struct {
			Type         string          `json:"type"`
			Index        int             `json:"index"`
			Message      json.RawMessage `json:"message,omitempty"`
			ContentBlock json.RawMessage `json:"content_block,omitempty"`
			Delta        json.RawMessage `json:"delta,omitempty"`
			Usage        *struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
			} `json:"usage,omitempty"`
			Error *struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error,omitempty"`
		}
		if err := json.Unmarshal([]byte(dataJSON), &envelope); err != nil {
			return nil // malformed frame — skip
		}
		if event == "" {
			event = envelope.Type
		}

		switch event {
		case "message_start":
			if len(envelope.Message) > 0 {
				var m struct {
					ID    string `json:"id"`
					Model string `json:"model"`
					Usage *struct {
						InputTokens              int `json:"input_tokens"`
						OutputTokens             int `json:"output_tokens"`
						CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
						CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
					} `json:"usage"`
				}
				if err := json.Unmarshal(envelope.Message, &m); err == nil {
					if resp.ID == "" {
						resp.ID = m.ID
					}
					if resp.Model == "" {
						resp.Model = m.Model
					}
					if m.Usage != nil {
						resp.Usage.PromptTokens = m.Usage.InputTokens
						resp.Usage.CompletionTokens = m.Usage.OutputTokens
						// LLM-caching phase A: surface
						// provider-native cache fields when
						// present. Anthropic emits these on the
						// message_start frame; we capture them
						// here for the final ChatResponse.
						resp.Usage.CacheCreationTokens = m.Usage.CacheCreationInputTokens
						resp.Usage.CacheReadTokens = m.Usage.CacheReadInputTokens
					}
				}
			}

		case "content_block_start":
			if len(envelope.ContentBlock) == 0 {
				return nil
			}
			var cb struct {
				Type string `json:"type"`
				ID   string `json:"id,omitempty"`
				Name string `json:"name,omitempty"`
				Text string `json:"text,omitempty"`
			}
			if err := json.Unmarshal(envelope.ContentBlock, &cb); err != nil {
				return nil
			}
			blocks[envelope.Index] = &blockState{
				kind:   cb.Type,
				callID: cb.ID,
				name:   cb.Name,
			}
			// text blocks may carry initial text on the start event
			// (rare, but spec-permitted).
			if cb.Type == "text" && cb.Text != "" {
				textBuf.WriteString(cb.Text)
				if onText != nil {
					onText(textBuf.String())
				}
			}

		case "content_block_delta":
			if len(envelope.Delta) == 0 {
				return nil
			}
			var d struct {
				Type        string `json:"type"`
				Text        string `json:"text,omitempty"`
				PartialJSON string `json:"partial_json,omitempty"`
			}
			if err := json.Unmarshal(envelope.Delta, &d); err != nil {
				return nil
			}
			state := blocks[envelope.Index]
			switch d.Type {
			case "text_delta":
				if d.Text != "" {
					textBuf.WriteString(d.Text)
					if onText != nil {
						onText(textBuf.String())
					}
				}
			case "input_json_delta":
				if state != nil && state.kind == "tool_use" {
					state.argsBuf.WriteString(d.PartialJSON)
				}
			case "thinking_delta", "signature_delta":
				// Discard — we don't surface reasoning text.
			}

		case "content_block_stop":
			state := blocks[envelope.Index]
			if state != nil && state.kind == "tool_use" && state.callID != "" {
				args := state.argsBuf.String()
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				inflightCalls = append(inflightCalls, ToolCall{
					ID:   state.callID,
					Type: "function",
					Function: FunctionCall{
						Name:      state.name,
						Arguments: args,
					},
				})
			}
			delete(blocks, envelope.Index)

		case "message_delta":
			// Carries stop_reason + output-token top-up; the final
			// output_tokens value here supersedes message_start.
			if len(envelope.Delta) > 0 {
				var d struct {
					StopReason string `json:"stop_reason,omitempty"`
				}
				if err := json.Unmarshal(envelope.Delta, &d); err == nil && d.StopReason != "" {
					stopReason = d.StopReason
				}
			}
			if envelope.Usage != nil && envelope.Usage.OutputTokens > 0 {
				resp.Usage.CompletionTokens = envelope.Usage.OutputTokens
			}

		case "message_stop":
			// Terminator — nothing to do; loop exits on EOF.

		case "ping":
			// Heartbeat — ignore.

		case "error":
			if envelope.Error != nil {
				return fmt.Errorf("anthropic api error %s: %s",
					envelope.Error.Type, envelope.Error.Message)
			}
			return fmt.Errorf("anthropic api error (unspecified)")
		}
		return nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return nil, err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // SSE comment
		}
		if strings.HasPrefix(line, "event:") {
			currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			d := strings.TrimPrefix(line, "data:")
			if len(d) > 0 && d[0] == ' ' {
				d = d[1:]
			}
			dataLines = append(dataLines, d)
			continue
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return nil, fmt.Errorf("sse read: %w", err)
	}
	if err := flush(); err != nil {
		return nil, err
	}

	resp.Usage.TotalTokens = resp.Usage.PromptTokens + resp.Usage.CompletionTokens

	// Map Anthropic's stop_reason onto our finish_reason. Known
	// Anthropic values: "end_turn", "max_tokens", "stop_sequence",
	// "tool_use". Anything else falls through as "stop" — the
	// dispatcher only reads "tool_calls" vs. everything-else.
	finish := "stop"
	switch stopReason {
	case "tool_use":
		finish = "tool_calls"
	case "max_tokens":
		finish = "length"
	}
	if len(inflightCalls) > 0 {
		finish = "tool_calls"
	}

	resp.Choices = []struct {
		Index        int     `json:"index"`
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	}{
		{
			Index: 0,
			Message: Message{
				Role:      "assistant",
				Content:   textBuf.String(),
				ToolCalls: inflightCalls,
			},
			FinishReason: finish,
		},
	}
	return resp, nil
}
