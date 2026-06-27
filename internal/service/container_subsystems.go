package service

// MCP + Telegram + database + logger wiring extracted from
// container.go as part of the 2026-05-16 service-package split.
// Owns:
//   - initMCP            (per-project MCP server connections)
//   - brokerHeadersFor   (per-server HTTP header builder for MCP)
//   - initTelegram       (Telegram bot init + dispatcher wiring)
//   - initLogger         (zerolog setup)
//   - initDatabase       (Postgres pool + migration runner)
//   - collectDBMetrics   (periodic pool-stats publisher)

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/storage"
	"vornik.io/vornik/internal/telegram"
)

// initMCP connects to MCP servers declared by each project, scoping
// clients per project so the same server name (e.g. "gmail") can appear
// in multiple projects with different credentials without colliding.
//
// Reuses an existing Manager across config reloads so the API server and
// dispatcher (which keep a pointer at init time) don't end up talking
// to a stale object.
func (c *Container) initMCP() {
	if c.Registry == nil {
		return
	}

	if c.mcpManager == nil {
		c.mcpManager = mcp.NewManager(c.Logger.With().Str("component", "mcp").Logger())
	}

	// Reconcile to the registry's current declarations via the
	// build-then-swap SyncProjects primitive. On startup this is
	// equivalent to the old per-project StartForProject loop; on a
	// config reload it ALSO drops projects that no longer declare
	// servers, and — crucially — it never leaves the catalog empty
	// while the re-dials are in flight. The previous reload pattern
	// (Manager.Close() then re-init) failed every in-flight and
	// incoming tool call for the duration of the reconnects (bug-sweep
	// follow-up 2026-06-04).
	desired := c.mcpDesiredServers()
	c.mcpManager.SyncProjects(context.Background(), desired)
	if len(desired) == 0 {
		// No project needs MCP. Leave the Manager in place but empty;
		// the API server's mcpExecutor pointer remains valid and every
		// Tools()/Execute() call returns the empty/not-found path
		// cleanly.
		c.Logger.Info().Msg("MCP: no projects declare servers")
		return
	}

	c.Logger.Info().
		Int("servers", c.mcpManager.ServerCount()).
		Int("projects", c.mcpManager.ProjectCount()).
		Msg("MCP servers connected")
}

// mcpDesiredServers assembles the per-project MCP server configs the
// registry currently declares — the desired state SyncProjects
// reconciles the live manager against.
func (c *Container) mcpDesiredServers() map[string][]mcp.ServerConfig {
	desired := make(map[string][]mcp.ServerConfig)
	for _, p := range c.Registry.ListProjects() {
		if len(p.MCP.Servers) == 0 {
			continue
		}
		// Per-project throttle map gets converted from the registry
		// shape to the mcp shape once per project (the registry must
		// not import the mcp package and vice versa — keeps the
		// dependency arrow pointing only one way). Empty map stays
		// empty so the client's NewToolRateLimiter returns nil and
		// the throttle gate is zero-cost.
		var toolLimits map[string]mcp.ToolRateLimitSpec
		if len(p.MCP.ToolRateLimits) > 0 {
			toolLimits = make(map[string]mcp.ToolRateLimitSpec, len(p.MCP.ToolRateLimits))
			for name, spec := range p.MCP.ToolRateLimits {
				toolLimits[name] = mcp.ToolRateLimitSpec{
					RPS:   spec.RPS,
					Burst: spec.Burst,
				}
			}
		}
		servers := make([]mcp.ServerConfig, 0, len(p.MCP.Servers))
		for _, s := range p.MCP.Servers {
			servers = append(servers, mcp.ServerConfig{
				Name:           s.Name,
				Transport:      s.Transport,
				Command:        s.Command,
				Args:           s.Args,
				Env:            s.Env,
				URL:            s.URL,
				AllowedTools:   s.AllowedTools,
				Headers:        brokerHeadersFor(p, s.Name),
				ToolRateLimits: toolLimits,
				ProjectID:      p.ID,
			})
		}
		desired[p.ID] = servers
	}
	return desired
}

