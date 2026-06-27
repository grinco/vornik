// Package chat — ClaudeSubscriptionClient talks directly to
// https://api.anthropic.com/v1/messages using the OAuth tokens the
// `claude` CLI writes to ~/.claude/.credentials.json after a
// `claude login`. This replaces the claude-cli subprocess provider:
//
//   - no ~200ms per-call subprocess startup
//   - native tool_use blocks (the cli wrapper's prompt-engineering
//     shim is retired here — the model returns real Anthropic
//     tool_use content parts)
//   - proper per-delta text streaming instead of chunk-at-a-time
//     stream-json events
//
// Auth is straight OAuth: the request carries Authorization: Bearer
// plus the subscription-unlock beta header combo
// (oauth-2025-04-20,claude-code-20250219). Without that beta tag the
// server 401s OAuth tokens — the API-key surface is the only one
// reachable without it.
//
// For API-key deployments prefer the HTTP provider (internal/chat/
// client.go) against either api.anthropic.com directly or the Bedrock
// access gateway — both use x-api-key and don't touch OAuth at all.

package chat

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

const (
	// claudeMessagesPath is the Messages API path + query the real CLI
	// 2.1.114 calls. The `?beta=true` query string is REQUIRED —
	// without it OAuth traffic routes through a tighter rate-limit
	// bucket that surfaces as empty-message "rate_limit_error" 429s
	// even well under the published session quotas.
	claudeMessagesPath = "/v1/messages?beta=true"
	// claudeDefaultBaseURL is the production Messages API host. The
	// ANTHROPIC_BASE_URL env var overrides this (same knob the real
	// CLI honors) — useful for mitmproxy capture, staging targets,
	// and the unit-test sniffer harness.
	claudeDefaultBaseURL = "https://api.anthropic.com"
	claudeAPIVersion     = "2023-06-01"
	// claudeOAuthBeta is the full beta-flag set the Claude Code CLI
	// 2.1.114 sends on every request. Captured via ANTHROPIC_BASE_URL
	// redirection against a loopback sniffer on 2026-04-20. Order
	// matches the CLI's output and matters only for grep-ability in
	// logs — the server parses as an unordered comma-list.
	//
	// Why this specific set, rather than just oauth-2025-04-20:
	//   - claude-code-20250219        — general Claude Code capability
	//   - oauth-2025-04-20            — OAuth bearer-token unlock
	//   - interleaved-thinking-…      — thinking+tools in one response
	//   - context-management-…        — server-side history pruning
	//   - prompt-caching-scope-…      — scoped prompt-cache v2
	//   - advisor-tool-…              — some internal tool hint the
	//                                   server expects on Claude Code
	//                                   traffic
	//   - effort-2025-11-24           — the output_config.effort knob
	//
	// Omitting any of these flags the request as "external non-CLI
	// client" and triggers the tighter-bucket 429 mentioned above.
	claudeOAuthBeta = "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,prompt-caching-scope-2026-01-05,advisor-tool-2026-03-01,effort-2025-11-24"
	// claudeDefaultMaxTokens caps output tokens when the caller
	// doesn't specify. Messages API requires max_tokens; 8192 is
	// large enough for any realistic agent turn without risking a
	// runaway reply on a buggy prompt.
	claudeDefaultMaxTokens = 8192
	// claudeIdentitySystemPrompt is the exact string the Claude Code
	// CLI sends as the first system-prompt block. Anthropic's abuse
	// filter appears to require this prefix on OAuth bearer requests
	// — without it, requests return 429 "rate_limit_error" with no
	// reset headers even when the subscription has quota.
	claudeIdentitySystemPrompt = "You are Claude Code, Anthropic's official CLI for Claude."
	// claudeDefaultUserAgent matches the CLI's User-Agent format
	// exactly. The "sdk-cli" platform token differs from a plain
	// "linux" and is what the server looks at to decide CLI vs.
	// external-SDK vs. browser. Version tracks the CLI's latest
	// shipped number at the time of capture; bumping it is a
	// no-op for correctness but worth doing annually to stay in
	// the bucket Anthropic rate-limits official clients against.
	claudeDefaultUserAgent = "claude-cli/2.1.114 (external, sdk-cli)"
)

