package graph

import (
	"context"
	"errors"
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
// graphRequestMaxTokens bounds every graph-pipeline completion. It is a
// BACKSTOP against a runaway model, not a tuning knob: if calls are hitting
// it, the fix is almost certainly reasoning effort (see completeWithRetry),
// because raising this only makes each failure cost more.
const graphRequestMaxTokens = 8192

// ErrTruncatedCompletion reports a completion the provider cut off at the
// token ceiling (finish_reason=length).
//
// It exists because the alternative is silence. A reasoning model that spends
// its whole budget thinking returns finish_reason=length with EMPTY content,
// and an empty completion parses as a successful extraction of zero entities
// — indistinguishable, downstream, from "this text genuinely names nobody".
// The chunk is then marked extracted and the loss is permanent: measured
// 2026-07-31, 83.3% of extractions were being laundered this way, and the
// design's "failed parse → retry → re-flag" self-healing never fired because
// nothing ever failed to parse.
//
// Returned by completeWithRetry for ALL four graph stages (extractor,
// resolver, validator, relationship), because all four share this budget and
// all four have the same blind spot.
var ErrTruncatedCompletion = errors.New("graph: completion truncated at the token ceiling (finish_reason=length)")

// truncated reports whether a response was cut off at the token ceiling.
// Checked on the FIRST choice only — the graph stages send n=1.
func truncated(resp *chat.ChatResponse) bool {
	if resp == nil || len(resp.Choices) == 0 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(resp.Choices[0].FinishReason), "length")
}

func completeWithRetry(ctx context.Context, client chat.Provider, msgs []chat.Message, maxAttempts int) (*chat.ChatResponse, error) {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	// Attribute every graph-pipeline LLM call (extractor, resolver, validator,
	// relationship) in the llm-call log; without this they log
	// call_site="unknown" (asked 2026-06-12). All callers live in package
	// graph, so a single label here covers the knowledge-graph pipeline.
	ctx = chat.WithCallSite(ctx, "memory.graph")
	// Cap the REASONING, not just the output.
	//
	// The extractor was returning empty content on 83.3% of calls, with 1332
	// finish_reason=length against 978 stop over 24h. gpt-oss-120b is a
	// reasoning model and treats the token allowance as a budget for thinking
	// first and answering second, so it spent the whole thing deliberating
	// about a structured-extraction task that needs very little of it.
	//
	// Raising the ceiling was tried first (8192 → 16384, 2026-08-05) and made
	// it worse in the only way that matters: the very next live call consumed
	// all 16384 and still returned nothing, so each failure cost twice as much
	// while remaining just as likely. The ceiling was never the cause. Effort
	// is, and it is the lever with a matching shape — entity extraction is
	// recall over a closed vocabulary, not a problem that rewards deliberation.
	// Legitimate completions measured ≤2604 tokens.
	//
	// Applied to all four stages: they share this budget and every one of them
	// is extraction or classification rather than reasoning work. Providers
	// with no notion of reasoning effort ignore it.
	ctx = chat.WithReasoningEffort(ctx, chat.ReasoningEffortLow)
	// The ceiling stays as the BACKSTOP it was always meant to be. Back to
	// 8192: with reasoning capped, the extra headroom bought nothing except a
	// more expensive failure, and a bound that never binds is not a bound.
	ctx = chat.WithRequestMaxTokens(ctx, graphRequestMaxTokens)
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := client.Complete(ctx, msgs)
		if err == nil {
			// A truncated completion is a FAILURE, not a result. Reported
			// here so every stage sees it, and reported even when content
			// came back — partial JSON that happens to parse is worse than
			// none, because it looks like a complete answer.
			if truncated(resp) {
				return resp, ErrTruncatedCompletion
			}
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
