package chat

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	bedrockctltypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/rs/zerolog"
)

// BedrockProvider talks directly to AWS Bedrock's Converse API using
// the AWS SDK v2's default credential chain (env vars / shared
// credentials / IAM role). It's the native replacement for the
// LiteLLM-style proxy that previously fronted Bedrock through the
// `http` sub-provider.
//
// Phase 2 scope: full text + tool-call Converse. Tools translate
// from the OpenAI function-call shape into Bedrock's toolConfig
// and ToolUseBlock results are surfaced as Message.ToolCalls.
// CompleteWithToolsStream still falls back to a single-chunk
// non-streaming Converse (Phase 3 wires ConverseStream); callers
// using the streaming entry point get the same result wrapped in
// one onText call.
type BedrockProvider struct {
	model     string
	region    string
	maxTokens int
	timeout   time.Duration

	client    bedrockRuntimeClient
	logger    zerolog.Logger
	metricsMu sync.Mutex
	metrics   *Metrics

	// staticModelList is the fallback catalog used when live
	// catalog discovery is disabled or fails. Typically derived
	// from pricing.yaml at the container layer.
	staticModelList []ModelInfo

	// liveCatalog controls live ListFoundationModels discovery via
	// the bedrock (control-plane) API. When non-nil the provider
	// queries Bedrock once at startup and caches the result for
	// liveCatalogTTL; misses or stale entries fall back to
	// staticModelList. nil disables live discovery entirely (the
	// historical behaviour).
	liveCatalog *bedrockLiveCatalog
}

// bedrockLiveCatalog holds the cached output of ListFoundationModels
// and the bedrock control-plane client used to refresh it. Methods
// on *BedrockProvider lock catalogMu while reading/writing.
type bedrockLiveCatalog struct {
	client    *bedrock.Client
	ttl       time.Duration
	mu        sync.Mutex
	models    []ModelInfo
	expiresAt time.Time
	lastErr   error
}

// bedrockRuntimeClient is the subset of *bedrockruntime.Client the
// provider calls. The seam exists so the provider can guard the SDK
// calls behind panic recovery and so tests can inject a client that
// panics — reproducing the aws-sdk-go-v2 deserialize-middleware nil-deref
// observed on context-cancel during a daemon SIGTERM redeploy
// (2026-06-19; crash through ResponseErrorWrapper.HandleDeserialize) —
// to assert the provider converts that panic into an error instead of
// taking the whole daemon down. *bedrockruntime.Client satisfies it.
type bedrockRuntimeClient interface {
	Converse(ctx context.Context, params *bedrockruntime.ConverseInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error)
	ConverseStream(ctx context.Context, params *bedrockruntime.ConverseStreamInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error)
}

// BedrockOption configures a BedrockProvider at construction time.
type BedrockOption func(*BedrockProvider)

// WithBedrockMaxTokens caps inferenceConfig.maxTokens for every
// Converse call. 0 leaves the field unset, letting the model's hard
// max apply (Bedrock typically allows the per-model hard cap, often
// 128k+, which collapses the input budget on long-context models).
// Match this to chat.max_tokens in the daemon config.
func WithBedrockMaxTokens(n int) BedrockOption {
	return func(p *BedrockProvider) { p.maxTokens = n }
}

// WithBedrockTimeout overrides the per-request timeout. Defaults to
// chat.DefaultTimeout (5 minutes). Bedrock Converse is non-streaming,
// so this is the wall-clock cap for the whole request — set higher
// than the model's typical p99 generation time.
func WithBedrockTimeout(d time.Duration) BedrockOption {
	return func(p *BedrockProvider) {
		if d > 0 {
			p.timeout = d
		}
	}
}

// WithBedrockLogger wires a zerolog logger for request/response
// telemetry. Without one, the provider is silent (no zerolog default
// — that would be a hidden global dependency).
func WithBedrockLogger(l zerolog.Logger) BedrockOption {
	return func(p *BedrockProvider) { p.logger = l }
}

// WithBedrockStaticModelList populates ListModels with a curated set
// to use as a fallback when live discovery is disabled or fails.
// Typically derived from pricing.yaml at the container layer.
func WithBedrockStaticModelList(models []ModelInfo) BedrockOption {
	return func(p *BedrockProvider) { p.staticModelList = models }
}

// WithBedrockLiveCatalog enables live model-discovery via Bedrock's
// ListFoundationModels API. The first ListModels call queries the
// control plane in the provider's region and caches the result for
// ttl (capped at 24h; defaults to 24h when ttl <= 0). Failures
// (typically missing bedrock:ListFoundationModels IAM permission or
// unreachable endpoint) fall back to the static catalog. Each refresh
// is logged at debug level so operators can confirm the cache
// behaviour without a wall of info-level entries.
func WithBedrockLiveCatalog(ttl time.Duration) BedrockOption {
	return func(p *BedrockProvider) {
		if ttl <= 0 || ttl > 24*time.Hour {
			ttl = 24 * time.Hour
		}
		p.liveCatalog = &bedrockLiveCatalog{ttl: ttl}
	}
}