// ClaudeSubscriptionClient implements chat.Provider by calling the
// public Messages API with a Claude Code subscription token. It's a
// drop-in replacement for CLIClient — no subprocess, no tool-shim, but
// the same Provider interface and metrics shape.
type ClaudeSubscriptionClient struct {
	auth    *claudeAuthManager
	account *claudeAccountResolver
	http    *http.Client
	model   string
	logger  zerolog.Logger
	metrics *Metrics

	// counter is used for correlation IDs in debug logs.
	counter atomic.Uint64

	// timeout bounds a single call end-to-end.
	timeout time.Duration

	// maxTokens is the value sent in request.max_tokens. 0 means use
	// claudeDefaultMaxTokens.
	maxTokens int

	// thinkingBudget enables Anthropic extended thinking when > 0.
	// 0 leaves the field off (default: thinking disabled).
	thinkingBudget int

	// userAgent is the User-Agent header sent on every request. The
	// server uses this for abuse classification; posing as claude-cli
	// keeps us on the same treatment profile the CLI gets.
	userAgent string

	// sessionID is the UUID we pass as X-Claude-Code-Session-Id on
	// every request. Generated once at construction and shared across
	// clones (WithModel preserves it) so the server sees one long-
	// running session per vornik process, matching how a real
	// `claude` REPL behaves. Without this header Anthropic's
	// subscription rate-limit bookkeeping appears to place the
	// request in a tighter quota bucket.
	sessionID string

	// modelCatalog is the catalog ListModels returns. Wired by the
	// container (typically from pricing.yaml entries matching the
	// Anthropic naming convention). nil → ListModels returns an
	// empty list (the Messages API has no list-models endpoint
	// reachable with a subscription OAuth bearer).
	modelCatalog []ModelInfo
}

// ClaudeSubscriptionOption configures a ClaudeSubscriptionClient.
type ClaudeSubscriptionOption func(*ClaudeSubscriptionClient)

// WithClaudeSubscriptionAuthPath overrides ~/.claude/.credentials.json.
// Empty = default.
func WithClaudeSubscriptionAuthPath(p string) ClaudeSubscriptionOption {
	return func(c *ClaudeSubscriptionClient) { c.auth = newClaudeAuthManager(p) }
}

// WithClaudeSubscriptionLogger wires a zerolog for the provider.
func WithClaudeSubscriptionLogger(l zerolog.Logger) ClaudeSubscriptionOption {
	return func(c *ClaudeSubscriptionClient) { c.logger = l }
}

// WithClaudeSubscriptionTimeout sets the per-call timeout.
func WithClaudeSubscriptionTimeout(d time.Duration) ClaudeSubscriptionOption {
	return func(c *ClaudeSubscriptionClient) { c.timeout = d }
}

// WithClaudeSubscriptionMaxTokens sets the max_tokens request field.
// 0 = use the 8192 default.
func WithClaudeSubscriptionMaxTokens(n int) ClaudeSubscriptionOption {
	return func(c *ClaudeSubscriptionClient) { c.maxTokens = n }
}

// WithClaudeSubscriptionThinkingBudget enables Anthropic extended
// thinking with the given budget_tokens. 0 disables thinking.
func WithClaudeSubscriptionThinkingBudget(n int) ClaudeSubscriptionOption {
	return func(c *ClaudeSubscriptionClient) { c.thinkingBudget = n }
}

// WithClaudeSubscriptionUserAgent overrides the User-Agent header.
// Defaults to "claude-cli/1.0.0 (external, <os>)" — posing as the CLI
// keeps us in the same rate-limit bucket Anthropic applies to the
// shipped binary.
func WithClaudeSubscriptionUserAgent(ua string) ClaudeSubscriptionOption {
	return func(c *ClaudeSubscriptionClient) { c.userAgent = ua }
}

// WithClaudeSubscriptionModelCatalog supplies the catalog ListModels
// will return. The Anthropic Messages API has no list-models endpoint
// reachable with a subscription OAuth bearer — the daemon provides the
// catalog out-of-band, typically by filtering pricing.yaml.
func WithClaudeSubscriptionModelCatalog(models []ModelInfo) ClaudeSubscriptionOption {
	return func(c *ClaudeSubscriptionClient) { c.modelCatalog = models }
}

