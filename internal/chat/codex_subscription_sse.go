package chat

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// parseCodexResponsesSSE reads the Responses API SSE stream and
// accumulates a ChatResponse in our shape. The event types we care
// about are:
//
//	response.created                    — ignored
//	response.output_item.added          — new item starts; we track
//	                                      type (message vs function_call)
//	response.output_text.delta          — chunk of assistant text
//	response.output_text.done           — end of a text item
//	response.function_call_arguments.delta — chunk of tool-call args
//	response.function_call_arguments.done  — end of a tool call
//	response.completed                  — final event; carries usage
//	response.error                      — terminal error; surface verbatim
//
// onText (when non-nil) receives the accumulated text as it arrives
// so streaming callers can render incrementally. Tool-call arguments
// are NOT streamed to onText — they're parsed once the arguments.done
// event fires, matching the existing HTTP-client streaming contract.
func parseCodexResponsesSSE(r io.Reader, onText StreamCallback) (*ChatResponse, error) {
	scanner := bufio.NewScanner(r)
	// The `instructions` field alone can be tens of KB; bump the
	// per-line buffer high enough to swallow any single SSE event.
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)

	resp := &ChatResponse{}
	var (
		textBuf     strings.Builder
		currentItem struct {
			kind    string // "message" | "function_call"
			callID  string
			name    string
			argsBuf strings.Builder
		}
		inflightCalls []ToolCall
	)

	// SSE frames are "event: <type>\ndata: <json>\n\n". Some proxies
	// collapse to data-only; we handle both. An event is terminated
	// by a blank line.
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

		// event=[DONE] terminator is sometimes sent for backward
		// compat with Chat Completions clients. Treat as no-op.
		if strings.TrimSpace(dataJSON) == "[DONE]" {
			return nil
		}
		// Some frames carry the event type inside the JSON payload
		// under "type" — use it when the "event:" header was absent.
		var envelope struct {
			Type     string          `json:"type"`
			Delta    string          `json:"delta,omitempty"`
			Text     string          `json:"text,omitempty"`
			Item     *codexSSEItem   `json:"item,omitempty"`
			Response json.RawMessage `json:"response,omitempty"`
			Error    *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error,omitempty"`
		}
		if err := json.Unmarshal([]byte(dataJSON), &envelope); err != nil {
			// Malformed / non-JSON — skip silently. Matches the
			// HTTP client's resilience to garbled chunks.
			return nil
		}
		if event == "" {
			event = envelope.Type
		}

		switch event {
		case "response.output_item.added":
			if envelope.Item != nil {
				currentItem.kind = envelope.Item.Type
				currentItem.callID = envelope.Item.CallID
				currentItem.name = envelope.Item.Name
				currentItem.argsBuf.Reset()
			}

		case "response.output_text.delta":
			if envelope.Delta != "" {
				textBuf.WriteString(envelope.Delta)
				if onText != nil {
					onText(textBuf.String())
				}
			}

		case "response.output_text.done":
			// Nothing to do — deltas already captured the full text.

		case "response.function_call_arguments.delta":
			if envelope.Delta != "" {
				currentItem.argsBuf.WriteString(envelope.Delta)
			}

		case "response.function_call_arguments.done":
			if currentItem.kind == "function_call" && currentItem.callID != "" {
				inflightCalls = append(inflightCalls, ToolCall{
					ID:   currentItem.callID,
					Type: "function",
					Function: FunctionCall{
						Name:      currentItem.name,
						Arguments: currentItem.argsBuf.String(),
					},
				})
			}
			currentItem.kind = ""
			currentItem.callID = ""
			currentItem.name = ""
			currentItem.argsBuf.Reset()

		case "response.completed":
			// The completed event carries the final response object
			// including usage. Re-parse to extract token counts.
			if len(envelope.Response) > 0 {
				var final struct {
					Usage *struct {
						InputTokens  int `json:"input_tokens"`
						OutputTokens int `json:"output_tokens"`
						TotalTokens  int `json:"total_tokens"`
					} `json:"usage"`
					ID string `json:"id"`
				}
				if err := json.Unmarshal(envelope.Response, &final); err == nil {
					if final.Usage != nil {
						resp.Usage.PromptTokens = final.Usage.InputTokens
						resp.Usage.CompletionTokens = final.Usage.OutputTokens
						resp.Usage.TotalTokens = final.Usage.TotalTokens
						if resp.Usage.TotalTokens == 0 {
							resp.Usage.TotalTokens = resp.Usage.PromptTokens + resp.Usage.CompletionTokens
						}
					}
					if final.ID != "" && resp.ID == "" {
						resp.ID = final.ID
					}
				}
			}

		case "response.error":
			if envelope.Error != nil {
				return fmt.Errorf("responses api error %s: %s",
					envelope.Error.Code, envelope.Error.Message)
			}
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
			// SSE comment — heartbeat, skip.
			continue
		}
		if strings.HasPrefix(line, "event:") {
			currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimPrefix(line, "data:"))
			// Convention: a leading space after "data:" is not data.
			if i := len(dataLines) - 1; len(dataLines[i]) > 0 && dataLines[i][0] == ' ' {
				dataLines[i] = dataLines[i][1:]
			}
			continue
		}
		// Unknown SSE field — ignore (per spec).
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return nil, fmt.Errorf("sse read: %w", err)
	}
	// Drain any unterminated final frame.
	if err := flush(); err != nil {
		return nil, err
	}

	// Assemble the single-choice response in our shape. The HTTP
	// client and claude-cli client both use this exact one-choice
	// wrapper, so downstream code is happy.
	msg := Message{Role: "assistant", Content: textBuf.String(), ToolCalls: inflightCalls}
	resp.Choices = []struct {
		Index        int     `json:"index"`
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	}{
		{Index: 0, Message: msg, FinishReason: chooseFinishReason(len(inflightCalls) > 0)},
	}
	return resp, nil
}

// codexSSEItem matches the `item` subobject on output_item.added.
// We only need a few fields; unknown ones parse away harmlessly.
type codexSSEItem struct {
	Type   string `json:"type"`    // "message" | "function_call"
	CallID string `json:"call_id"` // present on function_call
	Name   string `json:"name"`    // present on function_call
}

func chooseFinishReason(hasToolCalls bool) string {
	if hasToolCalls {
		return "tool_calls"
	}
	return "stop"
}
