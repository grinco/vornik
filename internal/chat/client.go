// Package chat provides an OpenAI-compatible chat client for vornik.
package chat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// maxChatResponseBytes caps upstream chat completion response size to guard
// against memory exhaustion from a hostile or misconfigured gateway.
const maxChatResponseBytes = 32 * 1024 * 1024

// Message represents a chat message. Content can be either a plain string
// (the common fast path) OR a slice of content blocks (multimodal input,
// e.g. text + image). When Blocks is non-empty it takes precedence over
// Content during marshalling; on unmarshal, a string in the wire format
// populates Content and an array populates Blocks. This mirrors the OpenAI
// Chat Completions content shape so providers that speak OpenAI-compat
// (Vertex Gemini, bedrock-access-gateway, local Ollama) carry images
// through transparently. Subscription paths (Claude, Codex) translate
// blocks into their vendor-specific content arrays in their converters.
type Message struct {
	Role       string         `json:"-"`
	Content    string         `json:"-"`
	Blocks     []ContentBlock `json:"-"`
	ToolCalls  []ToolCall     `json:"-"`
	ToolCallID string         `json:"-"`
	Name       string         `json:"-"`
	// CachePrefix marks this message as the end of a stable prefix
	// segment for provider-native prompt-prefix caching. When the
	// ChatRequest's CacheStrategy is non-off, sub-provider
	// converters that support native caching (Bedrock,
	// Anthropic via claude_subscription) insert the cache pragma
	// after this message. Sub-providers without native support
	// (OpenAI-compat, Vertex, Ollama) ignore the flag.
	//
	// Multiple messages may carry CachePrefix; the last one wins
	// for providers that allow only one cache marker (Bedrock's
	// CachePointBlock binds to a specific position in the system
	// or message-content array).
	CachePrefix bool `json:"-"`
	// ReasoningContent carries the model's chain-of-thought text
	// when the underlying provider exposes it as a separate
	// content block (Bedrock's ContentBlockMemberReasoningContent
	// for kimi-k2-thinking / Anthropic Claude with thinking
	// budgets). Kept distinct from Content so the visible reply
	// stays clean — downstream parsers (gates, plausibility,
	// hallucination judge) read Content; observability + cost
	// dashboards can read ReasoningContent independently.
	//
	// Empty when the provider didn't emit reasoning. Multi-turn
	// conversations DO NOT echo this field back upstream — Bedrock
	// only requires the {text, signature} pair on the WIRE for
	// continuation, and the converter handles that internally.
	// Callers shouldn't populate this on outbound messages.
	ReasoningContent string `json:"-"`
}

// ContentBlock is a single piece of multimodal message content. The shape
// matches the OpenAI Chat Completions API: a "type" discriminator plus
// the corresponding payload field. Only the field for the matching type
// should be populated; others are emitted with omitempty so the wire
// stays clean.
//
// Recognised types:
//   - "text"          — plain UTF-8 text in Text
//   - "image_url"     — image carried as a remote URL or data: URL
//   - "document_url"  — non-image attachment (PDF / DOCX / CSV / TXT
//     / MD / HTML / XLS) carried as a data: URL with
//     the matching MIME type. Bedrock surfaces these
//     as DocumentBlock content; OpenAI doesn't have
//     a standard document type, so this is a vornik
//     convention that providers translate per-vendor.
type ContentBlock struct {
	Type        string              `json:"type"`
	Text        string              `json:"text,omitempty"`
	ImageURL    *ImageURLContent    `json:"image_url,omitempty"`
	DocumentURL *DocumentURLContent `json:"document_url,omitempty"`
}

// DocumentURLContent mirrors ImageURLContent for non-image
// attachments. URL accepts a data: URL with one of the MIME types
// Bedrock recognises (application/pdf, text/csv, text/plain,
// text/markdown, text/html, application/msword,
// application/vnd.openxmlformats-officedocument.wordprocessingml.document,
// application/vnd.ms-excel,
// application/vnd.openxmlformats-officedocument.spreadsheetml.sheet).
// Name is required by Bedrock (DocumentBlock.Name); supply a neutral
// human-friendly identifier — this field can be quoted back at the
// model so don't put instructions in it.
type DocumentURLContent struct {
	URL  string `json:"url"`
	Name string `json:"name,omitempty"`
}

