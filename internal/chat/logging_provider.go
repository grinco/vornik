package chat

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// maxLoggedBodyBytes caps each prompt/response body emitted on the
// DEBUG content event so a 256 KiB telemetry rollup can't flood the
// console. The architect's JSON reply — the payload that matters for
// the "confidence 0.00" diagnosis — is far under this, so it's never
// truncated in practice.
const maxLoggedBodyBytes = 16 * 1024

// ModelAggregator is implemented by providers that can return a
// per-sub-provider model breakdown: the *Router, the QueuedProvider
// that wraps it, and the LoggingProvider that wraps that. The
// /api/v1/models and ollama model-list handlers prefer it over the
// flat ModelLister so wrapping the provider chain in a decorator never
// silently collapses the breakdown to a single "chat" group.
type ModelAggregator interface {
	ModelLister
	ListModelsAggregated(ctx context.Context) (ListModelsResult, bool)
}

// LoggingProvider wraps a Provider and emits a structured log line for
// every completion call — call-site, model, message/prompt size,
// latency, token usage, finish reason, and error. It exists so EVERY
// LLM call the daemon makes (chat, the memetic architect, instinct
// distillation, judges, summarisation) is visible on the console
// without each caller having to log for itself.
//
// Two verbosity tiers:
//   - INFO: one "llm call" line per request with metadata + outcome.
//   - DEBUG: additionally the full prompt (before the call) and the
//     full response content (after). This is the raw material needed
//     to answer e.g. "is the architect even querying the LLM?" and
//     "did the model emit confidence: 0.0 / null, or omit it?".
//
// Build the provided logger at DEBUG level (the service layer flips it
// when VORNIK_LLM_LOG_CONTENT is set) to surface the bodies; otherwise
// content stays off and only the INFO metadata line is emitted.
//
// The decorator is transparent: it forwards every optional Provider
// capability (ModelOverridable, ModelLister, Pinger, and the
// QueuedProvider/Router model aggregation) to the wrapped provider, so
// wrapping never removes a feature. Mirrors QueuedProvider's
// forwarding discipline.
type LoggingProvider struct {
	inner Provider
	log   zerolog.Logger
}

// NewLoggingProvider wraps inner so every completion is logged. A nil
// inner returns nil (nothing to wrap) so callers stay nil-safe.
func NewLoggingProvider(inner Provider, log zerolog.Logger) Provider {
	if inner == nil {
		return nil
	}
	return &LoggingProvider{inner: inner, log: log}
}

func (p *LoggingProvider) Complete(ctx context.Context, messages []Message) (*ChatResponse, error) {
	p.logRequest(ctx, "complete", messages, 0)
	start := time.Now()
	resp, err := p.inner.Complete(ctx, messages)
	p.logResult(ctx, "complete", messages, start, resp, err)
	return resp, err
}

func (p *LoggingProvider) CompleteWithTools(ctx context.Context, messages []Message, tools []Tool) (*ChatResponse, error) {
	p.logRequest(ctx, "complete_tools", messages, len(tools))
	start := time.Now()
	resp, err := p.inner.CompleteWithTools(ctx, messages, tools)
	p.logResult(ctx, "complete_tools", messages, start, resp, err)
	return resp, err
}

func (p *LoggingProvider) CompleteWithToolsStream(ctx context.Context, messages []Message, tools []Tool, onText StreamCallback) (*ChatResponse, error) {
	p.logRequest(ctx, "complete_stream", messages, len(tools))
	start := time.Now()
	resp, err := p.inner.CompleteWithToolsStream(ctx, messages, tools, onText)
	p.logResult(ctx, "complete_stream", messages, start, resp, err)
	return resp, err
}

// logRequest emits the prompt body at DEBUG before the call so a
// prompt is visible even if the upstream backend hangs or the request
// is cancelled. No-op when DEBUG is disabled.
func (p *LoggingProvider) logRequest(ctx context.Context, op string, messages []Message, toolCount int) {
	if !p.log.Debug().Enabled() {
		return
	}
	p.log.Debug().
		Str("call_site", callSite(ctx)).
		Str("op", op).
		Str("model", p.inner.Model()).
		Int("tools", toolCount).
		Str("prompt", truncateBody(joinPrompt(messages))).
		Msg("llm request")
}