// initMCPRegistry builds the daemon-level MCP discovery registry from
// config.MCP.Servers and runs an initial refresh so the surface comes
// up populated. Distinct from initMCP — the registry serves the
// discovery API (/api/v1/mcp/servers, /ui/mcp) only; it never grants
// tool access to projects. Empty / unset config block leaves
// c.mcpRegistry nil so handlers return an empty catalog.
func (c *Container) initMCPRegistry() {
	if len(c.Config.MCP.Servers) == 0 {
		c.Logger.Debug().Msg("MCP daemon-level registry: no servers configured")
		return
	}
	servers := make([]mcp.ServerConfig, 0, len(c.Config.MCP.Servers))
	for _, s := range c.Config.MCP.Servers {
		servers = append(servers, mcp.ServerConfig{
			Name:         s.Name,
			Transport:    s.Transport,
			Command:      s.Command,
			Args:         s.Args,
			Env:          s.Env,
			URL:          s.URL,
			AllowedTools: s.AllowedTools,
		})
	}
	c.mcpRegistry = mcp.NewRegistry(servers, 0,
		c.Logger.With().Str("component", "mcp-registry").Logger())

	// Run the first refresh in a goroutine bounded by 30s so
	// daemon startup isn't gated on a slow MCP server reaching out.
	// Snapshot() will return the pre-seeded "not yet refreshed"
	// rows in the meantime, so the API/UI surface is always live.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c.mcpRegistry.RefreshAll(ctx)
		c.Logger.Info().
			Int("servers", c.mcpRegistry.ServerCount()).
			Msg("MCP daemon-level registry initial refresh complete")
	}()
}

// brokerHeadersFor returns the per-server HTTP headers the daemon
// must attach to JSON-RPC calls bound for that MCP server. Today
// only the well-known "broker" server gets any — X-Project-ID
// (the broker refuses place_order without it) and X-Project-Caps
// (a JSON envelope of the project's per-call cap overlay so the
// safety envelope can scope position / turnover / rate limits to
// what the operator wrote in the project YAML, instead of the
// broker-wide env-var fallback). Returns nil for any other server
// name so non-broker MCPs see no daemon-injected headers.
func brokerHeadersFor(p *registry.Project, serverName string) map[string]string {
	if serverName != "broker" || p == nil {
		return nil
	}
	caps := struct {
		MaxPositionUSD             float64 `json:"max_position_usd"`
		MaxDailyTurnoverUSD        float64 `json:"max_daily_turnover_usd"`
		MaxOrdersPerHour           int     `json:"max_orders_per_hour"`
		MaxOrdersPerMinute         int     `json:"max_orders_per_minute"`
		DrawdownCircuitBreakerPct  float64 `json:"drawdown_circuit_breaker_pct"`
		DailyLossCircuitBreakerPct float64 `json:"daily_loss_circuit_breaker_pct"` // audit T4
		KillSwitch                 bool    `json:"kill_switch"`
		Mode                       string  `json:"mode"` // audit T1 defence-in-depth
	}{
		MaxPositionUSD:             p.Trading.Caps.MaxPositionUSD,
		MaxDailyTurnoverUSD:        p.Trading.Caps.MaxDailyTurnoverUSD,
		MaxOrdersPerHour:           p.Trading.Caps.MaxOrdersPerHour,
		MaxOrdersPerMinute:         p.Trading.Caps.MaxOrdersPerMinute,
		DrawdownCircuitBreakerPct:  p.Trading.Caps.DrawdownCircuitBreakerPct,
		DailyLossCircuitBreakerPct: p.Trading.Caps.DailyLossCircuitBreakerPct,
		KillSwitch:                 p.Trading.KillSwitch,
		Mode:                       p.Trading.Mode,
	}
	headers := map[string]string{"X-Project-ID": p.ID}
	encoded, err := json.Marshal(caps)
	if err == nil {
		headers["X-Project-Caps"] = string(encoded)
	} else {
		// Marshal of a fixed struct shape can't fail in practice. If
		// it ever did, we deliberately do NOT attach an overlay — the
		// broker now fails closed on a missing X-Project-Caps (audit
		// T3) rather than falling back to unlimited env caps, so a
		// project-id-only header refuses orders instead of trading
		// uncapped.
		_ = encoded
	}
	// Audit T2: authenticate the daemon→broker channel with the
	// shared secret when configured (symmetric with the broker's
	// VORNIK_BROKER_INTERNAL_KEY). Empty → no header, and the broker
	// logs that its order surface is unauthenticated.
	if key := strings.TrimSpace(os.Getenv("VORNIK_BROKER_INTERNAL_KEY")); key != "" {
		headers["Authorization"] = "Bearer " + key
	}
	return headers
}