// NewBedrockProvider builds a BedrockProvider for the given AWS
// region + default model. Credentials are resolved at call time via
// the SDK's default chain; the constructor itself doesn't fail if
// credentials are missing — the first Converse call surfaces the
// auth error.
//
// Region is required (e.g. "us-east-1"). Empty model is allowed at
// construction time as long as every Complete call goes through
// WithModel(); naked Complete on an empty model returns ErrEmptyModel.
func NewBedrockProvider(ctx context.Context, region, model string, opts ...BedrockOption) (*BedrockProvider, error) {
	if region == "" {
		return nil, fmt.Errorf("bedrock: region is required")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("bedrock: load aws config: %w", err)
	}
	p := &BedrockProvider{
		model:   model,
		region:  region,
		timeout: DefaultTimeout,
		client:  bedrockruntime.NewFromConfig(cfg),
	}
	for _, opt := range opts {
		opt(p)
	}
	// Live catalog enabled by an option above — build the control-
	// plane client off the same aws.Config the runtime client uses,
	// so credentials, region, and shared retry policy all stay in
	// sync. Cheap (no network call here).
	if p.liveCatalog != nil {
		p.liveCatalog.client = bedrock.NewFromConfig(cfg)
	}
	return p, nil
}

// Model returns the currently-pinned default model. Empty when the
// provider was constructed without a default and per-request
// WithModel routing is the only path.
func (p *BedrockProvider) Model() string { return p.model }

// SetMetrics wires (or re-wires) the Prometheus metrics sink after
// construction. Goroutine-safe: callers can swap this concurrently
// with active Converse calls (the next call sees the new sink).
func (p *BedrockProvider) SetMetrics(m *Metrics) {
	p.metricsMu.Lock()
	p.metrics = m
	p.metricsMu.Unlock()
}

// WithModel returns a shallow-copied provider pinned to model. The
// underlying SDK client is shared (reusing the credential cache +
// HTTP connection pool) so per-request model overrides are cheap.
//
// Implements ModelOverridable so the chat router can dispatch
// per-call model selection without reconstructing the credential
// chain on every request.
//
// We can't `clone := *p` because BedrockProvider embeds a sync.Mutex
// and copying a Mutex value is unsafe (govet copylocks). Build the
// clone field-by-field with a fresh Mutex; the clone's metrics
// pointer is read once under the source's lock to avoid racing with
// SetMetrics. The clone's static model list shares the underlying
// slice (read-only post-construction).
func (p *BedrockProvider) WithModel(model string) Provider {
	p.metricsMu.Lock()
	metricsSnap := p.metrics
	p.metricsMu.Unlock()
	clone := &BedrockProvider{
		model:           model,
		region:          p.region,
		maxTokens:       p.maxTokens,
		timeout:         p.timeout,
		client:          p.client,
		logger:          p.logger,
		metrics:         metricsSnap,
		staticModelList: p.staticModelList,
		// Share the same liveCatalog pointer — the cache is per-
		// provider, not per-clone, so a 24h cache populated by one
		// caller is reused by every WithModel-derived sibling.
		liveCatalog: p.liveCatalog,
	}
	return clone
}

// ListModels returns the Bedrock model catalog. When live discovery
// was enabled at construction (WithBedrockLiveCatalog), the provider
// queries Bedrock's ListFoundationModels API once and caches the
// result for the configured TTL — subsequent calls return cached
// rows without a network round-trip. If the live call fails (typically
// missing bedrock:ListFoundationModels IAM permission or a temporary
// service outage), the provider falls back to the static catalog so
// the model picker still shows something useful.
//
// Returns nil + no error when neither live nor static is configured.
func (p *BedrockProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	if p.liveCatalog != nil {
		if live, err := p.fetchLiveCatalog(ctx); err == nil && len(live) > 0 {
			return live, nil
		}
		// Fall through to static when live failed or returned empty.
	}
	if len(p.staticModelList) == 0 {
		return nil, nil
	}
	out := make([]ModelInfo, len(p.staticModelList))
	copy(out, p.staticModelList)
	for i := range out {
		out[i].Provider = "bedrock"
		if out[i].Source == "" {
			out[i].Source = "static"
		}
	}
	return out, nil
}

