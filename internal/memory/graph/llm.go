package graph

import (
	"context"
	"strings"
	"time"

	"vornik.io/vornik/internal/chat"
)

// completeWithRetry calls Complete up to maxAttempts times with
// capped exponential backoff on transient gateway errors. Mirrors
// the helper in internal/hallucination — kept narrow and
// duplicated rather than exported so each consumer owns its own
// retry policy without cross-package coupling.
//
// Backoff schedule: 500ms, 2s, 8s. Permanent errors (4xx ≠ 429,
// auth failures) bail immediately; ctx cancellation interrupts
// the wait.
func completeWithRetry(ctx context.Context, client chat.Provider, msgs []chat.Message, maxAttempts int) (*chat.ChatResponse, error) {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	// Attribute every graph-pipeline LLM call (extractor, resolver, validator,
	// relationship) in the llm-call log; without this they log
	// call_site="unknown" (asked 2026-06-12). All callers live in package
	// graph, so a single label here covers the knowledge-graph pipeline.
	ctx = chat.WithCallSite(ctx, "memory.graph")
	// Bound runaway reasoning models: graph extraction emits small JSON
	// (entities/relations), but a small reasoning model (gpt-oss-20b) could
	// loop to its 16384-token cap with finish_reason=length and empty output,
	// burning ~80s per attempt (incident 2026-06-13). 8192 is generous for the
	// real output (observed legit completions ≤2604 tokens) while halving the
	// worst-case waste on any stage still on a small model.
	ctx = chat.WithRequestMaxTokens(ctx, 8192)
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := client.Complete(ctx, msgs)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, err
		}
		if attempt == maxAttempts {
			break
		}
		if !isRetryableLLMErr(err) {
			break
		}
		backoff := 500 * time.Millisecond
		for i := 1; i < attempt; i++ {
			backoff *= 4
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
	}
	return nil, lastErr
}

// isRetryableLLMErr classifies LLM errors. Typed gateway errors
// expose Retryable() (5xx + 429); connection-drop shapes are
// matched by message substring because the chat layer doesn't
// always wrap them as typed errors.
func isRetryableLLMErr(err error) bool {
	if err == nil {
		return false
	}
	if ge, ok := err.(*chat.GatewayError); ok {
		return ge.Retryable()
	}
	msg := err.Error()
	for _, hint := range []string{
		"unexpected EOF",
		"connection reset",
		"connection refused",
		"broken pipe",
		"i/o timeout",
		"context deadline exceeded",
		"RESOURCE_EXHAUSTED",
		"queue is full",
	} {
		if strings.Contains(msg, hint) {
			return true
		}
	}
	return false
}

// stripJSONFence pulls a JSON value out of a model response that
// may be wrapped in ```json fences or padded with prose. Returns
// the trimmed inner string. Tolerant by design — small models
// often emit fences even when the prompt forbids them.
func stripJSONFence(text string) string {
	t := strings.TrimSpace(text)
	t = strings.TrimPrefix(t, "```json")
	t = strings.TrimPrefix(t, "```")
	t = strings.TrimSuffix(t, "```")
	return strings.TrimSpace(t)
}

// pickModel applies a per-call model override when the provider
// supports ModelOverridable, otherwise returns the client unchanged
// so the provider's construction-time model wins.
func pickModel(client chat.Provider, model string) chat.Provider {
	if model == "" {
		return client
	}
	if mo, ok := client.(chat.ModelOverridable); ok {
		return mo.WithModel(model)
	}
	return client
}