// initTelegram initializes the Telegram bot.
func (c *Container) initTelegram() error {
	cfg := c.Config.Telegram
	if !cfg.Enabled {
		c.Logger.Debug().Msg("telegram bot disabled in config")
		return nil
	}

	if cfg.BotToken == "" {
		return fmt.Errorf("telegram bot token not configured")
	}

	if c.ChatClient == nil {
		return fmt.Errorf("chat client required for telegram bot")
	}

	// Convert string user IDs to int64 and copy the per-user project
	// scope across. Config-level validation has already enforced the
	// polymorphic YAML shape (see config.UserAccess.UnmarshalYAML).
	allowedUsers := make(map[int64]telegram.UserAccess, len(cfg.AllowedUsers))
	for userIDStr, ua := range cfg.AllowedUsers {
		userID, err := strconv.ParseInt(userIDStr, 10, 64)
		if err != nil {
			c.Logger.Warn().
				Str("user_id_raw", userIDStr).
				Err(err).
				Msg("telegram allowed_users: skipping non-numeric user ID")
			continue
		}
		allowedUsers[userID] = telegram.UserAccess{
			Allowed:  ua.Allowed,
			Projects: ua.Projects,
		}
	}

	botOpts := []telegram.BotOption{
		telegram.WithLogger(c.Logger),
		telegram.WithTaskRepository(c.repos.Tasks),
		telegram.WithExecutionRepository(c.repos.Executions),
		telegram.WithArtifactRepository(c.repos.Artifacts),
		telegram.WithRegistry(c.Registry),
		telegram.WithTaskWatcherRepository(c.repos.Watchers),
	}
	if c.Config.Runtime.ProjectWorkspacePath != "" {
		botOpts = append(botOpts, telegram.WithProjectWorkspacePath(c.Config.Runtime.ProjectWorkspacePath))
	}
	// Read-path conversation compaction: when enabled, overflow turns are
	// condensed into a deterministic topic gist instead of being dropped
	// (fixes silent context loss on long sessions). Off → legacy truncation.
	if c.Config.Chat.Compaction.Enabled {
		botOpts = append(botOpts, telegram.WithCompactor(memory.NewChatGist(c.Config.Chat.Compaction.MaxGistTerms)))
	}
	if c.mcpManager != nil {
		botOpts = append(botOpts, telegram.WithMCPManager(c.mcpManager))
	}
	botOpts = append(botOpts, telegram.WithAuditRepository(c.repos.ToolAudit))
	botOpts = append(botOpts, telegram.WithLLMUsageRepository(c.repos.LLMUsage))
	// Two-tier intent judge: the heuristic tier runs sync on
	// every dispatcher tool call; the async LLM refiner re-
	// evaluates medium+ risk verdicts. Both verdicts persist
	// to intent_verdicts for calibration. The refiner uses the
	// dispatcher's chat router with the model id pinned below
	// (empty leaves the router's default in place).
	botOpts = append(botOpts, telegram.WithIntentJudgeRepository(
		c.repos.IntentVerdicts,
		c.Config.Intentjudge.RefinerModel,
	))
	// Phase 28 — conversational task lifecycle. Per-task reply
	// routing + /inbox command. nil-safe.
	botOpts = append(botOpts, telegram.WithTaskMessageRepository(c.repos.Messages))
	// DB-backed telegram session store (horizontal-scaling
	// follow-on). When wired, every post-turn write persists
	// conversation history + active project to channel_sessions,
	// and the bot rehydrates each chat on the first inbound
	// after a daemon restart / replica failover. Nil persister
	// (SQLite single-process or unwired repo) preserves the
	// pre-feature in-memory-only behaviour.
	if p := c.channelSessionPersister("telegram"); p != nil {
		botOpts = append(botOpts, telegram.WithSessionPersister(p))
	}
	// Cluster gate (2026.8.0 horizontal-scaling follow-on): only
	// the elected leader calls Telegram getUpdates so two replicas
	// can't both consume the same update and double-reply the
	// user. Nil elector (single-process default) leaves the loop
	// running on every daemon. The matching offset persistence
	// closes the failover-window replay (next block).
	c.telegramPollerElector = c.initWorkerElector("telegram_poller")
	if c.telegramPollerElector != nil {
		botOpts = append(botOpts, telegram.WithLeaderGate(c.telegramPollerElector))
	}
	if c.repos != nil && c.repos.TelegramPollerState != nil {
		// pollerBotID is the key into telegram_poller_state.
		// Token's hex prefix is stable per BotFather identity and
		// already unique across deployments; using it avoids an
		// extra getMe round-trip at boot. Single-bot deployments
		// see one row in the table; multi-bot deployments get one
		// per token automatically.
		botID := telegramPollerBotID(c.Config.Telegram.BotToken)
		if botID != "" {
			botOpts = append(botOpts, telegram.WithPollerStateRepository(c.repos.TelegramPollerState, botID))
		}
	}
	if c.Scheduler != nil {
		botOpts = append(botOpts, telegram.WithRescheduler(c.Scheduler))
	}
	// Operator-profile cross-channel linking (/link slash command).
	// All three repos are required; missing any disables the
	// command with a clear operator message rather than 500ing.
	if c.repos != nil && c.repos.OperatorProfiles != nil && c.repos.OperatorIdentityLinks != nil {
		botOpts = append(botOpts, telegram.WithOperatorLinkRepositories(
			c.repos.OperatorProfiles,
			c.repos.OperatorIdentityLinks,
			c.repos.ProfileUseAudit,
		))
	}
	// Phase 29 — Telegram Forum Topics. One topic per task in the
	// configured supergroup so lifecycle events fan out to a
	// dedicated thread and operator replies route via
	// message_thread_id. Disabled when forum_chat_id == 0.
	if c.Config.Telegram.ForumChatID != 0 {
		botOpts = append(botOpts,
			telegram.WithForumChatID(c.Config.Telegram.ForumChatID, c.Config.Telegram.ForumTopicIconColor),
			telegram.WithTelegramThreadRepository(c.repos.TelegramThreads),
		)
	}
	if c.pricingTable != nil {
		botOpts = append(botOpts, telegram.WithPricing(c.pricingTable))
	}
	botOpts = append(botOpts, telegram.WithRateLimiter(c.rateLimiter))
	if globalModel := c.Config.Runtime.AgentLLM.Model; globalModel != "" {
		botOpts = append(botOpts, telegram.WithDefaultModel(globalModel))
	}
	if c.memoryManager != nil {
		// Give the dispatcher direct RAG access so it can answer user
		// questions from project memory instead of scheduling a research
		// task for every topic that's already been worked on.
		botOpts = append(botOpts, telegram.WithMemorySearcher(c.memoryManager.Searcher))
		// Memory corrector: the dispatcher's memory_correct tool lets
		// the LLM refute wrong facts in the corpus when the user
		// corrects them mid-conversation. Adds a verified-correction
		// chunk so future retrievals pick up the right fact.
		corrector := memory.NewCorrector(c.memoryManager.Repository(), c.memoryManager.Searcher)
		botOpts = append(botOpts, telegram.WithMemoryCorrector(corrector))
	}
	if c.artifactStore != nil {
		// Snapshot Telegram-uploaded inputs into the artifact store
		// when create_task is called. The task payload then references
		// the artifact storage path, so retries survive /tmp reaping
		// and workspace cleanup.
		botOpts = append(botOpts, telegram.WithArtifactStore(c.artifactStore))
	}
	if c.voiceSTT != nil || c.voiceTTS != nil {
		// Voice round-trip: inbound voice attachments get transcribed
		// before reaching HandleMessage; outbound replies route through
		// sendVoice when the chat's most-recent inbound was voice. Either
		// provider may be nil — the option is nil-safe per direction.
		botOpts = append(botOpts, telegram.WithVoiceProviders(telegram.VoiceProviders{
			STT: c.voiceSTT,
			TTS: c.voiceTTS,
		}))
	}

	var sessionTTL time.Duration
	if cfg.SessionTTL != "" {
		if d, err := time.ParseDuration(cfg.SessionTTL); err == nil {
			sessionTTL = d
		} else {
			c.Logger.Warn().Err(err).Str("value", cfg.SessionTTL).Msg("invalid session_ttl — TTL disabled")
		}
	}

	// Default MaxHistoryTokens to 70% of context_size when not explicitly set,
	// leaving headroom for the system prompt, tool catalog, and model response.
	// 70% was chosen because typical dispatcher system prompts with a ~5-role
	// swarm plus tools consume roughly 15-20% of context; the remainder is
	// response + reasoning tokens. Operators who want strict behavior can set
	// max_history_tokens explicitly; set to -1 to disable token-aware trim.
	maxHistoryTokens := c.Config.Chat.MaxHistoryTokens
	if maxHistoryTokens == 0 && c.Config.Chat.ContextSize > 0 {
		maxHistoryTokens = c.Config.Chat.ContextSize * 70 / 100
	} else if maxHistoryTokens < 0 {
		maxHistoryTokens = 0
	}

	// Dispatcher iteration cap: prefer the Telegram-specific
	// override (telegram.dispatcher_max_iterations) when set,
	// otherwise fall back to the chat-wide one. Either lets an
	// operator tune the bot's tool-call loop independently of
	// the dispatcher's compiled-in 10 default.
	maxToolIters := c.Config.Telegram.DispatcherMaxIterations
	if maxToolIters == 0 {
		maxToolIters = c.Config.Chat.MaxToolIterations
	}

	bot, err := telegram.NewBot(
		telegram.BotConfig{
			Token:               cfg.BotToken,
			AllowedUsers:        allowedUsers,
			RateLimit:           cfg.RateLimit,
			MaxHistory:          c.Config.Chat.MaxHistory,
			MaxHistoryTokens:    maxHistoryTokens,
			MaxToolIterations:   maxToolIters,
			SessionPath:         cfg.SessionPath,
			SessionTTL:          sessionTTL,
			DispatchTimeout:     resolveDispatchTimeout(c.Config.Chat.DispatchTimeout, c.Config.Chat.Timeout),
			DispatcherProjectID: c.Config.Telegram.DispatcherProjectID,
			WebUIBaseURL:        c.Config.Telegram.WebUIBaseURL,
		},
		c.ChatClient,
		botOpts...,
	)
	if err != nil {
		return fmt.Errorf("failed to create telegram bot: %w", err)
	}

	c.TelegramBot = bot
	return nil
}