// fetchLiveCatalog returns the live Bedrock catalog, hitting the
// ListFoundationModels API at most once per TTL window. Concurrent
// callers serialize on the catalog mutex; the slow path (the actual
// SDK call) runs under the lock so only one network round-trip
// happens per refresh — at the cost of briefly serializing several
// concurrent /api/v1/models requests after a cache expiry. Acceptable
// because operators typically hit /api/v1/models interactively.
func (p *BedrockProvider) fetchLiveCatalog(ctx context.Context) ([]ModelInfo, error) {
	lc := p.liveCatalog
	if lc == nil || lc.client == nil {
		return nil, fmt.Errorf("bedrock: live catalog not configured")
	}
	lc.mu.Lock()
	defer lc.mu.Unlock()
	if time.Now().Before(lc.expiresAt) && len(lc.models) > 0 {
		out := make([]ModelInfo, len(lc.models))
		copy(out, lc.models)
		return out, nil
	}
	// Per-request timeout: ListFoundationModels is usually <1s but
	// inherits the caller ctx's deadline. Bound it independently so
	// a slow control-plane response doesn't block /api/v1/models
	// indefinitely.
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := lc.client.ListFoundationModels(callCtx, &bedrock.ListFoundationModelsInput{
		// No filters — we want every foundation model the account
		// can see in this region, including in-progress ones the
		// model picker should surface (marked LEGACY etc. via the
		// status field on each summary, which we don't expose
		// today but could later).
	})
	if err != nil {
		lc.lastErr = err
		p.logger.Warn().Err(err).Str("region", p.region).Msg("bedrock: ListFoundationModels failed, falling back to static catalog")
		return nil, err
	}
	models := make([]ModelInfo, 0, len(resp.ModelSummaries))
	for _, s := range resp.ModelSummaries {
		id := aws.ToString(s.ModelId)
		if id == "" {
			continue
		}
		// Skip non-text models (image/embedding) for the chat-list
		// surface — they aren't routable through Converse anyway.
		if !supportsTextChat(s) {
			continue
		}
		models = append(models, ModelInfo{
			ID:       id,
			Provider: "bedrock",
			Source:   "live",
			OwnedBy:  aws.ToString(s.ProviderName),
		})
	}
	lc.models = models
	lc.expiresAt = time.Now().Add(lc.ttl)
	lc.lastErr = nil
	p.logger.Debug().Int("count", len(models)).Str("region", p.region).Time("expires_at", lc.expiresAt).Msg("bedrock: live catalog refreshed")
	out := make([]ModelInfo, len(models))
	copy(out, models)
	return out, nil
}

// supportsTextChat returns true when the Bedrock model summary
// advertises TEXT in its input AND output modalities. Filters out
// embedding-only and image-generation entries that the Converse API
// won't accept anyway — the alternative is the model picker showing
// IDs that fail the moment you select them.
func supportsTextChat(s bedrockctltypes.FoundationModelSummary) bool {
	hasIn, hasOut := false, false
	for _, m := range s.InputModalities {
		if m == bedrockctltypes.ModelModalityText {
			hasIn = true
			break
		}
	}
	for _, m := range s.OutputModalities {
		if m == bedrockctltypes.ModelModalityText {
			hasOut = true
			break
		}
	}
	return hasIn && hasOut
}

// Complete runs a chat completion with no tools available. Maps the
// OpenAI-shaped messages to Bedrock Converse, calls the SDK, and
// translates the response back. Cost metrics land via the parsed
// TokenUsage so the executor's spend dashboard stays accurate.
func (p *BedrockProvider) Complete(ctx context.Context, messages []Message) (*ChatResponse, error) {
	return p.complete(ctx, messages, nil)
}

// CompleteWithTools translates the OpenAI tools into Bedrock's
// toolConfig.tools and exposes the model's ToolUseBlock responses as
// Message.ToolCalls. The dispatcher's tool-call loop keys off
// ToolCalls being non-empty, not the FinishReason — a model that
// emits both text + a tool call gets both surfaced.
func (p *BedrockProvider) CompleteWithTools(ctx context.Context, messages []Message, tools []Tool) (*ChatResponse, error) {
	return p.complete(ctx, messages, tools)
}

