// Package chat — CodexSubscriptionClient talks directly to OpenAI's
// Codex Responses API (https://chatgpt.com/backend-api/codex/responses)
// using the ChatGPT-subscription tokens written by `codex login`. This
// replaces the codex-cli subprocess provider and avoids its forced
// built-in tools: since we control the request body, only the tools
// we pass in the `tools` array are available to the model.
//
// Two wire-shape differences from /v1/chat/completions matter:
//   - request uses `input[]` + `instructions` instead of `messages[]`
//   - response is SSE with Responses-API event types (response.output_*)
//
// We translate both directions to keep the Provider interface stable.

package chat

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

const (
	codexResponsesURL  = "https://chatgpt.com/backend-api/codex/responses"
	codexOriginatorTag = "codex_cli_rs" // pose as the official CLI
	codexOpenAIBeta    = "responses=experimental"
)

// CodexSubscriptionClient implements chat.Provider by talking to the
// ChatGPT-subscription Responses API via the auth tokens the Codex
// CLI left in ~/.codex/auth.json. No subprocess, no codex exec —
// which means no built-in exec_command / file_read tools fighting
// our shim. The only tools the model sees are the ones we pass in
// Tool[], exactly like the HTTP (OpenAI-compatible) client.
type CodexSubscriptionClient struct {
	auth    *codexAuthManager
	http    *http.Client
	model   string
	logger  zerolog.Logger
	metrics *Metrics

	// counter for structured logging — identifies one subprocess
	// invocation end-to-end across log lines without leaking the
	// bearer token.
	counter atomic.Uint64

	// timeout bounds a single call end-to-end (connect + stream).
	// Defaults to DefaultTimeout (300s). Independent of the Go
	// context passed to Complete — whichever fires first wins.
	timeout time.Duration

	// effortLevel is the `reasoning.effort` field in the Responses
	// API request. "low" | "medium" | "high" | "" (default).
	// The equivalent of our claude-cli CLAUDE_CODE_EFFORT_LEVEL.
	effortLevel string

	// modelCatalog is the catalog ListModels returns. Wired by the
	// container (typically from pricing.yaml entries matching the
	// OpenAI naming convention). nil → ListModels returns an empty
	// list (the ChatGPT backend has no public /v1/models endpoint).
	modelCatalog []ModelInfo
}

// CodexSubscriptionOption configures a CodexSubscriptionClient.
type CodexSubscriptionOption func(*CodexSubscriptionClient)

// WithCodexSubscriptionAuthPath overrides the auth.json location.
// Empty string = ~/.codex/auth.json (the Codex CLI default).
func WithCodexSubscriptionAuthPath(p string) CodexSubscriptionOption {
	return func(c *CodexSubscriptionClient) { c.auth = newCodexAuthManager(p) }
}

// WithCodexSubscriptionLogger wires a zerolog for the provider.
func WithCodexSubscriptionLogger(l zerolog.Logger) CodexSubscriptionOption {
	return func(c *CodexSubscriptionClient) { c.logger = l }
}

// WithCodexSubscriptionTimeout sets the per-call timeout.
func WithCodexSubscriptionTimeout(d time.Duration) CodexSubscriptionOption {
	return func(c *CodexSubscriptionClient) { c.timeout = d }
}

// WithCodexSubscriptionEffortLevel sets reasoning.effort ("low"|"medium"|"high").
// Empty = don't send the field (let the model pick).
func WithCodexSubscriptionEffortLevel(e string) CodexSubscriptionOption {
	return func(c *CodexSubscriptionClient) { c.effortLevel = e }
}

// WithCodexSubscriptionModelCatalog supplies the catalog ListModels
// will return. The ChatGPT-subscription Codex/Responses surface has
// no public /v1/models endpoint, so the daemon provides the catalog
// out-of-band — typically by filtering pricing.yaml.
func WithCodexSubscriptionModelCatalog(models []ModelInfo) CodexSubscriptionOption {
	return func(c *CodexSubscriptionClient) { c.modelCatalog = models }
}

