package service

// Chat provider wiring extracted from container.go as part of the
// 2026-05-16 service-package split. All chat-related init paths
// (HTTP, Claude CLI, Codex CLI, router-of-sub-providers) live
// here so the daemon's chat surface is reviewable in one file
// instead of being interleaved with scheduler / telegram / etc.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/version"
)

func (c *Container) initChat() error {
	cfg := c.Config.Chat
	if !cfg.Enabled {
		c.Logger.Debug().Msg("chat client disabled in config")
		return nil
	}

	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	var err error
	switch provider {
	case "claude-cli":
		err = c.initChatClaudeCLI(cfg)
	case "codex-cli":
		err = c.initChatCodexCLI(cfg)
	case "router":
		err = c.initChatRouter(cfg)
	case "", "http", "openai":
		err = c.initChatHTTP(cfg)
	default:
		c.Logger.Warn().
			Str("provider", cfg.Provider).
			Msg("chat: unknown provider — falling back to http")
		err = c.initChatHTTP(cfg)
	}
	if err != nil {
		return err
	}
	if cfg.MaxConcurrentRequests > 0 && c.ChatClient != nil {
		c.ChatClient = chat.NewQueuedProvider(c.ChatClient, cfg.MaxConcurrentRequests)
		c.Logger.Info().
			Int("max_concurrent_requests", cfg.MaxConcurrentRequests).
			Msg("chat priority queue configured")
	}
	// Wrap the resolved provider (outermost, after the queue) so EVERY
	// LLM call — chat, the memetic architect, instinct distillation,
	// judges, summarisation, all of which take their provider from
	// c.ChatClient — emits a structured "llm call" line. Metadata logs
	// at INFO; the full prompt + response bodies log at DEBUG, which
	// VORNIK_LLM_LOG_CONTENT flips on for the dedicated "llm" logger
	// without flooding every other component to debug. This is the
	// observability the architect's "confidence 0.00" diagnosis needs:
	// without it there's no way to tell whether the model was queried
	// or what it actually returned.
	if c.ChatClient != nil {
		llmLog := c.Logger.With().Str("component", "llm").Logger()
		logContent := false
		if v := strings.TrimSpace(os.Getenv("VORNIK_LLM_LOG_CONTENT")); v != "" && v != "0" && v != "false" {
			llmLog = llmLog.Level(zerolog.DebugLevel)
			logContent = true
		}
		c.ChatClient = chat.NewLoggingProvider(c.ChatClient, llmLog)
		c.Logger.Info().
			Bool("log_content", logContent).
			Msg("chat call logging enabled")
	}
	return nil
}

// initChatHTTP configures the default OpenAI-compatible HTTP client.
func (c *Container) initChatHTTP(cfg config.ChatConfig) error {
	if cfg.Endpoint == "" {
		return fmt.Errorf("chat endpoint not configured")
	}
	if cfg.APIKey == "" {
		return fmt.Errorf("chat API key not configured")
	}
	if cfg.Model == "" {
		return fmt.Errorf("chat model not configured")
	}

	opts := []chat.ClientOption{}
	if cfg.Timeout != "" {
		if timeout, err := time.ParseDuration(cfg.Timeout); err == nil {
			opts = append(opts, chat.WithTimeout(timeout))
		}
	}
	if cfg.ContextSize > 0 {
		opts = append(opts, chat.WithContextSize(cfg.ContextSize))
	}
	if cfg.MaxTokens > 0 {
		opts = append(opts, chat.WithMaxTokens(cfg.MaxTokens))
	}

	c.ChatClient = chat.NewClient(cfg.Endpoint, cfg.APIKey, cfg.Model,
		append(opts, chat.WithLogger(c.Logger.With().Str("component", "chat").Logger()))...,
	)
	c.Logger.Info().
		Str("provider", "http").
		Str("endpoint", cfg.Endpoint).
		Str("model", cfg.Model).
		Int("max_history", cfg.MaxHistory).
		Int("context_size", cfg.ContextSize).
		Int("max_tokens", cfg.MaxTokens).
		Msg("chat client configured")
	return nil
}

// initChatClaudeCLI configures the subprocess-backed Claude CLI
// provider. Endpoint + api_key from the config are ignored — the
// `claude` CLI's own credential store (normally ~/.claude/…) is the
// source of truth.
//
// Model falls back to "" when unset so Claude Code's session default
// is used; operators on a subscription that exposes multiple models
// can set cfg.Model to pick a specific one.
func (c *Container) initChatClaudeCLI(cfg config.ChatConfig) error {
	opts := []chat.CLIOption{
		chat.WithCLILogger(c.Logger.With().Str("component", "chat").Str("provider", "claude-cli").Logger()),
	}
	if cfg.CLIBinary != "" {
		opts = append(opts, chat.WithCLIBinary(cfg.CLIBinary))
	}
	if cfg.Timeout != "" {
		if timeout, err := time.ParseDuration(cfg.Timeout); err == nil {
			opts = append(opts, chat.WithCLITimeout(timeout))
		}
	}

	c.ChatClient = chat.NewCLIClient(cfg.Model, opts...)
	c.Logger.Info().
		Str("provider", "claude-cli").
		Str("binary", fallbackNonEmpty(cfg.CLIBinary, "claude")).
		Str("model", fallbackNonEmpty(cfg.Model, "(session default)")).
		Msg("chat client configured")
	return nil
}