// CompleteWithToolsStream uses Bedrock's ConverseStream to deliver
// per-delta text. onText is invoked with the accumulated text after
// every delta — callers that drive a typing indicator off this hook
// see the same incremental shape they'd get from any other
// streaming provider. Tool-use deltas are buffered internally and
// surfaced only on completion (incremental tool_call chunks aren't
// part of the Provider contract — the OpenAI streaming format
// emits the full call on message stop too).
//
// Falls back to non-streaming Converse if ConverseStream returns an
// initial error (e.g. model doesn't support streaming, IAM gap on
// bedrock:InvokeModelWithResponseStream). The fallback path's
// onText still gets called once with the full text so the caller's
// typing indicator updates on completion.
func (p *BedrockProvider) CompleteWithToolsStream(ctx context.Context, messages []Message, tools []Tool, onText StreamCallback) (*ChatResponse, error) {
	if p.model == "" {
		return nil, ErrEmptyModel
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("bedrock: cannot CompleteWithToolsStream with empty messages")
	}

	// Mirror the non-streaming path's response_format handling:
	// json_object on a text-only request gets a system nudge.
	if rf := ResponseFormatFromContext(ctx); rf == "json_object" && len(tools) == 0 {
		messages = appendJSONOnlySystemHint(messages)
	}

	system, bedrockMsgs, err := openAIMessagesToBedrockWithCache(messages, CacheStrategyFromContext(ctx), p.model)
	if err != nil {
		return nil, err
	}

	input := &bedrockruntime.ConverseStreamInput{
		ModelId:  aws.String(p.model),
		Messages: bedrockMsgs,
		System:   system,
	}
	// Per-request max_tokens (from ctx) wins over construction-time
	// default — same precedence as the non-streaming path.
	streamMaxTokens := p.maxTokens
	if reqMax := MaxTokensFromContext(ctx); reqMax > 0 {
		streamMaxTokens = reqMax
	}
	if streamMaxTokens > 0 {
		input.InferenceConfig = &bedrocktypes.InferenceConfiguration{
			MaxTokens: aws.Int32(int32(streamMaxTokens)),
		}
	}
	if bedrockTools, terr := openAIToolsToBedrock(tools); terr != nil {
		return nil, fmt.Errorf("bedrock: translate tools: %w", terr)
	} else if len(bedrockTools) > 0 {
		input.ToolConfig = &bedrocktypes.ToolConfiguration{Tools: bedrockTools}
	}

	timeout := p.timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	out, err := p.converseStream(callCtx, input)
	if err != nil {
		// Fall back to non-streaming. Models without ConverseStream
		// support return ValidationException on the first call; the
		// non-streaming fallback keeps the caller working with a
		// single onText callback at the end.
		dur := time.Since(start)
		p.recordMetrics(dur, nil, err)
		if p.logger.GetLevel() <= zerolog.DebugLevel {
			p.logger.Debug().
				Str("model", p.model).
				Err(err).
				Msg("bedrock: ConverseStream failed; falling back to non-streaming Converse")
		}
		resp, fallbackErr := p.complete(ctx, messages, tools)
		if fallbackErr != nil {
			return nil, fallbackErr
		}
		if onText != nil && len(resp.Choices) > 0 {
			onText(resp.Choices[0].Message.Content)
		}
		return resp, nil
	}

	stream := out.GetStream()
	defer func() { _ = stream.Close() }()

	resp, err := readBedrockStream(stream, p.model, onText)
	dur := time.Since(start)
	var usage *bedrocktypes.TokenUsage
	if resp != nil && resp.Usage.TotalTokens > 0 {
		usage = &bedrocktypes.TokenUsage{
			InputTokens:  aws.Int32(int32(resp.Usage.PromptTokens)),
			OutputTokens: aws.Int32(int32(resp.Usage.CompletionTokens)),
			TotalTokens:  aws.Int32(int32(resp.Usage.TotalTokens)),
		}
	}
	p.recordMetrics(dur, usage, err)
	if err != nil {
		return nil, fmt.Errorf("bedrock ConverseStream(%s): %w", p.model, err)
	}
	return resp, nil
}