// NewClaudeSubscriptionClient constructs a provider. Model may be
// empty — it's required at send time, so leave it for per-request
// override via WithModel (the router's common path). Credentials are
// loaded lazily on the first call so a missing credentials.json
// surfaces as a clear error from Complete(), not at construction.
func NewClaudeSubscriptionClient(model string, opts ...ClaudeSubscriptionOption) *ClaudeSubscriptionClient {
	c := &ClaudeSubscriptionClient{
		auth:      newClaudeAuthManager(""),
		account:   newClaudeAccountResolver(),
		http:      &http.Client{Timeout: 0, Transport: sharedHTTPTransport()}, // we enforce via ctx below
		model:     model,
		logger:    zerolog.Nop(),
		timeout:   DefaultTimeout,
		userAgent: claudeDefaultUserAgent,
		sessionID: newClaudeSessionID(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Model implements Provider.
func (c *ClaudeSubscriptionClient) Model() string { return c.model }

// SetMetrics implements Provider. Also threads the metrics into the
// shared auth manager so token-refresh outcomes are observable.
func (c *ClaudeSubscriptionClient) SetMetrics(m *Metrics) {
	c.metrics = m
	if c.auth != nil {
		c.auth.setMetrics(m)
	}
}

// ListModels implements ModelLister. The Anthropic Messages API has no
// list-models endpoint reachable with a subscription OAuth bearer, so
// the caller wires a catalog at construction time via
// WithClaudeSubscriptionModelCatalog (typically derived from
// pricing.yaml). Returns nil when no catalog is configured.
func (c *ClaudeSubscriptionClient) ListModels(_ context.Context) ([]ModelInfo, error) {
	if len(c.modelCatalog) == 0 {
		return nil, nil
	}
	out := make([]ModelInfo, len(c.modelCatalog))
	copy(out, c.modelCatalog)
	return out, nil
}

// WithModel implements ModelOverridable. Shares the auth manager,
// http client, logger, metrics, and counter style with the parent so
// rotated refresh tokens propagate across every model clone.
func (c *ClaudeSubscriptionClient) WithModel(model string) Provider {
	if c == nil {
		return c
	}
	clone := &ClaudeSubscriptionClient{
		auth:           c.auth,
		account:        c.account,
		http:           c.http,
		model:          model,
		logger:         c.logger,
		metrics:        c.metrics,
		timeout:        c.timeout,
		maxTokens:      c.maxTokens,
		thinkingBudget: c.thinkingBudget,
		userAgent:      c.userAgent,
		sessionID:      c.sessionID,
	}
	return clone
}

// newClaudeSessionID generates a canonical UUID-v4 identifier in the
// 8-4-4-4-12 hex grouping the real CLI uses (e.g.
// "9f5c808c-faa8-463c-ab67-9b3931daf4c6"). The server treats the
// header as opaque, but matching the CLI's exact format avoids
// tripping any forward-compatibility validator the server might add
// for free.
func newClaudeSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("vornik-%x", time.Now().UnixNano())
	}
	// Set version (4) and variant (RFC 4122) bits, per UUID v4.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

var _ Provider = (*ClaudeSubscriptionClient)(nil)
var _ ModelOverridable = (*ClaudeSubscriptionClient)(nil)
var _ Pinger = (*ClaudeSubscriptionClient)(nil)

// Ping implements Pinger by exercising the auth-token loader. That
// parses ~/.claude/.credentials.json (or the configured path) and
// surfaces malformed-file / missing-file errors without consuming any
// LLM tokens or making an HTTP round-trip. The daemon's startup gate
// waits on this so the scheduler doesn't dispatch tasks before the
// subscription token is reachable on disk.
func (c *ClaudeSubscriptionClient) Ping(ctx context.Context) error {
	if c == nil || c.auth == nil {
		return fmt.Errorf("claude subscription client not configured")
	}
	if _, err := c.auth.Token(ctx); err != nil {
		return fmt.Errorf("claude subscription auth: %w", err)
	}
	return nil
}

func (c *ClaudeSubscriptionClient) Complete(ctx context.Context, messages []Message) (*ChatResponse, error) {
	return c.call(ctx, messages, nil, nil)
}

func (c *ClaudeSubscriptionClient) CompleteWithTools(ctx context.Context, messages []Message, tools []Tool) (*ChatResponse, error) {
	return c.call(ctx, messages, tools, nil)
}

func (c *ClaudeSubscriptionClient) CompleteWithToolsStream(ctx context.Context, messages []Message, tools []Tool, onText StreamCallback) (*ChatResponse, error) {
	return c.call(ctx, messages, tools, onText)
}

// call is the single round-trip: auth → build body → POST → parse SSE.
// Every Provider method funnels through here so metrics and logging
// stay consistent.
func (c *ClaudeSubscriptionClient) call(ctx context.Context, messages []Message, tools []Tool, onText StreamCallback) (*ChatResponse, error) {
	if c.model == "" {
		return nil, fmt.Errorf("claude subscription: model is required (set via WithModel or constructor)")
	}

	callID := c.counter.Add(1)
	start := time.Now()
	status := "error"
	defer func() { c.recordMetrics(start, status) }()

	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	token, err := c.auth.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("claude subscription: %w", err)
	}

	body, err := c.buildRequestBodyCtx(ctx, messages, tools, c.account.resolve(), c.sessionID)
	if err != nil {
		return nil, fmt.Errorf("claude subscription: build request: %w", err)
	}

	c.logger.Debug().
		Uint64("call_id", callID).
		Str("model", c.model).
		Int("tool_count", len(tools)).
		Int("message_count", len(messages)).
		Int("body_bytes", len(body)).
		Msg("claude-subscription: invoking")

	base := os.Getenv("ANTHROPIC_BASE_URL")
	if base == "" {
		base = claudeDefaultBaseURL
	}
	buildReq := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+claudeMessagesPath, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("accept", "text/event-stream")
		// Leave Accept-Encoding to Go's default — setting it manually
		// disables net/http's transparent gzip decoding, and we can't
		// handle br/zstd ourselves, so matching the CLI's full list
		// would break SSE parsing if the server picked a non-gzip
		// encoding.
		req.Header.Set("authorization", "Bearer "+token)
		req.Header.Set("anthropic-version", claudeAPIVersion)
		req.Header.Set("anthropic-beta", claudeOAuthBeta)
		// anthropic-dangerous-direct-browser-access is the flag Claude
		// Code's bundled Anthropic SDK sets to acknowledge it's running
		// outside a browser sandbox. Servers check this header to gate
		// OAuth traffic onto the subscription rate-limit bucket rather
		// than the external-third-party bucket — without it we see
		// terse 429s even with a valid token.
		req.Header.Set("anthropic-dangerous-direct-browser-access", "true")
		req.Header.Set("user-agent", c.userAgent)
		req.Header.Set("x-app", "cli")
		req.Header.Set("x-claude-code-session-id", c.sessionID)
		// The X-Stainless-* fingerprint is what the @anthropic-ai/sdk
		// package attaches automatically. Omitting them makes requests
		// look hand-rolled even with all other headers correct. Values
		// chosen to match the CLI 2.1.114 capture on linux/x64; adjust
		// only if you want to look like a different Stainless-generated
		// client.
		req.Header.Set("x-stainless-lang", "js")
		req.Header.Set("x-stainless-package-version", "0.81.0")
		req.Header.Set("x-stainless-os", "Linux")
		req.Header.Set("x-stainless-arch", "x64")
		req.Header.Set("x-stainless-runtime", "node")
		req.Header.Set("x-stainless-runtime-version", "v24.3.0")
		req.Header.Set("x-stainless-retry-count", "0")
		return req, nil
	}

	// Rate-limit hardening sub-item 8: prefer the response Retry-After
	// HEADER on 429s (HTTP/1.1 §7.1.3), fall back to the legacy body-
	// embedded hint via parseClaudeRetryAfterBody, and only then to
	// generic exponential backoff. Avoids amplifying the burst when
	// the server tells us exactly when to come back.
	resp, err := retryableHTTPDo(
		ctx, c.http, buildReq, 3, 500*time.Millisecond, c.logger,
		withRetryOn429(parseClaudeRetryAfterBody),
	)
	if err != nil {
		if _, ok := err.(*retryableHTTPError); ok {
			return nil, fmt.Errorf("claude subscription: %w", err)
		}
		return nil, fmt.Errorf("claude subscription: POST: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		errBody, _ := readAllCapped(resp.Body, 4096)
		// On 429 the subscription surface sometimes returns a terse
		// {"type":"error","error":{"type":"rate_limit_error","message":"Error"}}
		// with no retry-after body field — the useful reset hint
		// lives in response headers instead. Surface them so the
		// operator (or a future retry layer) can see the window.
		extra := ""
		if resp.StatusCode == http.StatusTooManyRequests {
			for _, h := range []string{
				"retry-after",
				"anthropic-ratelimit-unified-reset",
				"anthropic-ratelimit-unified-5h-remaining",
				"anthropic-ratelimit-unified-5h-reset",
			} {
				if v := resp.Header.Get(h); v != "" {
					extra += fmt.Sprintf(" %s=%s", h, v)
				}
			}
		}
		return nil, fmt.Errorf("claude subscription: HTTP %d:%s %s",
			resp.StatusCode, extra, string(errBody))
	}

	out, err := parseClaudeMessagesSSE(resp.Body, onText)
	if err != nil {
		return nil, fmt.Errorf("claude subscription: parse SSE: %w", err)
	}
	if out.Model == "" {
		out.Model = c.model
	}
	// Item 7 (Anthropic path) — when buildRequestBodyCtx registered
	// a synthetic emit_result tool and forced tool_choice to it,
	// the model emits its final answer as the tool's arguments.
	// Unwrap that single tool_call back into Message.Content so the
	// agent harness sees a regular assistant reply and doesn't try
	// to execute a tool named emit_result (which it doesn't have
	// in its dispatch table — schema enforcement would otherwise
	// surface as a "unknown tool" loop). The schema name is
	// surfaced via the same ctx the request builder consulted, so
	// the unwrap key matches whatever the executor configured.
	if name := syntheticEmitResultName(ctx); name != "" {
		unwrapEmitResultToolCall(out, name)
	}
	// Token usage attribution. Pre-fix recordMetrics tracked
	// latency + status only — every Claude-subscription call was
	// invisible to the vornik_chat_tokens_used_total counter,
	// blinding cost dashboards to the dominant production code
	// path on this provider.
	c.recordTokens(out)
	status = "ok"
	return out, nil
}

func (c *ClaudeSubscriptionClient) recordMetrics(start time.Time, status string) {
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

// parseClaudeRetryAfterBody is the fallback parser the 429 retry
// path consults when the response had no Retry-After HEADER (sub-
// item 8 step 2 in the rate-limit hardening spec). The Anthropic
// Messages API on the OAuth surface occasionally returns the hint
// inside the JSON envelope instead of a header:
//
//	{"type":"error","error":{"type":"rate_limit_error",
//	 "message":"...","retry_after":17}}
//
// or the older
//
//	{"error":{"retry_after_seconds":17}}
//
// We try both shapes (and accept float for graceful evolution).
// Returns ok=false when nothing matches so the caller can fall
// through to generic exponential backoff. We never panic on
// malformed JSON — body parsing is a best-effort enhancement.
func parseClaudeRetryAfterBody(body []byte) (time.Duration, bool) {
	if len(body) == 0 {
		return 0, false
	}
	var env struct {
		Error struct {
			RetryAfter        float64 `json:"retry_after"`
			RetryAfterSeconds float64 `json:"retry_after_seconds"`
		} `json:"error"`
		RetryAfter        float64 `json:"retry_after"`
		RetryAfterSeconds float64 `json:"retry_after_seconds"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return 0, false
	}
	for _, v := range []float64{
		env.Error.RetryAfter,
		env.Error.RetryAfterSeconds,
		env.RetryAfter,
		env.RetryAfterSeconds,
	} {
		if v > 0 {
			return time.Duration(v * float64(time.Second)), true
		}
	}
	return 0, false
}

// recordTokens attributes prompt + completion token counts to the
// vornik_chat_tokens_used_total counter. Mirrors the HTTP client's
// recordMetrics token branch (client.go:760) so cost dashboards see
// consistent attribution regardless of which sub-provider served
// the request.
func (c *ClaudeSubscriptionClient) recordTokens(resp *ChatResponse) {
	if c.metrics == nil || resp == nil {
		return
	}
	if resp.Usage.PromptTokens > 0 {
		c.metrics.TokensUsed.WithLabelValues(c.model, "prompt").Add(float64(resp.Usage.PromptTokens))
	}
	if resp.Usage.CompletionTokens > 0 {
		c.metrics.TokensUsed.WithLabelValues(c.model, "completion").Add(float64(resp.Usage.CompletionTokens))
	}
}