// logResult emits the INFO metadata line for every call, and — at
// DEBUG — the response body. Failures log at WARN with the error.
func (p *LoggingProvider) logResult(ctx context.Context, op string, messages []Message, start time.Time, resp *ChatResponse, err error) {
	site := callSite(ctx)
	latency := time.Since(start)

	if err != nil {
		// A cancelled CALLER context (context.Canceled) is expected
		// teardown — config reload, autonomy-loop restart, or daemon
		// shutdown tearing down the loop mid-call — NOT an LLM failure.
		// Log it at DEBUG so the console isn't polluted with a WARN that
		// reads to operators as "first automation eval → LLM error".
		// Every OTHER error (incl. context.DeadlineExceeded, a real
		// timeout) stays a WARN failure.
		if errors.Is(err, context.Canceled) {
			p.log.Debug().
				Str("call_site", site).
				Str("op", op).
				Str("model", p.inner.Model()).
				Int("messages", len(messages)).
				Int("prompt_bytes", promptBytes(messages)).
				Dur("latency", latency).
				Err(err).
				Msg("llm call cancelled (caller context done)")
			return
		}
		p.log.Warn().
			Str("call_site", site).
			Str("op", op).
			Str("model", p.inner.Model()).
			Int("messages", len(messages)).
			Int("prompt_bytes", promptBytes(messages)).
			Dur("latency", latency).
			Err(err).
			Msg("llm call failed")
		return
	}

	ev := p.log.Info().
		Str("call_site", site).
		Str("op", op).
		Str("model", p.inner.Model()).
		Int("messages", len(messages)).
		Int("prompt_bytes", promptBytes(messages)).
		Dur("latency", latency)
	if rf := ResponseFormatFromContext(ctx); rf != "" {
		ev = ev.Str("response_format", rf)
	}

	var content string
	if resp != nil {
		choices := len(resp.Choices)
		ev = ev.Int("choices", choices)
		if choices > 0 {
			content = resp.Choices[0].Message.Content
			ev = ev.
				Int("completion_bytes", len(content)).
				Str("finish_reason", resp.Choices[0].FinishReason)
		}
		ev = ev.
			Int("prompt_tokens", resp.Usage.PromptTokens).
			Int("completion_tokens", resp.Usage.CompletionTokens).
			Int("total_tokens", resp.Usage.TotalTokens)
	} else {
		ev = ev.Int("choices", 0)
	}
	ev.Msg("llm call")

	if resp != nil && p.log.Debug().Enabled() {
		p.log.Debug().
			Str("call_site", site).
			Str("op", op).
			Str("response", truncateBody(content)).
			Msg("llm response")
	}
}

func (p *LoggingProvider) Model() string         { return p.inner.Model() }
func (p *LoggingProvider) SetMetrics(m *Metrics) { p.inner.SetMetrics(m) }

// WithModel returns a logging wrapper around the inner provider's
// model-pinned clone, so per-request model overrides stay logged.
func (p *LoggingProvider) WithModel(model string) Provider {
	if o, ok := p.inner.(ModelOverridable); ok {
		return &LoggingProvider{inner: o.WithModel(model), log: p.log}
	}
	return p
}

// ListModels delegates to the wrapped provider when it supports
// discovery. Metadata calls aren't LLM completions, so they're not
// logged.
func (p *LoggingProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	if l, ok := p.inner.(ModelLister); ok {
		return l.ListModels(ctx)
	}
	return nil, nil
}

// Ping delegates to the wrapped provider's readiness probe.
func (p *LoggingProvider) Ping(ctx context.Context) error {
	if pg, ok := p.inner.(Pinger); ok {
		return pg.Ping(ctx)
	}
	return nil
}

// ListModelsAggregated forwards the per-sub-provider breakdown from
// the wrapped provider (QueuedProvider unwraps to *Router; a bare
// *Router answers directly) so wrapping the chain in logging doesn't
// collapse /api/v1/models to a flat list.
func (p *LoggingProvider) ListModelsAggregated(ctx context.Context) (ListModelsResult, bool) {
	if agg, ok := p.inner.(interface {
		ListModelsAggregated(context.Context) (ListModelsResult, bool)
	}); ok {
		return agg.ListModelsAggregated(ctx)
	}
	if r, ok := p.inner.(*Router); ok {
		return r.ListModels(ctx), true
	}
	return ListModelsResult{}, false
}

// callSite returns the call-site label or "unknown" when unset, so the
// metadata line always carries the field.
func callSite(ctx context.Context) string {
	if s := CallSiteFromContext(ctx); s != "" {
		return s
	}
	return "unknown"
}

func promptBytes(messages []Message) int {
	n := 0
	for _, m := range messages {
		n += len(m.Content)
		for _, b := range m.Blocks {
			n += len(b.Text)
		}
	}
	return n
}

func joinPrompt(messages []Message) string {
	var sb strings.Builder
	for i, m := range messages {
		if i > 0 {
			sb.WriteString("\n---\n")
		}
		sb.WriteString(m.Role)
		sb.WriteString(": ")
		sb.WriteString(m.Content)
	}
	return sb.String()
}

func truncateBody(s string) string {
	if len(s) <= maxLoggedBodyBytes {
		return s
	}
	return s[:maxLoggedBodyBytes] + "…(truncated " +
		strconv.Itoa(len(s)-maxLoggedBodyBytes) + " bytes)"
}

var _ Provider = (*LoggingProvider)(nil)
var _ ModelOverridable = (*LoggingProvider)(nil)
var _ ModelAggregator = (*LoggingProvider)(nil)