// readBedrockStream consumes the ConverseStreamEventStream channel
// and folds the events into a single ChatResponse. Text deltas
// stream through onText incrementally; tool-use deltas accumulate
// the JSON arguments (each chunk is a partial JSON string emitted
// over multiple events) and surface as ToolCalls on the final
// response.
//
// The reader handles every documented event member; unknown types
// are skipped silently so a future SDK enum addition doesn't break
// existing callers. Errors from the channel itself (network drop,
// throttling mid-stream) bubble up so the caller can retry.
func readBedrockStream(stream *bedrockruntime.ConverseStreamEventStream, model string, onText StreamCallback) (*ChatResponse, error) {
	resp := &ChatResponse{
		Object: "chat.completion",
		Model:  model,
	}
	var (
		textBuf      strings.Builder
		reasoningBuf strings.Builder                 // accumulated reasoning_content deltas
		toolBuilders = map[int32]*streamingToolUse{} // indexed by ContentBlockIndex
		stopReason   bedrocktypes.StopReason
	)

	for ev := range stream.Events() {
		switch e := ev.(type) {
		case *bedrocktypes.ConverseStreamOutputMemberMessageStart:
			// Role marker; ignored — we always emit assistant.
			_ = e

		case *bedrocktypes.ConverseStreamOutputMemberContentBlockStart:
			if e.Value.Start == nil || e.Value.ContentBlockIndex == nil {
				continue
			}
			if tu, ok := e.Value.Start.(*bedrocktypes.ContentBlockStartMemberToolUse); ok {
				toolBuilders[*e.Value.ContentBlockIndex] = &streamingToolUse{
					id:   aws.ToString(tu.Value.ToolUseId),
					name: aws.ToString(tu.Value.Name),
				}
			}

		case *bedrocktypes.ConverseStreamOutputMemberContentBlockDelta:
			if e.Value.Delta == nil {
				continue
			}
			switch d := e.Value.Delta.(type) {
			case *bedrocktypes.ContentBlockDeltaMemberText:
				textBuf.WriteString(d.Value)
				if onText != nil {
					onText(textBuf.String())
				}
			case *bedrocktypes.ContentBlockDeltaMemberToolUse:
				if e.Value.ContentBlockIndex == nil {
					continue
				}
				if b, ok := toolBuilders[*e.Value.ContentBlockIndex]; ok && d.Value.Input != nil {
					b.argsJSON.WriteString(*d.Value.Input)
				}
			case *bedrocktypes.ContentBlockDeltaMemberReasoningContent:
				// Reasoning deltas are NOT streamed through onText
				// — that callback is the visible response. Stash
				// reasoning chunks separately so the final response's
				// Message.ReasoningContent reflects the model's CoT
				// without leaking it into the user-facing stream.
				switch rd := d.Value.(type) {
				case *bedrocktypes.ReasoningContentBlockDeltaMemberText:
					reasoningBuf.WriteString(rd.Value)
				case *bedrocktypes.ReasoningContentBlockDeltaMemberRedactedContent:
					if reasoningBuf.Len() == 0 || !strings.Contains(reasoningBuf.String(), "[redacted reasoning]") {
						if reasoningBuf.Len() > 0 {
							reasoningBuf.WriteString("\n\n")
						}
						reasoningBuf.WriteString("[redacted reasoning]")
					}
				}
				// Signature deltas (rd type *Signature) carry the
				// continuation token; we drop them on the floor —
				// they only matter when sending the reasoning back
				// to Bedrock for multi-turn, which we deliberately
				// don't do (see Message.ReasoningContent docs).
			}

		case *bedrocktypes.ConverseStreamOutputMemberContentBlockStop:
			// All deltas for this block have arrived; tool builders
			// stay keyed by index so the final ToolCalls slice can
			// emit them in stable order.
			_ = e

		case *bedrocktypes.ConverseStreamOutputMemberMessageStop:
			stopReason = e.Value.StopReason

		case *bedrocktypes.ConverseStreamOutputMemberMetadata:
			if e.Value.Usage != nil {
				resp.Usage.PromptTokens = int(aws.ToInt32(e.Value.Usage.InputTokens))
				resp.Usage.CompletionTokens = int(aws.ToInt32(e.Value.Usage.OutputTokens))
				resp.Usage.TotalTokens = int(aws.ToInt32(e.Value.Usage.TotalTokens))
			}
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}

	// Flush accumulated tool calls in index order — the OpenAI
	// shape's tool_calls is an ordered list.
	var toolCalls []ToolCall
	indexes := make([]int32, 0, len(toolBuilders))
	for idx := range toolBuilders {
		indexes = append(indexes, idx)
	}
	sortInt32(indexes)
	for _, idx := range indexes {
		b := toolBuilders[idx]
		args := b.argsJSON.String()
		if args == "" {
			args = "{}"
		}
		toolCalls = append(toolCalls, ToolCall{
			ID:   b.id,
			Type: "function",
			Function: FunctionCall{
				Name:      b.name,
				Arguments: args,
			},
		})
	}

	resp.Choices = append(resp.Choices, struct {
		Index        int     `json:"index"`
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	}{
		Index: 0,
		Message: Message{
			Role:             "assistant",
			Content:          textBuf.String(),
			ReasoningContent: reasoningBuf.String(),
			ToolCalls:        toolCalls,
		},
		FinishReason: bedrockStopReasonToOpenAI(stopReason),
	})
	return resp, nil
}

// streamingToolUse buffers the per-delta tool-call arguments. Each
// delta carries a JSON string fragment; concatenating them yields
// the final arguments JSON the model intended.
type streamingToolUse struct {
	id       string
	name     string
	argsJSON strings.Builder
}

// sortInt32 is a tiny dependency-free in-place sort so we don't pull
// "sort" + a comparator closure for a 2-element slice in the common
// case. The streaming pass typically produces 0–2 tool calls.
func sortInt32(a []int32) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}