// initLogger initializes structured JSON logging using zerolog.
// telegramPollerBotID derives a stable identifier for the
// telegram_poller_state row from the bot token. We use a short
// prefix of the token rather than the full token so an operator
// inspecting the table doesn't see the secret in plaintext, and
// the prefix is still unique per BotFather identity (Telegram
// tokens encode the bot's numeric ID in the first segment). Empty
// token → empty botID → persistence disabled.
func telegramPollerBotID(token string) string {
	if token == "" {
		return ""
	}
	// Telegram tokens look like "12345:ABC...". The numeric
	// part before the colon is the bot's user ID — stable per
	// bot. If for any reason the token doesn't contain a colon,
	// fall back to a length-bounded hex prefix; never embed the
	// secret half.
	for i, r := range token {
		if r == ':' {
			return "tg:" + token[:i]
		}
		if i >= 16 {
			break
		}
	}
	if len(token) > 8 {
		return "tg:" + token[:8]
	}
	return "tg:" + token
}

func (c *Container) initLogger() {
	// Configure log level
	level := zerolog.InfoLevel
	switch c.Config.Logging.Level {
	case "debug":
		level = zerolog.DebugLevel
	case "warn":
		level = zerolog.WarnLevel
	case "error":
		level = zerolog.ErrorLevel
	}

	// Create logger with JSON output
	c.Logger = zerolog.New(os.Stdout).With().
		Timestamp().
		Logger().
		Level(level)
}