// fallbackNonEmpty is a tiny helper for log messages — shows a
// human-readable placeholder when a value is empty (indicating the
// CLI's default will be used).
func fallbackNonEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// chatModelCatalogFromPricing builds a []chat.ModelInfo by filtering
// pricing.yaml entries with the given predicate. Used to seed the
// model-list catalog on chat sub-providers that have no upstream
// /v1/models endpoint (Claude/Codex subscription + CLI surfaces).
// Returns nil when the pricing table is empty or unconfigured — the
// caller then leaves the provider's catalog unset and ListModels
// returns nil rather than a misleading empty list.
func chatModelCatalogFromPricing(table *pricing.Table, match func(string) bool, ownedBy string) []chat.ModelInfo {
	if table == nil {
		return nil
	}
	ids := table.IDs()
	if len(ids) == 0 {
		return nil
	}
	out := make([]chat.ModelInfo, 0, len(ids))
	for _, id := range ids {
		if !match(id) {
			continue
		}
		out = append(out, chat.ModelInfo{ID: id, Source: "pricing", OwnedBy: ownedBy})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// isAnthropicModelID matches Claude/Anthropic model IDs as they appear
// in pricing.yaml. Today every Anthropic native ID starts with
// "claude-" (claude-opus-*, claude-sonnet-*, claude-haiku-*); the
// daemon's own LLD pins this convention. Bedrock-routed Claude IDs
// (anthropic.claude-* on Bedrock) are deliberately excluded — those
// belong to the Bedrock sub-provider's catalog, not the
// subscription/CLI surfaces.
func isAnthropicModelID(id string) bool {
	return strings.HasPrefix(id, "claude-")
}

// isOpenAIModelID matches the OpenAI native model IDs the Codex
// subscription / Codex CLI surfaces will accept. Covers the gpt-*
// family (gpt-5*, gpt-5.4*, gpt-5-codex, …) plus the o<digit>-*
// reasoning/deep-research naming (o3-deep-research, o4-mini-…).
// Excludes Bedrock-routed openai.* IDs for the same reason as
// isAnthropicModelID.
func isOpenAIModelID(id string) bool {
	if strings.HasPrefix(id, "gpt-") {
		return true
	}
	// o<digit>-… — first char 'o', second char a digit, then '-'.
	if len(id) >= 3 && id[0] == 'o' && id[1] >= '0' && id[1] <= '9' {
		// Walk past the leading digits to the first non-digit; that
		// must be '-' for the ID to be a reasoning-family identifier.
		i := 1
		for i < len(id) && id[i] >= '0' && id[i] <= '9' {
			i++
		}
		return i < len(id) && id[i] == '-'
	}
	return false
}

// isVertexModelID matches Vertex AI / Gemini IDs. The Vertex
// OpenAI-compat surface requires the "<publisher>/<model>" shape,
// so every Vertex entry in pricing.yaml carries the "google/" prefix.
// Anything else (gemini-* without the prefix) won't route through
// Vertex and is filtered out.
//
// OpenRouter free-tier models also carry the "google/" publisher
// prefix (e.g. "google/gemma-4-26b-a4b-it:free") but route to the
// openrouter sub-provider via the ":free" SUFFIX route, which takes
// precedence over any prefix route (see Router.providerFor). The
// catalog must mirror that precedence — otherwise a ":free" model the
// daemon actually sends to OpenRouter shows up mis-attributed under
// vertex in `vornikctl models` / GET /api/v1/models. Excluding it here
// keeps the discovery catalog consistent with where requests really
// go; OpenRouter surfaces these via its own live /v1/models list.
func isVertexModelID(id string) bool {
	return strings.HasPrefix(id, "google/") && !chat.IsFreeModel(id)
}

// isBedrockModelID matches pricing.yaml entries that aren't claimed
// by any other sub-provider — Bedrock-shaped IDs use a "<vendor>.<model>"
// convention (anthropic.*, qwen.*, openai.gpt-oss-*, etc.) so the
// rule is just "not Anthropic-native, not OpenAI-native, not Vertex".
// Used to seed the fallback catalog when ListFoundationModels fails
// (typically missing IAM permission).
func isBedrockModelID(id string) bool {
	if id == "" {
		return false
	}
	// OpenRouter ":free" models belong to the openrouter sub-provider
	// (suffix route). Without this guard they'd no longer match
	// isVertexModelID (which now excludes ":free") and would fall
	// through to the Bedrock fallback catalog — trading a vertex
	// mis-attribution for a worse bedrock one.
	if chat.IsFreeModel(id) {
		return false
	}
	if isAnthropicModelID(id) || isOpenAIModelID(id) || isVertexModelID(id) {
		return false
	}
	return true
}

// initChatCodexCLI configures the subprocess-backed Codex CLI provider.
// Mirrors initChatClaudeCLI for the sister CLI. Under ChatGPT-account
// auth cfg.Model MUST be empty (codex ignores it); under OpenAI
// API-key auth it's passed via `-m`.
func (c *Container) initChatCodexCLI(cfg config.ChatConfig) error {
	opts := []chat.CodexOption{
		chat.WithCodexLogger(c.Logger.With().Str("component", "chat").Str("provider", "codex-cli").Logger()),
	}
	if cfg.CLIBinary != "" {
		opts = append(opts, chat.WithCodexBinary(cfg.CLIBinary))
	}
	if cfg.Timeout != "" {
		if timeout, err := time.ParseDuration(cfg.Timeout); err == nil {
			opts = append(opts, chat.WithCodexTimeout(timeout))
		}
	}

	c.ChatClient = chat.NewCodexCLIClient(cfg.Model, opts...)
	c.Logger.Info().
		Str("provider", "codex-cli").
		Str("binary", fallbackNonEmpty(cfg.CLIBinary, "codex")).
		Str("model", fallbackNonEmpty(cfg.Model, "(CLI default)")).
		Msg("chat client configured")
	return nil
}

// initChatRouter builds a chat.Router composing multiple sub-providers
// and dispatching by model prefix. Operators flip Provider to "router"
// when they want agent containers to mix Claude-tier and Codex-tier
// calls in the same swarm — each role's `model:` string picks the
// backend automatically.
func (c *Container) initChatRouter(cfg config.ChatConfig) error {
	rcfg := cfg.Router

	// Build each enabled sub-provider. Skipping disabled blocks is
	// fine — the routes-validation step below flags any route that
	// names a disabled kind.
	subs := map[string]chat.Provider{}
	timeout := time.Duration(0)
	if cfg.Timeout != "" {
		if t, err := time.ParseDuration(cfg.Timeout); err == nil {
			timeout = t
		}
	}

	// Pre-build the model catalogs for sub-providers without a
	// real /v1/models endpoint. pricing.yaml doubles as the
	// operator-curated catalog — every entry there is a model the
	// daemon is ready to bill, so it's the right source of truth.
	// Predicates separate the catalog by vendor naming convention:
	// claude-* / gpt-*+o<digit>-* / google/* / everything else
	// (vendor.model — Bedrock).
	anthropicCatalog := chatModelCatalogFromPricing(c.pricingTable, isAnthropicModelID, "anthropic")
	openaiCatalog := chatModelCatalogFromPricing(c.pricingTable, isOpenAIModelID, "openai")
	vertexCatalog := chatModelCatalogFromPricing(c.pricingTable, isVertexModelID, "google")
	bedrockFallbackCatalog := chatModelCatalogFromPricing(c.pricingTable, isBedrockModelID, "")

	if rcfg.ClaudeCLI.Enabled {
		opts := []chat.CLIOption{
			chat.WithCLILogger(c.Logger.With().Str("component", "chat").Str("provider", "claude-cli").Logger()),
		}
		if rcfg.ClaudeCLI.Binary != "" {
			opts = append(opts, chat.WithCLIBinary(rcfg.ClaudeCLI.Binary))
		}
		if timeout > 0 {
			opts = append(opts, chat.WithCLITimeout(timeout))
		}
		if rcfg.ClaudeCLI.EffortLevel != "" {
			opts = append(opts, chat.WithCLIEffortLevel(rcfg.ClaudeCLI.EffortLevel))
		}
		if len(anthropicCatalog) > 0 {
			opts = append(opts, chat.WithCLIModelCatalog(anthropicCatalog))
		}
		subs["claude-cli"] = chat.NewCLIClient(rcfg.ClaudeCLI.Model, opts...)
	}
	if rcfg.CodexCLI.Enabled {
		opts := []chat.CodexOption{
			chat.WithCodexLogger(c.Logger.With().Str("component", "chat").Str("provider", "codex-cli").Logger()),
		}
		if rcfg.CodexCLI.Binary != "" {
			opts = append(opts, chat.WithCodexBinary(rcfg.CodexCLI.Binary))
		}
		if timeout > 0 {
			opts = append(opts, chat.WithCodexTimeout(timeout))
		}
		if len(openaiCatalog) > 0 {
			opts = append(opts, chat.WithCodexModelCatalog(openaiCatalog))
		}
		subs["codex-cli"] = chat.NewCodexCLIClient(rcfg.CodexCLI.Model, opts...)
	}
	if rcfg.CodexSubscription.Enabled {
		opts := []chat.CodexSubscriptionOption{
			chat.WithCodexSubscriptionLogger(c.Logger.With().Str("component", "chat").Str("provider", "codex-subscription").Logger()),
		}
		if rcfg.CodexSubscription.AuthPath != "" {
			opts = append(opts, chat.WithCodexSubscriptionAuthPath(rcfg.CodexSubscription.AuthPath))
		}
		if timeout > 0 {
			opts = append(opts, chat.WithCodexSubscriptionTimeout(timeout))
		}
		if rcfg.CodexSubscription.EffortLevel != "" {
			opts = append(opts, chat.WithCodexSubscriptionEffortLevel(rcfg.CodexSubscription.EffortLevel))
		}
		if len(openaiCatalog) > 0 {
			opts = append(opts, chat.WithCodexSubscriptionModelCatalog(openaiCatalog))
		}
		subs["codex-subscription"] = chat.NewCodexSubscriptionClient(rcfg.CodexSubscription.Model, opts...)
	}
	if rcfg.ClaudeSubscription.Enabled {
		opts := []chat.ClaudeSubscriptionOption{
			chat.WithClaudeSubscriptionLogger(c.Logger.With().Str("component", "chat").Str("provider", "claude-subscription").Logger()),
		}
		if rcfg.ClaudeSubscription.AuthPath != "" {
			opts = append(opts, chat.WithClaudeSubscriptionAuthPath(rcfg.ClaudeSubscription.AuthPath))
		}
		if timeout > 0 {
			opts = append(opts, chat.WithClaudeSubscriptionTimeout(timeout))
		}
		if rcfg.ClaudeSubscription.MaxTokens > 0 {
			opts = append(opts, chat.WithClaudeSubscriptionMaxTokens(rcfg.ClaudeSubscription.MaxTokens))
		}
		if rcfg.ClaudeSubscription.ThinkingBudget > 0 {
			opts = append(opts, chat.WithClaudeSubscriptionThinkingBudget(rcfg.ClaudeSubscription.ThinkingBudget))
		}
		if rcfg.ClaudeSubscription.UserAgent != "" {
			opts = append(opts, chat.WithClaudeSubscriptionUserAgent(rcfg.ClaudeSubscription.UserAgent))
		}
		if len(anthropicCatalog) > 0 {
			opts = append(opts, chat.WithClaudeSubscriptionModelCatalog(anthropicCatalog))
		}
		subs["claude-subscription"] = chat.NewClaudeSubscriptionClient(rcfg.ClaudeSubscription.Model, opts...)
	}
	if rcfg.HTTP.Enabled {
		if rcfg.HTTP.Endpoint == "" || rcfg.HTTP.APIKey == "" {
			return fmt.Errorf("chat.router.http: endpoint and api_key are required when http sub-provider is enabled")
		}
		// model is optional: chat.model promotes to the fallback at
		// startup, and prefix-routed requests pin via WithModel — so an
		// empty router.http.model is fine as long as one of those paths
		// supplies one. Naked calls with no override and no chat.model
		// fail at request time with ErrEmptyModel.
		if rcfg.HTTP.Model == "" && cfg.Model == "" {
			c.Logger.Warn().Msg("chat.router.http: model unset and chat.model unset — naked http calls will fail with ErrEmptyModel")
		}
		opts := []chat.ClientOption{
			chat.WithLogger(c.Logger.With().Str("component", "chat").Str("provider", "http").Logger()),
		}
		if timeout > 0 {
			opts = append(opts, chat.WithTimeout(timeout))
		}
		// Cap output tokens. Without this the request goes out with
		// max_tokens omitted; bedrock-access-gateway then substitutes
		// the model's hard max output (e.g. 128k for glm-5), which
		// collapses the usable input budget and triggers
		// "maximum context length exceeded" on legitimate prompts.
		// Per-subprovider override via chat.router.http.max_tokens
		// wins over the chat-level default.
		httpMaxTokens := rcfg.HTTP.MaxTokens
		if httpMaxTokens == 0 {
			httpMaxTokens = cfg.MaxTokens
		}
		if httpMaxTokens > 0 {
			opts = append(opts, chat.WithMaxTokens(httpMaxTokens))
		}
		subs["http"] = chat.NewClient(rcfg.HTTP.Endpoint, rcfg.HTTP.APIKey, rcfg.HTTP.Model, opts...)
	}
	if rcfg.Vertex.Enabled {
		if rcfg.Vertex.APIKey == "" || rcfg.Vertex.ProjectID == "" {
			return fmt.Errorf("chat.router.vertex: api_key and project_id are required when vertex sub-provider is enabled")
		}
		if rcfg.Vertex.Model == "" && cfg.Model == "" {
			c.Logger.Warn().Msg("chat.router.vertex: model unset and chat.model unset — naked vertex calls will fail with ErrEmptyModel")
		}
		endpoint := rcfg.Vertex.Endpoint
		if endpoint == "" {
			endpoint = buildVertexEndpoint(rcfg.Vertex.ProjectID, rcfg.Vertex.Location)
		}
		opts := []chat.ClientOption{
			chat.WithLogger(c.Logger.With().Str("component", "chat").Str("provider", "vertex").Logger()),
			// Vertex's OpenAI-compat surface rejects the Bearer header unless
			// the value is a GCP OAuth access token. API keys go in
			// `X-Goog-Api-Key` with no prefix — the HTTP body is otherwise
			// identical to every other OpenAI-compat endpoint.
			chat.WithAuthHeader("X-Goog-Api-Key", ""),
		}
		// Vertex's openapi surface only implements /v1/chat/completions
		// — /v1/models 404s with Google's HTML error page. Always pin
		// a static catalog (even an empty one) so the model-discovery
		// surface never falls through to that 404; source the contents
		// from pricing.yaml entries with the "google/" publisher prefix.
		// All Vertex IDs MUST carry the "google/" publisher prefix or
		// the OpenAI-compat surface rejects them with "Malformed
		// publisher model … expected '<publisher>/<model>'".
		opts = append(opts, chat.WithStaticModelList(vertexCatalog))
		if len(vertexCatalog) == 0 {
			c.Logger.Warn().Msg("chat.router.vertex: no google/* entries in pricing.yaml — model discovery for vertex will return an empty list")
		}
		if timeout > 0 {
			opts = append(opts, chat.WithTimeout(timeout))
		}
		if rcfg.Vertex.MaxTokens > 0 {
			opts = append(opts, chat.WithMaxTokens(rcfg.Vertex.MaxTokens))
		}
		subs["vertex"] = chat.NewClient(endpoint, rcfg.Vertex.APIKey, rcfg.Vertex.Model, opts...)
	}
	if rcfg.OpenRouter.Enabled {
		if rcfg.OpenRouter.APIKey == "" {
			return fmt.Errorf("chat.router.openrouter: api_key is required when openrouter sub-provider is enabled")
		}
		if rcfg.OpenRouter.Model == "" && cfg.Model == "" {
			c.Logger.Warn().Msg("chat.router.openrouter: model unset and chat.model unset — naked openrouter calls will fail with ErrEmptyModel")
		}
		endpoint := resolveOpenRouterEndpoint(rcfg.OpenRouter.Endpoint)
		opts := []chat.ClientOption{
			chat.WithLogger(c.Logger.With().Str("component", "chat").Str("provider", "openrouter").Logger()),
			// App-attribution headers. Default to identifying vornik by
			// name + build version (NOT a website URL) so the daemon
			// shows up coherently in OpenRouter's analytics without
			// advertising a domain. Operators override per config.
			chat.WithExtraHeaders(openRouterAttributionHeaders(rcfg.OpenRouter.Referer, rcfg.OpenRouter.Title)),
		}
		// free_only: surface only zero-cost models in discovery so
		// `vornikctl models list` against OpenRouter's 300+ catalogue is
		// actionable, matching the guard applied to completions below.
		if rcfg.OpenRouter.FreeOnly {
			opts = append(opts, chat.WithModelListFilter(func(m chat.ModelInfo) bool {
				return chat.IsFreeModel(m.ID)
			}))
		}
		if timeout > 0 {
			opts = append(opts, chat.WithTimeout(timeout))
		}
		orMaxTokens := rcfg.OpenRouter.MaxTokens
		if orMaxTokens == 0 {
			orMaxTokens = cfg.MaxTokens
		}
		if orMaxTokens > 0 {
			opts = append(opts, chat.WithMaxTokens(orMaxTokens))
		}
		var orProvider chat.Provider = chat.NewClient(endpoint, rcfg.OpenRouter.APIKey, rcfg.OpenRouter.Model, opts...)
		// free_only: hard-guard against accidental paid spend. Reject any
		// non-":free" model before it hits the wire. Opt-in — default
		// false leaves paid models reachable.
		if rcfg.OpenRouter.FreeOnly {
			orProvider = chat.NewFreeOnlyProvider(orProvider)
		}
		subs["openrouter"] = orProvider
	}
	if rcfg.Bedrock.Enabled {
		if rcfg.Bedrock.Region == "" {
			return fmt.Errorf("chat.router.bedrock: region is required when bedrock sub-provider is enabled")
		}
		if rcfg.Bedrock.Model == "" && cfg.Model == "" {
			c.Logger.Warn().Msg("chat.router.bedrock: model unset and chat.model unset — naked bedrock calls will fail with ErrEmptyModel")
		}
		bedrockOpts := []chat.BedrockOption{
			chat.WithBedrockLogger(c.Logger.With().Str("component", "chat").Str("provider", "bedrock").Logger()),
			// Live discovery via the bedrock control plane —
			// ListFoundationModels gets the operator the live catalog
			// for the region, cached for 24h. Falls back to the
			// pricing-derived list if the call errors (missing IAM
			// permission, transient outage).
			chat.WithBedrockLiveCatalog(24 * time.Hour),
		}
		if len(bedrockFallbackCatalog) > 0 {
			bedrockOpts = append(bedrockOpts, chat.WithBedrockStaticModelList(bedrockFallbackCatalog))
		}
		if timeout > 0 {
			bedrockOpts = append(bedrockOpts, chat.WithBedrockTimeout(timeout))
		}
		// Cap output tokens — same reasoning as the http sub-provider:
		// without it Bedrock applies the model's hard max output
		// (often 128k+) which collapses the input budget on
		// long-context models. chat.router.bedrock.max_tokens wins
		// over the chat-level default.
		bedrockMaxTokens := rcfg.Bedrock.MaxTokens
		if bedrockMaxTokens == 0 {
			bedrockMaxTokens = cfg.MaxTokens
		}
		if bedrockMaxTokens > 0 {
			bedrockOpts = append(bedrockOpts, chat.WithBedrockMaxTokens(bedrockMaxTokens))
		}
		bedrockClient, err := chat.NewBedrockProvider(context.Background(), rcfg.Bedrock.Region, rcfg.Bedrock.Model, bedrockOpts...)
		if err != nil {
			return fmt.Errorf("chat.router.bedrock: %w", err)
		}
		subs["bedrock"] = bedrockClient
	}

	if len(subs) == 0 {
		return fmt.Errorf("chat.router: no sub-providers enabled (set router.claude_cli.enabled / claude_subscription.enabled / codex_cli.enabled / codex_subscription.enabled / http.enabled / vertex.enabled / bedrock.enabled)")
	}

	// Default sub-provider — the fallback for requests that don't
	// match any route, and the dispatcher's own default.
	defaultKind := rcfg.Default
	if defaultKind == "" {
		// Pick a sensible default when operator didn't specify:
		// prefer http (our Bedrock gateway) → claude-subscription →
		// codex-subscription → claude-cli → codex-cli. http first
		// because the current deployment leans on it heavily; the
		// subscription paths are there for plan-billed calls.
		for _, k := range []string{"http", "claude-subscription", "codex-subscription", "vertex", "openrouter", "claude-cli", "codex-cli"} {
			if _, ok := subs[k]; ok {
				defaultKind = k
				break
			}
		}
	}
	fallback, ok := subs[defaultKind]
	if !ok {
		return fmt.Errorf("chat.router.default=%q names a sub-provider that isn't enabled", defaultKind)
	}

	// Promote top-level chat.model to the fallback's effective default.
	// Without this, chat.model was silently ignored under provider=router
	// — operators set it, the dispatcher's non-WithModel calls landed on
	// the fallback, and the fallback ran whatever its router.<sub>.model
	// happened to be. Pinning the fallback via WithModel keeps the route
	// table's references to the unpinned sub-provider intact (per-prefix
	// requests still go through WithModel(req.Model) and ignore this
	// pin) but gives autonomy / telegram / dispatcher calls a single
	// source of truth for "what does the global default model mean".
	// router.<sub>.model remains the build-time default for each sub-
	// provider, used when chat.model is empty and no per-request override
	// is supplied.
	fallbackPinSource := "router.sub.model"
	if cfg.Model != "" {
		if o, ok := fallback.(chat.ModelOverridable); ok {
			fallback = o.WithModel(cfg.Model)
			fallbackPinSource = "chat.model"
		}
	}

	// Routes. When operator left the list empty, install a sensible
	// default table. The claude-* prefix prefers claude-subscription
	// when enabled, falls back to claude-cli otherwise — that way
	// upgrading to the direct-API provider is a one-line config
	// change, and existing CLI-only deployments keep working
	// unchanged. Same logic applied to codex-* prefixes.
	//
	// 2026-05-28 (B-11): sparse operator routes used to silently fall
	// through to the default sub-provider for un-routed prefixes. A
	// fresh install with routes:[{prefix:"google/",kind:"vertex"}]
	// would send "openai.gpt-oss-120b-1:0" to Vertex, which rejects
	// the Bedrock-shape model ID with a 400. Operator never had a
	// chance to know what prefixes the system expects. Now defaults
	// are APPENDED to the operator's routes (operator wins on
	// duplicate prefix; the first match in the merged list is what
	// the router uses) so any enabled sub-provider gets its canonical
	// prefixes covered even when the operator's routes list is short.
	routeConfigs := rcfg.Routes
	if len(routeConfigs) == 0 {
		routeConfigs = defaultRouterRoutesForSubs(subs)
	} else {
		routeConfigs = mergeWithDefaultRoutes(routeConfigs, defaultRouterRoutesForSubs(subs))
	}

	var routes []chat.Route
	for _, rc := range routeConfigs {
		sub, ok := subs[rc.Kind]
		if !ok {
			c.Logger.Warn().
				Str("route_prefix", rc.Prefix).
				Str("route_kind", rc.Kind).
				Msg("chat.router: route names a sub-provider that isn't enabled — dropping")
			continue
		}
		// Per-route bounded queue (hardening sub-item 4). When the
		// operator configured queue_depth > 0, wrap the sub-provider
		// in a depth-N semaphore so autonomy bursts don't slam the
		// upstream; drop-at-cap surfaces as *chat.RouteOverflowError
		// which the chat-proxy maps to HTTP 503. depth ≤ 0 keeps
		// the legacy fire-all-in-parallel path.
		if rc.QueueDepth > 0 {
			timeout := time.Duration(rc.QueueTimeoutMs) * time.Millisecond
			// observabilityRegistry() returns a concrete *prometheus.Registry
			// that is nil during early startup (observability inits after
			// chat). Pass it as a true-nil prometheus.Registerer interface
			// rather than a typed-nil pointer, so the route-queue metrics
			// constructor's nil-guard works and we don't crash-loop the
			// daemon (regression: 2026-06-03). route-queue metrics for
			// startup-built routes are simply absent; throttling still works.
			var reg prometheus.Registerer
			if r := c.observabilityRegistry(); r != nil {
				reg = r
			}
			sub = chat.NewBoundedRouteProvider(sub, rc.Kind, rc.QueueDepth, timeout, reg)
		}
		routes = append(routes, chat.Route{Prefix: rc.Prefix, Suffix: rc.Suffix, Provider: sub, Name: rc.Kind})
	}

	router, err := chat.NewRouter(fallback, routes,
		chat.WithRouterLogger(c.Logger.With().Str("component", "chat").Str("provider", "router").Logger()),
		// Pass the named sub-provider map so router.ListModels can
		// enumerate every enabled provider — even ones that aren't
		// referenced by an explicit route.
		chat.WithRouterSubs(subs),
		// Label the fallback so the unrouted-model dispatch path
		// can decide whether to forward the request's model name.
		// bedrock/http/vertex honour the request; CLI kinds ignore
		// it (they refuse arbitrary model IDs).
		chat.WithRouterFallbackName(defaultKind),
	)
	if err != nil {
		return fmt.Errorf("chat.router: %w", err)
	}
	c.ChatClient = router

	enabledKinds := make([]string, 0, len(subs))
	for k := range subs {
		enabledKinds = append(enabledKinds, k)
	}
	c.Logger.Info().
		Str("provider", "router").
		Str("default", defaultKind).
		Strs("enabled_subs", enabledKinds).
		Int("routes", len(routes)).
		Str("fallback_model", fallback.Model()).
		Str("fallback_pin_source", fallbackPinSource).
		Msg("chat client configured")
	return nil
}

// defaultRouterRoutesForSubs returns a model-prefix → sub-provider
// table built against the currently-enabled sub-providers. Each
// prefix picks the first candidate that's actually registered, so
// the operator doesn't get a dropped-route warning just because
// they haven't flipped every provider on.
//
// Claude models prefer the direct API; the CLI subprocess is the
// fallback. Codex models follow the same logic. Entries whose
// candidates are all disabled are omitted — whichever default the
// fallback provider exposes handles them.
func defaultRouterRoutesForSubs(subs map[string]chat.Provider) []config.ChatRouteConfig {
	var out []config.ChatRouteConfig
	add := func(prefix string, candidates ...string) {
		for _, k := range candidates {
			if _, ok := subs[k]; ok {
				out = append(out, config.ChatRouteConfig{Prefix: prefix, Kind: k})
				return
			}
		}
	}
	// OpenRouter free-tier routing comes FIRST so the ":free" suffix
	// route is evaluated ahead of any vendor-prefix route below (e.g.
	// "google/" → vertex). The router gives suffix matches precedence
	// regardless of order, but keeping them first also makes the
	// rendered route table read intuitively. openrouter/ covers
	// openrouter/auto and OpenRouter-namespaced IDs.
	if _, ok := subs["openrouter"]; ok {
		out = append(out,
			config.ChatRouteConfig{Suffix: chat.FreeModelSuffix, Kind: "openrouter"},
			config.ChatRouteConfig{Prefix: "openrouter/", Kind: "openrouter"},
		)
	}
	// Subscription / CLI providers — preferred over generic Bedrock
	// for matching prefixes (Anthropic + OpenAI native).
	add("claude-", "claude-subscription", "claude-cli", "anthropic.")
	add("gpt-", "codex-subscription", "codex-cli")
	add("o3-", "codex-subscription", "codex-cli")
	add("o4-", "codex-subscription", "codex-cli")
	add("codex", "codex-subscription", "codex-cli")
	// Vertex prefixes.
	add("gemini-", "vertex")
	add("google/", "vertex")
	// Bedrock publisher prefixes. The Bedrock model-ID convention is
	// "<publisher>.<model>-<version>:<inference-profile>" — e.g.
	// "openai.gpt-oss-120b-1:0", "anthropic.claude-sonnet-4-5",
	// "amazon.nova-pro-v1:0". Every publisher Bedrock supports gets
	// a default route so fresh installs don't trip on the "Malformed
	// publisher model" 400 that Vertex returns for cross-namespace
	// model IDs (observed 2026-05-28 on companion-rag-ingest task
	// where sparse routes sent openai.gpt-oss-120b-1:0 to Vertex).
	add("amazon.", "bedrock")
	add("anthropic.", "bedrock")
	add("cohere.", "bedrock")
	add("deepseek.", "bedrock")
	add("global.", "bedrock") // cross-region inference profiles
	add("meta.", "bedrock")
	add("minimax.", "bedrock")
	add("mistral.", "bedrock")
	add("moonshot.", "bedrock")
	add("moonshotai.", "bedrock")
	add("nvidia.", "bedrock")
	add("openai.", "bedrock")
	add("qwen.", "bedrock")
	add("stability.", "bedrock")
	add("writer.", "bedrock")
	add("zai.", "bedrock")
	return out
}

// mergeWithDefaultRoutes returns user routes first, then appends
// every default route whose prefix isn't already covered by a user
// route. First-match-wins semantics in the router mean user routes
// take precedence on duplicate prefixes; defaults fill the gaps.
//
// Kept as a free function so the merge logic is testable without
// booting the whole chat sub-system.
func mergeWithDefaultRoutes(user, defaults []config.ChatRouteConfig) []config.ChatRouteConfig {
	// Key on the (prefix, suffix) pair, not prefix alone: suffix routes
	// (OpenRouter's ":free") all carry Prefix=="" and would collide with
	// a legacy empty-prefix catch-all under a prefix-only key, dropping
	// one of them. The NUL separator can't appear in a model string so
	// the composite key is unambiguous.
	key := func(r config.ChatRouteConfig) string { return r.Prefix + "\x00" + r.Suffix }
	seen := make(map[string]struct{}, len(user))
	for _, r := range user {
		seen[key(r)] = struct{}{}
	}
	merged := make([]config.ChatRouteConfig, 0, len(user)+len(defaults))
	merged = append(merged, user...)
	for _, d := range defaults {
		if _, ok := seen[key(d)]; ok {
			continue
		}
		merged = append(merged, d)
	}
	return merged
}

// buildVertexEndpoint returns the OpenAI-compat Vertex AI URL for a given
// project + location. The trailing `/chat/completions` is stripped inside
// chat.NewClient (via normalizeEndpoint), so we build only the base prefix.
//
// Location hostname rules:
//   - "" or "global" → `aiplatform.googleapis.com` (the location path
//     segment is still "global" — the global endpoint is not regionless).
//   - anything else  → `<location>-aiplatform.googleapis.com` (regional).
//
// Operators who point at an internal proxy or preview endpoint set
// chat.router.vertex.endpoint directly and skip this helper.
func buildVertexEndpoint(projectID, location string) string {
	if location == "" {
		location = "global"
	}
	host := "aiplatform.googleapis.com"
	if location != "global" {
		host = location + "-aiplatform.googleapis.com"
	}
	return fmt.Sprintf("https://%s/v1/projects/%s/locations/%s/endpoints/openapi",
		host, projectID, location)
}

// defaultOpenRouterEndpoint is OpenRouter's OpenAI-compatibility base URL.
// normalizeEndpoint (inside chat.NewClient) tolerates the trailing path.
const defaultOpenRouterEndpoint = "https://openrouter.ai/api/v1"

// resolveOpenRouterEndpoint returns the operator's endpoint override or
// the baked-in OpenRouter default when unset.
func resolveOpenRouterEndpoint(endpoint string) string {
	if endpoint == "" {
		return defaultOpenRouterEndpoint
	}
	return endpoint
}

// openRouterAttributionHeaders builds the HTTP-Referer / X-Title pair
// OpenRouter uses for app attribution. Defaults identify vornik by name +
// build version (no website URL, per operator preference): referer
// "vornik/<version>", title "vornik". Operator config overrides either.
func openRouterAttributionHeaders(referer, title string) map[string]string {
	if referer == "" {
		referer = "vornik/" + version.Default
	}
	if title == "" {
		title = "vornik"
	}
	return map[string]string{
		"HTTP-Referer": referer,
		"X-Title":      title,
	}
}

func (c *Container) waitForChatProviderReady(ctx context.Context) {
	if c.ChatClient == nil {
		return
	}
	pg, ok := c.ChatClient.(chat.Pinger)
	if !ok {
		c.Logger.Debug().Msg("chat readiness gate: provider does not implement Pinger — skipping")
		return
	}
	const totalBudget = 30 * time.Second
	deadline := time.Now().Add(totalBudget)
	delay := 250 * time.Millisecond
	const maxDelay = 4 * time.Second
	attempt := 0
	for {
		attempt++
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := pg.Ping(probeCtx)
		cancel()
		if err == nil {
			c.Logger.Info().Int("attempt", attempt).Msg("chat readiness gate: provider ready")
			return
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			c.Logger.Warn().Err(err).Int("attempt", attempt).Dur("budget", totalBudget).
				Msg("chat readiness gate: provider not ready within budget — proceeding (infra-retry will catch first-call failures)")
			return
		}
		c.Logger.Debug().Err(err).Int("attempt", attempt).Dur("next_delay", delay).
			Msg("chat readiness gate: ping failed, will retry")
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay < maxDelay {
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
	}
}