// complete is the shared internal entrypoint for the three Complete
// variants. Splitting it lets phase 2 wire toolConfig in one place
// without touching the public-facing methods.
func (p *BedrockProvider) complete(ctx context.Context, messages []Message, tools []Tool) (*ChatResponse, error) {
	if p.model == "" {
		return nil, ErrEmptyModel
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("bedrock: cannot Complete with empty messages")
	}

	input, err := p.buildConverseInput(ctx, messages, tools)
	if err != nil {
		return nil, err
	}

	timeout := p.timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	out, err := p.converse(callCtx, input)
	dur := time.Since(start)
	var usage *bedrocktypes.TokenUsage
	if out != nil {
		usage = out.Usage
	}
	p.recordMetrics(dur, usage, err)

	if err != nil {
		// Surface AWS errors with enough shape to be actionable. The
		// SDK wraps every error with operation context already; we
		// add the model so the operator sees which call failed.
		return nil, fmt.Errorf("bedrock Converse(%s): %w", p.model, err)
	}

	msg, _ := out.Output.(*bedrocktypes.ConverseOutputMemberMessage)
	resp := bedrockOutputToChatResponse(msg, p.model, out.StopReason, out.Usage, requestIDFromMetadata(out))
	// Item 7 (Bedrock path) — when buildConverseInput registered a
	// synthetic emit_response tool and pinned ToolChoice to it, the
	// model emits its final answer as the tool's arguments. Unwrap
	// that single tool_call back into Message.Content so the agent
	// harness sees a regular assistant reply and doesn't try to
	// dispatch a tool named emit_response (which isn't in its
	// dispatch table — schema enforcement would otherwise surface
	// as an "unknown tool" 404 loop). Symmetric with the Anthropic
	// unwrap landed in commit 6f90e43 (claude_subscription.go's
	// call() method). The schema name is surfaced via the same ctx
	// the request builder consulted, so the unwrap key matches
	// whatever the executor configured.
	if name := syntheticEmitResponseName(ctx); name != "" {
		unwrapEmitResponseToolCall(resp, name)
	}
	// Surface partial tool-call extraction failures. Pre-fix these
	// were silently swallowed: a single malformed smithy document on
	// one tool call dropped every tool call in the turn, and the
	// dispatcher then treated the response as a final text answer
	// even when the model's stop_reason was tool_use. Now we log
	// the partial-extraction event so operators can spot it and so
	// the downstream tool loop carries on with whatever tool calls
	// WERE successfully extracted.
	if resp.ExtractionWarning != "" {
		toolCallCount := 0
		if len(resp.Choices) > 0 {
			toolCallCount = len(resp.Choices[0].Message.ToolCalls)
		}
		p.logger.Warn().
			Str("model", p.model).
			Str("error", resp.ExtractionWarning).
			Int("tool_calls_extracted", toolCallCount).
			Msg("bedrock: partial tool-call extraction; some tool args failed to marshal — surface the rest so the agent can continue")
	}

	if p.logger.GetLevel() <= zerolog.DebugLevel {
		toolCallCount := 0
		if len(resp.Choices) > 0 {
			toolCallCount = len(resp.Choices[0].Message.ToolCalls)
		}
		p.logger.Debug().
			Str("model", p.model).
			Int64("duration_ms", dur.Milliseconds()).
			Int("openai_messages_in", len(messages)).
			Int("bedrock_messages_sent", len(input.Messages)).
			Int("input_tokens", resp.Usage.PromptTokens).
			Int("output_tokens", resp.Usage.CompletionTokens).
			Str("finish_reason", resp.Choices[0].FinishReason).
			Int("tools_offered", len(tools)).
			Int("tool_calls_returned", toolCallCount).
			Msg("bedrock: converse completed")
	}
	return resp, nil
}

