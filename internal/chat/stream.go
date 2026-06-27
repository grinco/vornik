package chat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// StreamCallback is invoked with the accumulated text content during streaming.
// Called periodically (not on every token) to avoid flooding the caller.
type StreamCallback func(accumulated string)

// streamChunk is one SSE data event from the OpenAI streaming API.
type streamChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Created int64  `json:"created"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string          `json:"role,omitempty"`
			Content   string          `json:"content,omitempty"`
			ToolCalls []toolCallDelta `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		// PromptTokensDetails surfaces OpenAI's cache-attribution
		// breakdown when the upstream supports it
		// (`prompt_tokens_details.cached_tokens`). Empty / nil for
		// providers that don't emit it.
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens,omitempty"`
		} `json:"prompt_tokens_details,omitempty"`
		// CacheCreationInputTokens / CacheReadInputTokens — the
		// Anthropic-style fields. Bedrock-access-gateway can also
		// emit these on /v1 routes.
		CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	} `json:"usage,omitempty"`
}

// toolCallDelta is an incremental tool call chunk.
type toolCallDelta struct {
	Index        int             `json:"index"`
	ID           string          `json:"id,omitempty"`
	Type         string          `json:"type,omitempty"`
	ExtraContent json.RawMessage `json:"extra_content,omitempty"`
	Function     struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

// CompleteWithToolsStream sends a streaming chat completion request.
// The onText callback is called periodically with accumulated content.
// Returns the complete ChatResponse when the stream finishes.
func (c *Client) CompleteWithToolsStream(ctx context.Context, messages []Message, tools []Tool, onText StreamCallback) (*ChatResponse, error) {
	if c.endpoint == "" {
		return nil, ErrEmptyEndpoint
	}
	if c.model == "" {
		return nil, ErrEmptyModel
	}
	if len(messages) == 0 {
		return nil, ErrEmptyMessages
	}

	type streamRequest struct {
		ChatRequest
		Stream bool `json:"stream"`
	}
	// MaxTokens / ResponseFormat must be carried — pre-fix this literal
	// copied only Model / Messages / Tools / Options, so any role with
	// chat.max_tokens configured got uncapped output and any role using
	// responseFormat: json_object had structured-output enforcement
	// silently bypassed when ProcessStreaming was the dispatch path.
	req := streamRequest{
		ChatRequest: ChatRequest{
			Model:          c.model,
			Messages:       messages,
			Tools:          tools,
			MaxTokens:      c.maxTokens,
			Options:        c.contextOptions(),
			ResponseFormat: ResponseFormatStructFromContext(ctx),
		},
		Stream: true,
	}
	c.prepareRequestForProvider(&req.ChatRequest)

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := c.endpoint + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.setAuthHeader(httpReq)

	// Streaming responses are long-lived. When configured, enforce the same
	// timeout semantics as non-streaming calls.
	streamTimeout := c.timeout
	if streamTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, streamTimeout)
		defer cancel()
		httpReq = httpReq.WithContext(ctx)
	}

	start := time.Now()
	c.logger.Debug().Str("url", url).Str("model", c.model).Dur("stream_timeout", streamTimeout).Msg("streaming chat completion started")

	// Use a client without http.Client.Timeout — the context timeout above
	// handles cancellation. http.Client.Timeout would race with the SSE stream.
	// Per-call client allocation is fine: the keep-alive pool lives in the
	// shared transport, so connections are still reused across stream calls
	// (a nil Transport would fall back to the under-pooled DefaultTransport).
	streamClient := &http.Client{Transport: sharedHTTPTransport()}
	resp, err := streamClient.Do(httpReq)
	if err != nil {
		c.logger.Warn().Err(err).Str("url", url).Dur("duration", time.Since(start)).Msg("streaming request failed")
		c.recordMetrics(start, "error", nil)
		return nil, fmt.Errorf("%w: %v", ErrRequestFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		// Non-streaming error — read the body as JSON
		respBody, _ := readLimited(resp.Body, 4096)
		c.logger.Warn().
			Int("status_code", resp.StatusCode).
			Int("resp_bytes", len(respBody)).
			Dur("duration", time.Since(start)).
			Str("body", string(respBody)).
			Msg("streaming request error")
		c.recordMetrics(start, fmt.Sprintf("http_%d", resp.StatusCode), nil)
		var apiErr struct {
			Error *Error `json:"error"`
		}
		msg := ""
		if json.Unmarshal(respBody, &apiErr) == nil && apiErr.Error != nil {
			msg = apiErr.Error.Message
		}
		return nil, &GatewayError{
			Status:  resp.StatusCode,
			Message: msg,
			Body:    truncateLogString(string(respBody), 512),
		}
	}

	// If the server returned a regular JSON response instead of SSE
	// (some endpoints ignore stream:true), fall back to parsing it directly.
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") && !strings.Contains(ct, "text/plain") {
		c.logger.Debug().Str("content_type", ct).Msg("streaming: server returned non-SSE response, falling back")
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxChatResponseBytes))
		if readErr != nil {
			return nil, fmt.Errorf("failed to read non-SSE response: %w", readErr)
		}
		var chatResp ChatResponse
		if err := json.Unmarshal(body, &chatResp); err != nil {
			return nil, fmt.Errorf("failed to parse non-SSE response: %w", err)
		}
		// Fire callback with the final content so the placeholder gets updated.
		if onText != nil && len(chatResp.Choices) > 0 && chatResp.Choices[0].Message.Content != "" {
			onText(chatResp.Choices[0].Message.Content)
		}
		c.recordMetrics(start, "success", &chatResp)
		return &chatResp, nil
	}

	// Parse SSE stream
	var content strings.Builder
	toolCalls := make(map[int]*ToolCall) // index → accumulated ToolCall
	var finishReason string
	var respID, respModel string
	var usage struct {
		PromptTokens        int
		CompletionTokens    int
		TotalTokens         int
		CacheCreationTokens int
		CacheReadTokens     int
	}

	lastFlush := time.Now()
	flushInterval := 800 * time.Millisecond

	scanner := bufio.NewScanner(resp.Body)
	// Default scanner cap is 64KB. A single SSE data: line for a
	// large tool-call argument (computer_use, structured JSON
	// outputs) routinely exceeds that and would silently truncate
	// the stream — the scanner stops at ErrTooLong and the rest of
	// the response is lost. Match the cap used by every other SSE
	// parser in this package (claude_subscription_sse.go, codex_*).
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		if respID == "" {
			respID = chunk.ID
			respModel = chunk.Model
		}
		if chunk.Usage != nil {
			usage.PromptTokens = chunk.Usage.PromptTokens
			usage.CompletionTokens = chunk.Usage.CompletionTokens
			usage.TotalTokens = chunk.Usage.TotalTokens
			// LLM-caching phase A: capture provider-native cache
			// tokens. Two encodings in the wild:
			//   - OpenAI: prompt_tokens_details.cached_tokens
			//   - Anthropic / bedrock-access-gateway:
			//     cache_creation_input_tokens + cache_read_input_tokens
			// We surface whichever the upstream emits; both being
			// zero is the normal case (sub-provider has no cache).
			if chunk.Usage.PromptTokensDetails != nil {
				usage.CacheReadTokens = chunk.Usage.PromptTokensDetails.CachedTokens
			}
			if chunk.Usage.CacheReadInputTokens > 0 {
				usage.CacheReadTokens = chunk.Usage.CacheReadInputTokens
			}
			if chunk.Usage.CacheCreationInputTokens > 0 {
				usage.CacheCreationTokens = chunk.Usage.CacheCreationInputTokens
			}
		}

		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				content.WriteString(choice.Delta.Content)
			}
			if choice.FinishReason != nil {
				finishReason = *choice.FinishReason
			}
			for _, tc := range choice.Delta.ToolCalls {
				existing, ok := toolCalls[tc.Index]
				if !ok {
					existing = &ToolCall{Type: "function"}
					toolCalls[tc.Index] = existing
				}
				if tc.ID != "" {
					existing.ID = tc.ID
				}
				if tc.Type != "" {
					existing.Type = tc.Type
				}
				if len(tc.ExtraContent) != 0 {
					existing.ExtraContent = append(existing.ExtraContent[:0], tc.ExtraContent...)
				}
				if tc.Function.Name != "" {
					existing.Function.Name = tc.Function.Name
				}
				existing.Function.Arguments += tc.Function.Arguments
			}
		}

		// Flush to callback periodically, not on every token.
		// Tool-call deltas intentionally stay silent here. Higher layers
		// that execute tools can render humanized progress text without
		// exposing raw function names such as "memory_correct" to users.
		if onText != nil && time.Since(lastFlush) >= flushInterval {
			if content.Len() > 0 {
				onText(content.String())
			}
			lastFlush = time.Now()
		}
	}

	// Final flush
	if onText != nil && content.Len() > 0 {
		onText(content.String())
	}

	c.logger.Debug().
		Str("url", url).
		Dur("duration", time.Since(start)).
		Int("content_len", content.Len()).
		Int("tool_calls", len(toolCalls)).
		Msg("streaming chat completion finished")

	// Build the assembled tool call slice. Iterate by sorted keys
	// rather than 0..len-1: Vertex/Gemini's OpenAI-compatible SSE
	// has been observed emitting a single tool_call delta with
	// `index: 1` on multi-turn conversations (HA + gemma-4 trace,
	// 2026-05-16) — the previous `for i := 0; i < len(map); i++`
	// loop silently dropped every entry whose index wasn't a dense
	// 0-based position and `done_reason: tool_calls` arrived without
	// the corresponding tool_calls slice. Sorting by index also
	// preserves the model's intended call order for parallel calls.
	keys := make([]int, 0, len(toolCalls))
	for k := range toolCalls {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	assembledToolCalls := make([]ToolCall, 0, len(toolCalls))
	for _, k := range keys {
		assembledToolCalls = append(assembledToolCalls, *toolCalls[k])
	}

	chatResp := &ChatResponse{
		ID:    respID,
		Model: respModel,
		Choices: []struct {
			Index        int     `json:"index"`
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		}{
			{
				Index: 0,
				Message: Message{
					Role:      "assistant",
					Content:   content.String(),
					ToolCalls: assembledToolCalls,
				},
				FinishReason: finishReason,
			},
		},
	}
	chatResp.Usage.PromptTokens = usage.PromptTokens
	chatResp.Usage.CompletionTokens = usage.CompletionTokens
	chatResp.Usage.TotalTokens = usage.TotalTokens
	chatResp.Usage.CacheCreationTokens = usage.CacheCreationTokens
	chatResp.Usage.CacheReadTokens = usage.CacheReadTokens

	c.recordMetrics(start, "success", chatResp)
	return chatResp, nil
}

func readLimited(r interface{ Read([]byte) (int, error) }, limit int) ([]byte, error) {
	buf := make([]byte, limit)
	n, err := r.Read(buf)
	return buf[:n], err
}