// NewCodexSubscriptionClient builds a provider. `model` defaults to
// gpt-5.4-mini when empty. auth.json is read lazily on the first
// Complete call, so misconfiguration surfaces with a clear error
// rather than failing at construction.
func NewCodexSubscriptionClient(model string, opts ...CodexSubscriptionOption) *CodexSubscriptionClient {
	if model == "" {
		model = "gpt-5.4-mini"
	}
	c := &CodexSubscriptionClient{
		auth:    newCodexAuthManager(""),
		http:    &http.Client{Timeout: 0, Transport: sharedHTTPTransport()}, // we enforce timeout via ctx below
		model:   model,
		logger:  zerolog.Nop(),
		timeout: DefaultTimeout,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Model implements Provider.
func (c *CodexSubscriptionClient) Model() string { return c.model }

// SetMetrics implements Provider. Also threads the metrics into the
// shared auth manager so token-refresh outcomes are observable.
func (c *CodexSubscriptionClient) SetMetrics(m *Metrics) {
	c.metrics = m
	if c.auth != nil {
		c.auth.setMetrics(m)
	}
}

// ListModels implements ModelLister. The chatgpt.com backend has no
// public list endpoint reachable via the plan-billed OAuth bearer,
// so the caller wires a catalog at construction time via
// WithCodexSubscriptionModelCatalog (typically derived from
// pricing.yaml). Returns nil when no catalog is configured.
func (c *CodexSubscriptionClient) ListModels(_ context.Context) ([]ModelInfo, error) {
	if len(c.modelCatalog) == 0 {
		return nil, nil
	}
	out := make([]ModelInfo, len(c.modelCatalog))
	copy(out, c.modelCatalog)
	return out, nil
}

// WithModel implements ModelOverridable. Returns a fresh client
// pinned to the requested model but sharing the parent's auth
// manager, http client, logger, and metrics. The atomic call
// counter resets on each clone — per-clone counters don't
// collide because logs already carry the model label.
func (c *CodexSubscriptionClient) WithModel(model string) Provider {
	if c == nil {
		return c
	}
	clone := &CodexSubscriptionClient{
		auth:        c.auth,
		http:        c.http,
		model:       model,
		logger:      c.logger,
		metrics:     c.metrics,
		timeout:     c.timeout,
		effortLevel: c.effortLevel,
	}
	return clone
}

// Ping implements Pinger. We call the auth manager's Token loader,
// which reads + parses the on-disk auth.json and surfaces malformed
// or missing-file failures. No HTTP round-trip; no token cost. A
// successful Ping means a real Complete won't fail at the auth-load
// step — it may still fail upstream, but the daemon's startup gate
// has confirmed the local pre-conditions.
func (c *CodexSubscriptionClient) Ping(ctx context.Context) error {
	if c == nil || c.auth == nil {
		return fmt.Errorf("codex subscription client not configured")
	}
	if _, _, err := c.auth.Token(ctx); err != nil {
		return fmt.Errorf("codex subscription auth: %w", err)
	}
	return nil
}

// Compile-time conformance.
var _ Provider = (*CodexSubscriptionClient)(nil)
var _ ModelOverridable = (*CodexSubscriptionClient)(nil)
var _ Pinger = (*CodexSubscriptionClient)(nil)

// Complete / CompleteWithTools / CompleteWithToolsStream all share the
// same request-building + SSE-parsing core. onText is only used by
// the streaming variant; the others pass nil.

func (c *CodexSubscriptionClient) Complete(ctx context.Context, messages []Message) (*ChatResponse, error) {
	return c.call(ctx, messages, nil, nil)
}

func (c *CodexSubscriptionClient) CompleteWithTools(ctx context.Context, messages []Message, tools []Tool) (*ChatResponse, error) {
	return c.call(ctx, messages, tools, nil)
}

func (c *CodexSubscriptionClient) CompleteWithToolsStream(ctx context.Context, messages []Message, tools []Tool, onText StreamCallback) (*ChatResponse, error) {
	return c.call(ctx, messages, tools, onText)
}

// call is the full round-trip: auth → build request → POST → parse SSE.
// Metrics + structured logging wrap it so every invocation surfaces
// under one call_id.
func (c *CodexSubscriptionClient) call(ctx context.Context, messages []Message, tools []Tool, onText StreamCallback) (*ChatResponse, error) {
	callID := c.counter.Add(1)
	start := time.Now()
	status := "error"
	defer func() { c.recordMetrics(start, status) }()

	// Per-call context deadline. Parent context deadline still wins
	// if it's tighter — no way around that under context semantics.
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	token, accountID, err := c.auth.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("codex subscription: %w", err)
	}

	body, err := c.buildRequestBody(messages, tools)
	if err != nil {
		return nil, fmt.Errorf("codex subscription: build request: %w", err)
	}

	c.logger.Debug().
		Uint64("call_id", callID).
		Str("model", c.model).
		Int("tool_count", len(tools)).
		Int("message_count", len(messages)).
		Int("body_bytes", len(body)).
		Msg("codex-subscription: invoking")

	buildReq := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexResponsesURL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("accept", "text/event-stream")
		req.Header.Set("authorization", "Bearer "+token)
		req.Header.Set("chatgpt-account-id", accountID)
		req.Header.Set("openai-beta", codexOpenAIBeta)
		req.Header.Set("originator", codexOriginatorTag)
		return req, nil
	}

	// 3 attempts total (the original call + 2 retries) covers the
	// 500s we've observed from OpenAI's backend without dragging
	// operator latency on a long outage. Backoff starts at 500ms.
	resp, err := retryableHTTPDo(ctx, c.http, buildReq, 3, 500*time.Millisecond, c.logger)
	if err != nil {
		if _, ok := err.(*retryableHTTPError); ok {
			return nil, fmt.Errorf("codex subscription: %w", err)
		}
		return nil, fmt.Errorf("codex subscription: POST: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		errBody, _ := readAllCapped(resp.Body, 4096)
		return nil, fmt.Errorf("codex subscription: HTTP %d: %s",
			resp.StatusCode, string(errBody))
	}

	out, err := parseCodexResponsesSSE(resp.Body, onText)
	if err != nil {
		return nil, fmt.Errorf("codex subscription: parse SSE: %w", err)
	}
	out.Model = c.model
	status = "ok"
	return out, nil
}

// recordMetrics mirrors the CLIClient metrics wiring — the metrics
// surface is shared so dashboards group by provider label.
func (c *CodexSubscriptionClient) recordMetrics(start time.Time, status string) {
	if c.metrics == nil {
		return
	}
	dur := time.Since(start).Seconds()
	c.metrics.RequestsTotal.WithLabelValues(c.model, status).Inc()
	c.metrics.RequestDuration.WithLabelValues(c.model).Observe(dur)
	if status != "ok" {
		c.metrics.ErrorsTotal.WithLabelValues(c.model, status).Inc()
	}
}