// buildConverseInput assembles the Bedrock Converse request from
// OpenAI-shaped inputs + per-request ctx options. Extracted from
// complete() so the windows-size + response_format tests can
// inspect the InferenceConfig / message shape without making real
// AWS calls.
//
// Honours, in order of precedence:
//
//   - ctx-level overrides (MaxTokensFromContext,
//     ResponseFormatFromContext)
//   - construction-time options (p.maxTokens)
//   - SDK defaults (when neither is set)
//
// The function does NOT mutate p; callers can invoke it from
// concurrent goroutines safely.
func (p *BedrockProvider) buildConverseInput(ctx context.Context, messages []Message, tools []Tool) (*bedrockruntime.ConverseInput, error) {
	rfStruct := ResponseFormatStructFromContext(ctx)
	rfType := ResponseFormatFromContext(ctx)

	// json_schema enforcement: synthesise a tool whose input
	// schema IS the user-supplied JSON Schema, then force
	// tool_choice to that tool. The model literally cannot return
	// invalid JSON — every response goes through the SDK's tool-
	// args validator before reaching us. Strongest portable
	// structured-output guarantee on Bedrock today.
	//
	// Sits before the non-tool nudge path: when json_schema is set,
	// we ALWAYS go through the tool-choice path even if the caller
	// passed no tools, because the synthetic emit_response tool
	// becomes the response carrier. The response side
	// (extractToolCallsFromContent + bedrockOutputToChatResponse)
	// unwraps the tool call's input back into Message.Content.
	if rfType == "json_schema" && rfStruct != nil && rfStruct.JSONSchema != nil {
		emitTool, name, err := buildJSONSchemaEnforcementTool(rfStruct.JSONSchema)
		if err != nil {
			return nil, fmt.Errorf("bedrock: response_format json_schema: %w", err)
		}
		// Append the synthetic tool to whatever the caller
		// passed — the model picks one but Bedrock requires the
		// others to remain in the catalogue or it complains
		// about unknown tools in conversation history.
		augmentedTools := make([]Tool, 0, len(tools)+1)
		augmentedTools = append(augmentedTools, tools...)
		augmentedTools = append(augmentedTools, emitTool)
		bedrockTools, err := openAIToolsToBedrock(augmentedTools)
		if err != nil {
			return nil, fmt.Errorf("bedrock: translate tools: %w", err)
		}
		system, bedrockMsgs, err := openAIMessagesToBedrockWithCache(messages, CacheStrategyFromContext(ctx), p.model)
		if err != nil {
			return nil, err
		}
		input := &bedrockruntime.ConverseInput{
			ModelId:  aws.String(p.model),
			Messages: bedrockMsgs,
			System:   system,
			ToolConfig: &bedrocktypes.ToolConfiguration{
				Tools: bedrockTools,
				ToolChoice: &bedrocktypes.ToolChoiceMemberTool{
					Value: bedrocktypes.SpecificToolChoice{Name: aws.String(name)},
				},
			},
		}
		applyMaxTokens(ctx, p, input)
		return input, nil
	}

	// json_object on text-only requests: append a system nudge so
	// the model emits JSON instead of prose. Bedrock's Converse has
	// no native response_format field; the prompt nudge is the
	// strongest portable signal we have at the text level.
	//
	// json_object on tool-bearing requests: force ToolChoice = any
	// so the model MUST call ONE of the offered tools. Each tool's
	// input schema validates the response shape. Stronger than the
	// nudge for the with-tools case.
	if rfType == "json_object" {
		if len(tools) == 0 {
			messages = appendJSONOnlySystemHint(messages)
		}
	}

	system, bedrockMsgs, err := openAIMessagesToBedrockWithCache(messages, CacheStrategyFromContext(ctx), p.model)
	if err != nil {
		return nil, err
	}

	input := &bedrockruntime.ConverseInput{
		ModelId:  aws.String(p.model),
		Messages: bedrockMsgs,
		System:   system,
	}
	applyMaxTokens(ctx, p, input)
	if bedrockTools, err := openAIToolsToBedrock(tools); err != nil {
		return nil, fmt.Errorf("bedrock: translate tools: %w", err)
	} else if len(bedrockTools) > 0 {
		// ToolChoice stays at Bedrock's default (auto) — the model
		// decides when to call a tool and when to emit the final
		// answer.
		//
		// Pre-fix this branch forced ToolChoice = any whenever
		// json_object was set with tools present, on the
		// "model can't fall back to prose" theory. The cost was
		// catastrophic for agentic loops: any-choice means the
		// model MUST call a tool on EVERY turn — finish_reason
		// is always "tool_use" — so the agent's exit condition
		// (finish_reason != tool_calls) never fires and the loop
		// burns its full iteration budget producing no
		// result.json. Reproduced 2026-05-08 on the assistant-
		// swarm researcher: outputSchema → effectiveResponseFormat
		// returns "json_object" → ToolChoice=any → model loops
		// 28 turns calling memory_search/file_read forever →
		// schema violation: required keys [research, produced_files]
		// missing. Removing the forcing lets the model emit a
		// final text turn with the JSON result; the prompt-
		// injected schema text + post-validation provide the
		// shape contract without blocking termination.
		//
		// The json_schema path (handled earlier in this function)
		// still pins ToolChoice to the synthetic emit_response
		// tool — there the synthetic tool IS the final answer
		// carrier, so forcing it is correct.
		input.ToolConfig = &bedrocktypes.ToolConfiguration{Tools: bedrockTools}
	}
	return input, nil
}