// ImageURLContent carries an image either as a remote URL or as an inline
// data URL ("data:image/jpeg;base64,..."). Detail mirrors OpenAI's
// low|high|auto hint; providers that don't honor it ignore the field.
type ImageURLContent struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// TextBlock is a small constructor for the most common content block.
// Exported as a content-block construction helper used across packages'
// multimodal-converter tests.
func TextBlock(text string) ContentBlock {
	return ContentBlock{Type: "text", Text: text}
}

// ImageBlock builds an image content block from an inline data URL or a
// remote URL. Use BuildDataURL to encode a local file into the data: form.
func ImageBlock(url string) ContentBlock {
	return ContentBlock{Type: "image_url", ImageURL: &ImageURLContent{URL: url}}
}

// BuildDataURL encodes raw image bytes as a data URL with the given MIME
// type. Empty bytes (or empty mimeType) returns "".
func BuildDataURL(mimeType string, data []byte) string {
	if len(data) == 0 || mimeType == "" {
		return ""
	}
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// messageWire is the on-the-wire shape. Content is a RawMessage so we can
// emit either a string or an array, and accept either form on unmarshal.
type messageWire struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

// MarshalJSON emits the OpenAI-compatible wire shape: content as a JSON
// string when Blocks is empty, content as an array of blocks when Blocks
// is set. A message with no Content and no Blocks emits an empty string
// — some providers reject missing content outright.
func (m Message) MarshalJSON() ([]byte, error) {
	w := messageWire{
		Role:       m.Role,
		ToolCalls:  m.ToolCalls,
		ToolCallID: m.ToolCallID,
		Name:       m.Name,
	}
	switch {
	case len(m.Blocks) > 0:
		b, err := json.Marshal(m.Blocks)
		if err != nil {
			return nil, fmt.Errorf("chat.Message: marshal blocks: %w", err)
		}
		w.Content = b
	default:
		b, err := json.Marshal(m.Content)
		if err != nil {
			return nil, fmt.Errorf("chat.Message: marshal content: %w", err)
		}
		w.Content = b
	}
	return json.Marshal(w)
}

// UnmarshalJSON accepts the OpenAI-compatible content shape: either a
// JSON string (populates Content) or an array of content blocks
// (populates Blocks). Anything else is rejected with a descriptive
// error so a malformed agent payload fails loudly instead of silently
// dropping the user's input.
func (m *Message) UnmarshalJSON(data []byte) error {
	var w messageWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	m.Role = w.Role
	m.ToolCalls = w.ToolCalls
	m.ToolCallID = w.ToolCallID
	m.Name = w.Name
	m.Content = ""
	m.Blocks = nil

	if len(w.Content) == 0 || string(w.Content) == "null" {
		return nil
	}
	// Try the string fast path first — the dominant case.
	var s string
	if err := json.Unmarshal(w.Content, &s); err == nil {
		m.Content = s
		return nil
	}
	// Fall back to the multimodal array form.
	var blocks []ContentBlock
	if err := json.Unmarshal(w.Content, &blocks); err == nil {
		m.Blocks = blocks
		return nil
	}
	return fmt.Errorf("chat.Message: content must be a string or an array of content blocks")
}

// EffectiveText returns the text content of the message, joining any
// text blocks when Blocks is set. Image blocks are skipped. Useful for
// callers that only care about the text payload (logs, token estimates,
// regex parsers) and don't need to handle the multimodal shape.
func (m Message) EffectiveText() string {
	if len(m.Blocks) == 0 {
		return m.Content
	}
	var sb strings.Builder
	for _, b := range m.Blocks {
		if b.Type == "text" && b.Text != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// ToolCall represents a tool invocation requested by the model.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
	// ExtraContent carries provider-specific tool-call metadata that
	// must be replayed verbatim. Vertex/Gemini's OpenAI-compatible
	// endpoint uses this for google.thought_signature; dropping it
	// makes Gemini 3 reject the next request after a tool call.
	ExtraContent json.RawMessage `json:"extra_content,omitempty"`
}

// FunctionCall contains the function name and JSON-encoded arguments.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool defines a tool the model can call.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction describes a callable function with JSON Schema parameters.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ChatRequest is the OpenAI-compatible request.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	// Stream is the OpenAI SSE-streaming directive. Decoded so the
	// chat-proxy handler can reject stream:true with a typed error
	// instead of silently returning a buffered JSON body that
	// openai-python interprets as a malformed SSE event and surfaces
	// as a 500 on the client side. Internal callers (agent harness)
	// never set this — they use the non-streaming Provider methods
	// directly. Wire SSE support is the next slice if we want it.
	Stream  bool           `json:"stream,omitempty"`
	Options map[string]any `json:"options,omitempty"` // provider-specific (e.g. Ollama num_ctx)
	// ResponseFormat carries OpenAI-shape structured-output
	// directives. The agent harness sets this from the role's
	// responseFormat: "json_object" YAML field; providers that
	// honour it (bedrock via prompt augmentation; http/vertex by
	// passing the field through) use it to nudge or force
	// JSON-only output.
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
	// CacheStrategy controls prompt-prefix cache annotations on
	// the outgoing request. When unset (default), the request is
	// sent verbatim — backwards-compatible. When set, the
	// router's per-sub-provider converter inserts the native cache
	// pragma based on the strategy's Mode + per-Message
	// CachePrefix flags.
	//
	// See https://docs.vornik.io
	CacheStrategy *CacheStrategy `json:"cache_strategy,omitempty"`
}

// CacheStrategy carries the operator-level prompt-cache directives
// for one ChatRequest. Designed to be set by the router from a
// daemon-wide config bool — callers don't usually construct this
// directly.
type CacheStrategy struct {
	// Mode is the cache-injection mode.
	//
	//   - "off"    — no annotations (default; same as nil
	//                CacheStrategy).
	//   - "prefix" — cache after every Message marked
	//                CachePrefix=true. Caller-controlled.
	//   - "auto"   — when no Message has CachePrefix=true, the
	//                converter auto-marks the last system message
	//                (the headline use-case: stable per-role
	//                system prompts shared across an entire
	//                project lifetime).
	Mode string `json:"mode"`

	// TTL is the requested cache TTL. Bedrock + Anthropic clamp
	// to their max (5 min). Zero means use the provider default.
	TTL time.Duration `json:"ttl,omitempty"`
}

// CacheMode constants. Use these instead of the bare strings so a
// typo in caller code lands at compile time.
const (
	CacheModeOff    = "off"
	CacheModePrefix = "prefix"
	CacheModeAuto   = "auto"
)

// ResponseFormat is the OpenAI-shape structured-output directive.
// Two shapes are recognised:
//
//   - {"type": "json_object"} — loose JSON-only output. Bedrock
//     enforces via a system-prompt nudge when no tools are
//     present; with tools, the converter forces tool_choice =
//     required so the model MUST call one of the offered tools
//     (the strongest portable structured-output guarantee).
//
//   - {"type": "json_schema", "json_schema": {"name": "...",
//     "schema": <jsonschema object>}} — typed structured output.
//     Bedrock enforces by injecting a synthetic tool whose input
//     schema IS the user-supplied JSON Schema, then forcing
//     tool_choice to that tool. The model's tool_use block's
//     input is unwrapped back into the visible Content on the
//     response side. Strict-mode enforcement at the wire level —
//     the model literally cannot return invalid JSON.
//
// Other providers (http / vertex) pass the field through their
// wire shape; subscription providers translate per-vendor.
type ResponseFormat struct {
	Type       string              `json:"type"`
	JSONSchema *ResponseJSONSchema `json:"json_schema,omitempty"`
}

// ResponseJSONSchema carries the typed JSON Schema for the
// {"type":"json_schema"} variant of ResponseFormat. Mirrors the
// OpenAI Chat Completions API shape so an agent harness that's
// already wiring the field for OpenAI just lands here too.
type ResponseJSONSchema struct {
	// Name is a short identifier for the schema, surfaced as the
	// synthetic tool's name when bedrock enforces via tool_choice.
	// Required by OpenAI; required here too for symmetry.
	Name string `json:"name"`
	// Description is optional human prose surfaced as the tool's
	// description.
	Description string `json:"description,omitempty"`
	// Schema is the JSON Schema object the response must conform
	// to. Top-level type MUST be "object" (Bedrock's tool-input
	// validation rejects non-object schemas).
	Schema json.RawMessage `json:"schema"`
	// Strict, when true, asks the provider to enforce the schema
	// rather than treat it as a hint. Bedrock's forced tool_choice
	// path is always strict, so this field is informational on the
	// bedrock route; other providers may use it to switch
	// validation modes.
	Strict bool `json:"strict,omitempty"`
}

// ChatResponse is the OpenAI-compatible response.
type ChatResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Created int64  `json:"created,omitempty"`
	Choices []struct {
		Index        int     `json:"index"`
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		// CacheCreationTokens is the count of input tokens written
		// into the provider's prompt cache on this request. Zero
		// when no cache annotations were sent (phase A — always),
		// when the provider doesn't support caching (Vertex /
		// HTTP / Ollama), or when no cache hit occurred. Populated
		// by Bedrock (ConverseResponse.Usage.CacheWriteInputTokens)
		// and Anthropic (response.usage.cache_creation_input_tokens)
		// per the LLM-caching design doc.
		CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`
		// CacheReadTokens is the count of input tokens served from
		// the provider's prompt cache, replacing what would
		// otherwise have been fresh prompt-tokens. Pricing
		// typically charges ~10% of the fresh rate for these.
		// Populated by Bedrock + Anthropic; zero for the other
		// sub-providers.
		CacheReadTokens int `json:"cache_read_tokens,omitempty"`
	} `json:"usage"`
	// ExtractionWarning is set by the Bedrock converter when one or
	// more tool-call argument blocks failed to marshal. The response
	// still carries the successfully-extracted tool calls in
	// Choices[i].Message.ToolCalls — this field exists so the caller
	// (BedrockProvider.complete) can log the partial-extraction
	// event with its own logger context. Empty when extraction was
	// fully successful or no tool calls were present. Excluded from
	// JSON serialization (- tag) because it's an internal signal,
	// not a wire-format value.
	ExtractionWarning string `json:"-"`
}

// Error represents an API error response.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Error implements the error interface.
func (e *Error) Error() string {
	return fmt.Sprintf("chat error %d: %s", e.Code, e.Message)
}

// GatewayError wraps a non-2xx HTTP response from the chat endpoint. It
// preserves the HTTP status code so callers can distinguish retryable
// gateway-side failures (5xx, 429) from client errors (400, 401, 404) that
// would fail again on retry. Unlike Error, whose Code field comes from the
// JSON response body and may be zero or model-specific, Status here is
// always the real HTTP status code.
type GatewayError struct {
	Status  int    // HTTP status code (e.g. 500, 502, 429)
	Message string // parsed error.message from the response body, when present
	Body    string // truncated raw response body, for diagnostics
}

// Error implements the error interface.
func (g *GatewayError) Error() string {
	if g.Message != "" {
		return fmt.Sprintf("gateway error %d: %s", g.Status, g.Message)
	}
	return fmt.Sprintf("gateway error %d: %s", g.Status, g.Body)
}

// Retryable reports whether the gateway failure is worth retrying after
// pruning conversation state. 5xx and 429 are the canonical "try again"
// statuses; 4xx other than 429 indicates the request itself is malformed
// or unauthorized and retrying won't help.
func (g *GatewayError) Retryable() bool {
	return g.Status >= 500 || g.Status == 429
}

// Common errors.
var (
	ErrEmptyEndpoint   = errors.New("endpoint cannot be empty")
	ErrEmptyModel      = errors.New("model cannot be empty")
	ErrEmptyMessages   = errors.New("messages cannot be empty")
	ErrRequestFailed   = errors.New("request failed")
	ErrInvalidResponse = errors.New("invalid response")
)

// Client is an OpenAI-compatible HTTP client.
type Client struct {
	endpoint    string
	apiKey      string
	model       string
	contextSize int // context window tokens (sent as options.num_ctx); 0 = omit
	maxTokens   int // max output tokens per call; 0 = omit (use provider default)
	// authHeader/authPrefix control how the API key is carried on the wire.
	// Empty authHeader falls back to "Authorization" with a "Bearer " prefix —
	// the default OpenAI-compatible shape. Providers that expect a raw key in
	// a custom header (e.g. Google Vertex "x-goog-api-key: <key>") set these
	// via WithAuthHeader.
	authHeader string
	authPrefix string
	// extraHeaders are static headers added to every request (completions
	// and /models) on top of Content-Type + auth. OpenRouter uses these
	// for app-attribution (HTTP-Referer / X-Title); generic for any
	// provider that wants fixed headers. Set via WithExtraHeaders. The
	// auth header is always applied last, so an extra header can never
	// override it.
	extraHeaders map[string]string
	// modelListFilter, when non-nil, is applied to ListModels output
	// (both the live-fetch and static-list paths). Returning false drops
	// the model. OpenRouter passes a `:free` suffix predicate when
	// free_only is set so discovery surfaces only zero-cost models. nil
	// = pass everything through (default).
	modelListFilter func(ModelInfo) bool
	httpClient      *http.Client
	timeout         time.Duration
	logger          zerolog.Logger
	metrics         *Metrics
	// staticModels overrides ListModels with a hardcoded list and
	// skips the /v1/models live fetch. Set via WithStaticModelList
	// for providers that don't expose an OpenAI-compat list endpoint
	// (Google Vertex's openapi surface 404s on /v1/models — only
	// /v1/chat/completions is implemented).
	staticModels []ModelInfo
	// staticModelsSet records whether WithStaticModelList was ever
	// invoked, distinguishing "operator pinned a catalog (possibly
	// empty)" from "operator didn't configure one, fall through to
	// the live /v1/models fetch". An empty slice with this flag set
	// means "this endpoint has no list API — don't even try"; Vertex
	// is the canonical case.
	staticModelsSet bool
}

// ClientOption configures the client.
type ClientOption func(*Client)

// WithTimeout sets the request timeout.
func WithTimeout(d time.Duration) ClientOption {
	return func(c *Client) {
		c.timeout = d
	}
}

// WithContextSize sets the context window size in tokens.
// Sent to the provider as options.num_ctx (Ollama) or equivalent.
// Zero means use the provider's default.
func WithContextSize(n int) ClientOption {
	return func(c *Client) {
		c.contextSize = n
	}
}

// WithMaxTokens sets the maximum output tokens per call.
// Zero means omit the field and use the provider's default.
func WithMaxTokens(n int) ClientOption {
	return func(c *Client) {
		c.maxTokens = n
	}
}

// WithStaticModelList replaces ListModels' live /v1/models fetch
// with the supplied list. Used for providers that don't expose an
// OpenAI-compat list endpoint — notably Vertex's openapi surface,
// which only implements /v1/chat/completions and 404s on /v1/models.
//
// An empty slice is a valid value: it means "this endpoint has no
// list API, return nothing rather than fall through to a live fetch
// that will fail". For Vertex specifically, callers should pass this
// option unconditionally even when their pricing-derived catalog is
// empty — otherwise the live fetch produces a 404 HTML error page in
// the model discovery output.
func WithStaticModelList(models []ModelInfo) ClientOption {
	return func(c *Client) {
		c.staticModelsSet = true
		c.staticModels = make([]ModelInfo, len(models))
		copy(c.staticModels, models)
	}
}

// WithAuthHeader overrides the HTTP header used to carry the API key.
// Default behaviour (when not called, or called with an empty name) is
// `Authorization: Bearer <key>` — the OpenAI-compatible shape every other
// sub-provider speaks. Google Vertex's OpenAI-compat endpoint accepts the
// key only via `X-Goog-Api-Key: <key>`, with no prefix; it rejects Bearer
// tokens that aren't GCP OAuth access tokens. Callers pass prefix="" for
// that case. For Bearer-style custom headers, include the trailing space
// in prefix (e.g. "Bearer ").
func WithAuthHeader(name, prefix string) ClientOption {
	return func(c *Client) {
		c.authHeader = name
		c.authPrefix = prefix
	}
}

// WithExtraHeaders adds static headers to every completion and /models
// request. Used for OpenRouter app-attribution (HTTP-Referer / X-Title)
// but generic to any provider. The map is copied defensively. An entry
// named "Authorization" is ignored — the configured api key always wins,
// so a stray config can't disable auth. nil / empty is a no-op.
func WithExtraHeaders(headers map[string]string) ClientOption {
	return func(c *Client) {
		if len(headers) == 0 {
			return
		}
		c.extraHeaders = make(map[string]string, len(headers))
		for k, v := range headers {
			if strings.EqualFold(k, "Authorization") {
				continue
			}
			c.extraHeaders[k] = v
		}
	}
}

// WithModelListFilter sets a predicate applied to ListModels output on
// both the live-fetch and static-list paths. Models for which the
// predicate returns false are dropped. nil (the default) passes
// everything through. OpenRouter uses this to surface only `:free`
// models when free_only is configured.
func WithModelListFilter(filter func(ModelInfo) bool) ClientOption {
	return func(c *Client) {
		c.modelListFilter = filter
	}
}

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(hc *http.Client) ClientOption {
	return func(c *Client) {
		c.httpClient = hc
	}
}

// WithLogger sets a logger for the client.
func WithLogger(logger zerolog.Logger) ClientOption {
	return func(c *Client) {
		c.logger = logger
	}
}

// WithMetrics sets Prometheus metrics for the client.
func WithMetrics(m *Metrics) ClientOption {
	return func(c *Client) {
		c.metrics = m
	}
}

// SetMetrics updates the Prometheus metrics on an already-created client.
// Used when observability is initialized after the client is created.
// Initializes zero-value series so Grafana always shows the error counter.
func (c *Client) SetMetrics(m *Metrics) {
	c.metrics = m
	// Touch the error counter so Prometheus exposes it even at zero.
	if m != nil && c.model != "" {
		m.RequestsTotal.WithLabelValues(c.model, "error")
		m.ErrorsTotal.WithLabelValues(c.model, "error")
	}
}

// Model returns the configured model name.
func (c *Client) Model() string {
	return c.model
}

// WithModel implements ModelOverridable. Returns a shallow-copy Client
// pinned to `model`. Shared httpClient pointer is intentional — one
// transport per host is standard practice and the model is the only
// field we want to diverge. Any pooled connections the transport
// opened stay reusable across the clone.
func (c *Client) WithModel(model string) Provider {
	if c == nil {
		return c
	}
	clone := *c
	clone.model = model
	return &clone
}

// NewClient creates a new chat client.
func NewClient(endpoint, apiKey, model string, opts ...ClientOption) *Client {
	c := &Client{
		endpoint: normalizeEndpoint(endpoint),
		apiKey:   apiKey,
		model:    model,
		timeout:  DefaultTimeout,
		logger:   zerolog.Nop(),
	}

	for _, opt := range opts {
		opt(c)
	}

	if c.httpClient == nil {
		c.httpClient = &http.Client{
			Timeout:   c.timeout,
			Transport: sharedHTTPTransport(),
		}
	}

	return c
}

// setAuthHeader writes the configured API-key header onto req. When no key
// was set the call is a no-op — relays and self-hosted gateways that don't
// authenticate stay reachable. Header name defaults to "Authorization" and
// prefix to "Bearer " so the OpenAI-compat path is unchanged; providers
// using WithAuthHeader get their own header/prefix pair instead.
func (c *Client) setAuthHeader(req *http.Request) {
	if c.apiKey == "" {
		return
	}
	name := c.authHeader
	prefix := c.authPrefix
	if name == "" {
		name = "Authorization"
		prefix = "Bearer "
	}
	req.Header.Set(name, prefix+c.apiKey)
}

// setExtraHeaders writes the configured static headers onto req. Called
// before setAuthHeader so the auth header always wins on collision (the
// WithExtraHeaders option also strips any "Authorization" entry, so this
// is belt-and-suspenders).
func (c *Client) setExtraHeaders(req *http.Request) {
	for k, v := range c.extraHeaders {
		req.Header.Set(k, v)
	}
}

func normalizeEndpoint(endpoint string) string {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	endpoint = strings.TrimSuffix(endpoint, "/chat/completions")
	return endpoint
}

// contextOptions returns the provider-specific options map when context size is set.
func (c *Client) contextOptions() map[string]any {
	if c.contextSize <= 0 {
		return nil
	}
	return map[string]any{"num_ctx": c.contextSize}
}

// Complete sends a chat completion request without tools.
func (c *Client) Complete(ctx context.Context, messages []Message) (*ChatResponse, error) {
	return c.doComplete(ctx, ChatRequest{
		Model:     c.model,
		Messages:  messages,
		MaxTokens: c.maxTokens,
		Options:   c.contextOptions(),
	})
}

// PingCompletion proves the configured endpoint+key can actually invoke the
// client's model by issuing one minimal chat completion (max_tokens=1,
// a trivial user message) and returning only the error. The response
// body is ignored — this is a reachability/invocability probe, not a
// content call. It reuses doComplete so the wire shape (auth headers,
// OpenAI-compatible request body) is owned in one place.
//
// Used by the onboarding chat validator and reusable by future
// "repair my setup" flows and feature-doctor reachability checks.
func (c *Client) PingCompletion(ctx context.Context) error {
	_, err := c.doComplete(ctx, ChatRequest{
		Model:     c.model,
		Messages:  []Message{{Role: "user", Content: "ping"}},
		MaxTokens: 1,
	})
	return err
}

// CompleteWithTools sends a chat completion request with tool definitions.
func (c *Client) CompleteWithTools(ctx context.Context, messages []Message, tools []Tool) (*ChatResponse, error) {
	return c.doComplete(ctx, ChatRequest{
		Model:     c.model,
		Messages:  messages,
		Tools:     tools,
		MaxTokens: c.maxTokens,
		Options:   c.contextOptions(),
	})
}

// doComplete executes a chat completion HTTP request.
func (c *Client) doComplete(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if c.endpoint == "" {
		return nil, ErrEmptyEndpoint
	}
	if c.model == "" {
		return nil, ErrEmptyModel
	}
	if len(req.Messages) == 0 {
		return nil, ErrEmptyMessages
	}

	// Per-request response_format (item 7 of the
	// deterministic-output-schema delivery plan). The chat-proxy
	// lifts response_format off the caller's ChatRequest and
	// stamps it on ctx via WithRequestResponseFormatStruct;
	// providers consult that ctx in their complete path so the
	// directive lands on the wire to upstream.
	//
	// Pre-fix Complete / CompleteWithTools left req.ResponseFormat
	// unset even when ctx carried one, so a role's outputSchema →
	// chat-proxy stamps json_schema → *Client.complete dropped it
	// → bedrock-access-gateway (or OpenAI/Vertex) saw a free-form
	// request and the model emitted prose. Item 7's schema
	// enforcement only works end-to-end when the directive reaches
	// the upstream JSON body, which is here.
	//
	// Per-request value wins over a struct-already-set on req
	// (rare; the dispatcher's CompleteWithTools call constructs
	// req fresh per turn so collision is unlikely). When ctx has
	// nothing, leave req.ResponseFormat as-is — preserves the
	// no-format default and the back-compat path for callers that
	// build a custom ChatRequest.
	if rf := ResponseFormatStructFromContext(ctx); rf != nil {
		req.ResponseFormat = rf
	}
	c.prepareRequestForProvider(&req)

	// Enforce timeout via context so slow or chunked responses can't
	// bypass http.Client.Timeout indefinitely.
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	reqBytes := len(body)

	url := c.endpoint + "/chat/completions"
	buildReq := func() (*http.Request, error) {
		hr, berr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if berr != nil {
			return nil, berr
		}
		hr.Header.Set("Content-Type", "application/json")
		c.setExtraHeaders(hr)
		c.setAuthHeader(hr)
		return hr, nil
	}

	start := time.Now()
	c.logger.Debug().
		Str("url", url).
		Str("model", c.model).
		Int("message_count", len(req.Messages)).
		Int("req_bytes", reqBytes).
		Msg("chat completion request started")

	// Retry transient gateway failures with backoff: 5xx (exponential)
	// and 429. The OpenAI-compat path (HTTP + Vertex sub-provider)
	// previously did a single Do, so a transient upstream blip — most
	// notably a Vertex RESOURCE_EXHAUSTED 429, which arrives WITHOUT a
	// Retry-After header — surfaced straight to the operator. Honour
	// Retry-After when present; otherwise fall back to bounded
	// exponential backoff (withGenericBackoffOn429).
	// 429-only retry: a rate limit is transient and pruning won't help,
	// so the client backs off and retries it here. 5xx is deliberately
	// NOT retried at this layer — it surfaces as a GatewayError so the
	// dispatcher's prune-and-retry (which trims bloated history that
	// often CAUSES the 5xx) can do its context-aware recovery; a blind
	// client retry of the same history would just fail again.
	resp, err := retryableHTTPDo(ctx, c.httpClient, buildReq, 3, 500*time.Millisecond, c.logger,
		withRetryOn429(nil), withGenericBackoffOn429(), withNo5xxRetry())
	if err != nil {
		var rhe *retryableHTTPError
		if errors.As(err, &rhe) {
			// Retries exhausted on a 5xx/429 — surface as a GatewayError
			// so Retryable() callers (dispatcher recovery) still see it
			// and the status label lands on metrics, same as the inline
			// status-≥400 path below. Parse the upstream error body into
			// Message so operators see the real cause, not just a code.
			statusLabel := fmt.Sprintf("http_%d", rhe.StatusCode)
			msg := ""
			var apiErr struct {
				Error *Error `json:"error"`
			}
			if json.Unmarshal([]byte(rhe.Body), &apiErr) == nil && apiErr.Error != nil {
				msg = apiErr.Error.Message
			}
			c.logger.Warn().
				Int("status_code", rhe.StatusCode).
				Str("url", url).
				Str("model", c.model).
				Dur("duration", time.Since(start)).
				Msg("chat completion exhausted gateway retries")
			c.recordMetrics(start, statusLabel, nil)
			return nil, &GatewayError{Status: rhe.StatusCode, Message: msg, Body: truncateLogString(rhe.Body, 512)}
		}
		c.logger.Warn().
			Err(err).
			Str("url", url).
			Str("model", c.model).
			Dur("duration", time.Since(start)).
			Msg("chat completion request failed")
		c.recordMetrics(start, "error", nil)
		return nil, fmt.Errorf("%w: %v", ErrRequestFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Cap the response at 32 MiB so a hostile or misconfigured gateway cannot
	// OOM the daemon. Real chat completions are far smaller; 32 MiB leaves
	// headroom for extremely large tool-call payloads.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxChatResponseBytes))
	if err != nil {
		c.logger.Warn().
			Err(err).
			Str("url", url).
			Dur("duration", time.Since(start)).
			Msg("chat completion response read failed")
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	c.logger.Debug().
		Str("url", url).
		Int("status_code", resp.StatusCode).
		Dur("duration", time.Since(start)).
		Int("response_bytes", len(respBody)).
		Msg("chat completion response received")

	if resp.StatusCode >= 400 {
		statusLabel := fmt.Sprintf("http_%d", resp.StatusCode)
		c.logger.Warn().
			Str("url", url).
			Int("status_code", resp.StatusCode).
			Int("req_bytes", reqBytes).
			Int("resp_bytes", len(respBody)).
			Dur("duration", time.Since(start)).
			Str("response_body", truncateLogString(string(respBody), 512)).
			Msg("chat completion returned error status")
		c.recordMetrics(start, statusLabel, nil)
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

	var chatResp ChatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		c.logger.Warn().
			Err(err).
			Str("url", url).
			Str("response_body", truncateLogString(string(respBody), 512)).
			Msg("chat completion response parse failed")
		return nil, fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}

	c.logger.Info().
		Str("url", url).
		Str("model", chatResp.Model).
		Int("choice_count", len(chatResp.Choices)).
		Dur("duration", time.Since(start)).
		Msg("chat completion request finished")

	c.recordMetrics(start, "success", &chatResp)
	return &chatResp, nil
}

func (c *Client) prepareRequestForProvider(req *ChatRequest) {
	if req == nil || !c.isVertexOpenAICompat() {
		return
	}
	addMissingVertexThoughtSignatures(req.Messages)
}

func (c *Client) isVertexOpenAICompat() bool {
	if strings.EqualFold(c.authHeader, "X-Goog-Api-Key") {
		return true
	}
	model := strings.ToLower(c.model)
	return strings.HasPrefix(model, "google/gemini-") || strings.HasPrefix(model, "gemini-")
}

var vertexSkipThoughtSignatureValidator = json.RawMessage(`{"google":{"thought_signature":"skip_thought_signature_validator"}}`)

func addMissingVertexThoughtSignatures(messages []Message) {
	for mi := range messages {
		if messages[mi].Role != "assistant" {
			continue
		}
		for ti := range messages[mi].ToolCalls {
			tc := &messages[mi].ToolCalls[ti]
			if len(tc.ExtraContent) != 0 || tc.Function.Name == "" {
				continue
			}
			tc.ExtraContent = append(json.RawMessage(nil), vertexSkipThoughtSignatureValidator...)
		}
	}
}

func (c *Client) recordMetrics(start time.Time, status string, resp *ChatResponse) {
	if c.metrics == nil {
		return
	}
	duration := time.Since(start).Seconds()
	c.metrics.RequestsTotal.WithLabelValues(c.model, status).Inc()
	c.metrics.RequestDuration.WithLabelValues(c.model).Observe(duration)
	if status != "success" {
		c.metrics.ErrorsTotal.WithLabelValues(c.model, status).Inc()
	}
	if resp != nil && resp.Usage.TotalTokens > 0 {
		c.metrics.TokensUsed.WithLabelValues(c.model, "prompt").Add(float64(resp.Usage.PromptTokens))
		c.metrics.TokensUsed.WithLabelValues(c.model, "completion").Add(float64(resp.Usage.CompletionTokens))
	}
}

func truncateLogString(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "...(truncated)"
}