// initDatabase initializes the database connection.
func (c *Container) initDatabase() error {
	driver := c.Config.Database.Driver
	if driver == "" {
		driver = "postgres"
	}
	c.Logger.Info().
		Str("driver", driver).
		Str("host", c.Config.Database.Host).
		Int("port", c.Config.Database.Port).
		Str("database", c.Config.Database.Name).
		Msg("opening database backend")

	backend, err := storage.Open(context.Background(), c.Config.Database)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}

	// Run pending migrations. Historical behaviour: log and continue
	// on migration failure rather than fail boot — the existing
	// schema is usually still serviceable.
	if err := backend.Migrate(context.Background()); err != nil {
		c.Logger.Warn().Err(err).Msg("database migration failed (continuing with existing schema)")
	}

	// Verify connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if backend.PG != nil {
		if err := backend.PG.PingContext(ctx); err != nil {
			return fmt.Errorf("failed to ping database: %w", err)
		}
	}

	c.backend = backend
	c.DB = backend.DB

	// Initialize DB metrics if observability registry is available
	if registry := c.observabilityRegistry(); registry != nil {
		c.dbMetrics = persistence.NewDBMetrics(registry)
		// Start periodic stats collection in background
		go c.collectDBMetrics()
	}

	// Build the repository surface. The Postgres factory wraps the
	// shared DBTX with metrics; the SQLite backend already wired its
	// own repos in storage.openSQLite, so reuse those directly (no
	// metrics wrapping for sqlite — single-file embedded DB, pool
	// stats don't apply).
	if backend.Driver == "sqlite" {
		c.repos = backend.Repos
	} else {
		c.repos = storage.Build(c.instrumentedDB())
	}

	return nil
}

// collectDBMetrics periodically records database connection pool metrics.
// This goroutine may be started during init before Run() sets collectorsCtx,
// so we re-read the context pointer on every iteration. A nil done channel
// in a select blocks forever, which is correct while the context is not yet set.
func (c *Container) collectDBMetrics() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		var done <-chan struct{}
		if ctx := c.collectorsCtx; ctx != nil {
			done = ctx.Done()
		}

		select {
		case <-ticker.C:
			if c.DB == nil || c.dbMetrics == nil {
				continue
			}
			c.dbMetrics.RecordPoolStats(c.Config.Database.Name, c.DB.Stats())
		case <-done:
			return
		}
	}
}