// applyMaxTokens stamps the effective max_tokens onto the input's
// InferenceConfig. Per-request ctx override wins over the
// construction-time default. Extracted from buildConverseInput so
// the json_schema path doesn't have to duplicate the precedence
// logic.
func applyMaxTokens(ctx context.Context, p *BedrockProvider, input *bedrockruntime.ConverseInput) {
	effectiveMaxTokens := p.maxTokens
	if reqMax := MaxTokensFromContext(ctx); reqMax > 0 {
		effectiveMaxTokens = reqMax
	}
	if effectiveMaxTokens > 0 {
		input.InferenceConfig = &bedrocktypes.InferenceConfiguration{
			MaxTokens: aws.Int32(int32(effectiveMaxTokens)),
		}
	}
}

// recordMetrics emits provider-level latency + error counters when a
// Metrics sink is wired. Labels match the daemon-wide Metrics struct
// so the bedrock and http providers share dashboard semantics.
// converse and converseStream wrap the AWS SDK calls with panic
// recovery. The aws-sdk-go-v2 bedrockruntime deserialize middleware can
// nil-deref when the request context is cancelled mid-flight — observed
// 2026-06-19 when a task cancel aborted an in-flight Converse while the
// daemon was handling SIGTERM for a redeploy, crashing the process
// through ResponseErrorWrapper.HandleDeserialize. The middleware runs
// synchronously in the caller's goroutine, so a recover here catches it.
// Bedrock calls made from a net/http handler are already protected by the
// server's per-request recover, but the daemon's internal callers
// (autonomy lead, judge, memory titler, classifier) run on plain
// background goroutines where an unrecovered panic is fatal to the whole
// process. Converting it to an error keeps the daemon alive; a cancelled
// call then fails like any other.
func (p *BedrockProvider) converse(ctx context.Context, input *bedrockruntime.ConverseInput) (out *bedrockruntime.ConverseOutput, err error) {
	defer func() {
		if r := recover(); r != nil {
			out = nil
			err = fmt.Errorf("recovered panic in aws-sdk bedrockruntime Converse: %v", r)
		}
	}()
	if bedrockTimingEnabled() {
		n, tn := len(input.Messages), bedrockToolCount(input.ToolConfig)
		logBedrockSDKStart(p.logger, "Converse", p.model, n, tn)
		start := time.Now()
		out, err = p.client.Converse(ctx, input)
		logBedrockSDKEnd(p.logger, "Converse", p.model, n, tn, start, err)
		return out, err
	}
	return p.client.Converse(ctx, input)
}

func (p *BedrockProvider) converseStream(ctx context.Context, input *bedrockruntime.ConverseStreamInput) (out *bedrockruntime.ConverseStreamOutput, err error) {
	defer func() {
		if r := recover(); r != nil {
			out = nil
			err = fmt.Errorf("recovered panic in aws-sdk bedrockruntime ConverseStream: %v", r)
		}
	}()
	if bedrockTimingEnabled() {
		n, tn := len(input.Messages), bedrockToolCount(input.ToolConfig)
		logBedrockSDKStart(p.logger, "ConverseStream", p.model, n, tn)
		start := time.Now()
		out, err = p.client.ConverseStream(ctx, input)
		logBedrockSDKEnd(p.logger, "ConverseStream", p.model, n, tn, start, err)
		return out, err
	}
	return p.client.ConverseStream(ctx, input)
}

func (p *BedrockProvider) recordMetrics(dur time.Duration, usage *bedrocktypes.TokenUsage, err error) {
	p.metricsMu.Lock()
	m := p.metrics
	p.metricsMu.Unlock()
	if m == nil {
		return
	}
	if m.RequestDuration != nil {
		m.RequestDuration.WithLabelValues(p.model).Observe(dur.Seconds())
	}
	status := "success"
	if err != nil {
		status = "error"
		if m.ErrorsTotal != nil {
			m.ErrorsTotal.WithLabelValues(p.model, "bedrock_converse").Inc()
		}
	}
	if m.RequestsTotal != nil {
		m.RequestsTotal.WithLabelValues(p.model, status).Inc()
	}
	if usage != nil && m.TokensUsed != nil {
		if in := aws.ToInt32(usage.InputTokens); in > 0 {
			m.TokensUsed.WithLabelValues(p.model, "prompt").Add(float64(in))
		}
		if out := aws.ToInt32(usage.OutputTokens); out > 0 {
			m.TokensUsed.WithLabelValues(p.model, "completion").Add(float64(out))
		}
	}
}

// requestIDFromMetadata pulls the AWS request ID off the response
// for diagnostic logging. Empty when the SDK didn't capture one
// (rare — the middleware almost always populates it).
func requestIDFromMetadata(out *bedrockruntime.ConverseOutput) string {
	if out == nil {
		return ""
	}
	// ResultMetadata is map-like; the AWS SDK doesn't expose a typed
	// accessor for the request ID outside the smithy middleware
	// layer. The empty string is fine for phase 1 telemetry — the
	// SDK already logs the ID on errors via its own middleware.
	return ""
}
