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
	"errors"
	"fmt"
	"maps"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/aidisclosure"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/conversation/a2a"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/mcpauth"
	"vornik.io/vornik/internal/mcpconnect"
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

// mcpReconnectBudget bounds the whole MCP reconcile initMCP kicks off. Since
// SyncProjects now dials concurrently (each server ≤30s), the batch normally
// finishes in ~one dial; this overall deadline is the backstop that guarantees
// the reload activator returns and releases the reloader's reloadMu even if
// several servers are offline. See the initMCP call site + the 2026-07-08
// watcher-wedge fix.
const mcpReconnectBudget = 35 * time.Second

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
	// Bound the whole reconnect. initMCP runs inside the config-reload
	// activator (SetActivator → initMCP), which holds the reloader's
	// reloadMu. SyncProjects dials each server serially, each dial bounded at
	// 30s; with one or more OFFLINE servers (e.g. pagedrop down) the serial
	// sum can hold reloadMu for minutes. An overall deadline caps that so the
	// reload cycle always returns and releases reloadMu — the watcher's
	// bounded TryReload then keeps the scan loop live either way. Root-cause
	// fix for the 2026-07-08 watcher wedge (offline MCP server stalled the
	// activator; the deferred initMCP-no-timeout follow-up from the
	// 2026-07-06 non-blocking-config-save design).
	ctx, cancel := context.WithTimeout(context.Background(), mcpReconnectBudget)
	defer cancel()
	c.mcpManager.SyncProjects(ctx, desired)
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
	daemonServers := indexMCPServersByName(c.daemonMCPServers())
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
			cfg := mcp.ServerConfig{
				Name:           s.Name,
				Transport:      s.Transport,
				Command:        s.Command,
				Args:           s.Args,
				Env:            s.Env,
				URL:            s.URL,
				AllowedTools:   s.AllowedTools,
				TimeoutSeconds: s.TimeoutSeconds,
				Headers:        brokerHeadersFor(p, s.Name),
				ToolRateLimits: toolLimits,
				ProjectID:      p.ID,
			}
			// Regression: template-bundles-v2 final review — name-only
			// subscription entries (tool-assistant template) were
			// silently skipped with `unsupported transport ""`. The
			// tool-assistant project.yaml.tmpl renders mcp.servers
			// entries as `- name: "<server>"` only, per the
			// name-presence subscription model documented in
			// internal/ui/project_config_form_mcp.go — the daemon must
			// supply the connection details for a project entry that
			// doesn't carry its own. When the project entry has no
			// transport of its own and its name matches a daemon-level
			// server (config.yaml's mcp.servers), inherit that
			// server's connection fields while keeping the project's
			// own AllowedTools narrowing. A project entry that already
			// declares its own transport stays verbatim (today's
			// behavior), and a name-only entry with no daemon match
			// falls through unchanged — the manager's existing
			// log-and-skip path handles it.
			auth := s.Auth
			grants := mcpauth.Grants{Allowed: p.Permissions.Secrets}
			// Scope the CREDENTIAL is resolved under. The project's own by
			// default; an inherited daemon credential overrides it below.
			authProjectID := mcpCredentialScope(p.ID, false)
			if s.Transport == "" {
				if daemon, ok := daemonServers[s.Name]; ok {
					cfg.Transport = daemon.Transport
					cfg.Command = daemon.Command
					cfg.Args = append([]string(nil), daemon.Args...)
					cfg.Env = maps.Clone(daemon.Env)
					cfg.URL = daemon.URL
					// A name-only entry means "use that daemon server", so
					// it inherits the daemon block's credentials along with
					// its connection fields. The project's own
					// permissions.secrets does NOT gate an inherited block:
					// the credential belongs to the daemon-scope server
					// definition, which is admin-configured and reachable
					// from every project by design (auth design §9).
					//
					// When BOTH sides declare auth the config is ambiguous, so
					// the server is withheld rather than one side silently
					// winning (review-20260804-350e finding 1). Picking either
					// would make the load-time grant check on the project's
					// block meaningless while leaving it able to fail the whole
					// project — an operator would be validating a block that is
					// then discarded.
					if !daemon.Auth.IsZero() {
						if !s.Auth.IsZero() {
							c.Logger.Error().
								Str("project", p.ID).
								Str("server", s.Name).
								Msg("MCP: refusing to register server — the project entry and the daemon-scope server both declare auth; give the project entry its own transport/url to own the credential, or drop its auth block to inherit the daemon's")
							continue
						}
						auth = daemon.Auth
						grants = mcpauth.Grants{Unrestricted: true}
						// The credential is inherited, so the GRANT it resolves
						// against is too — see mcpCredentialScope for why, and for
						// what breaks when this is skipped (it was, until
						// 2026-08-05).
						authProjectID = mcpCredentialScope(p.ID, true)
						c.Logger.Warn().
							Str("project", p.ID).
							Str("server", s.Name).
							Msg("MCP: project uses a daemon-scope server whose credentials are shared with every project")
					}
				}
			}
			// The label must name BOTH the project doing the work and the
			// scope its credential actually resolves at — a log line reading
			// "project X" for a grant stored at project_id "" sends an
			// operator looking up the wrong row (review-20260805-e814).
			scopeLabel := "project " + p.ID
			if authProjectID == "" {
				scopeLabel = "project " + p.ID + " (credential at daemon scope)"
			}
			if !c.applyMCPAuth(&cfg, auth, grants, authProjectID, scopeLabel) {
				continue
			}
			servers = append(servers, cfg)
		}
		desired[p.ID] = servers
	}
	return desired
}

// mcpCredentialScope returns the project_id an MCP server's OAuth grant is
// resolved under.
//
// The design states the property — "daemon-scope servers are available to all
// projects" (auth design §9) — and this is the mechanism. A grant is stored per
// (project_id, server_name); a daemon-scope server's grant lives at "". So a
// project that subscribes to a daemon server by NAME ONLY, and therefore
// inherits its credential, must resolve that credential at "" as well. One
// consent serves every project that does not override it, which is the whole
// point of the daemon scope.
//
// A project entry that declares its own transport/url owns its own credential
// and resolves at its own id.
//
// Getting this wrong is quiet: the lookup misses, the server registers
// UNAUTHENTICATED (§8 registers rather than withholds an unconnected oauth
// server), initialize 401s, and the project holds the server with ZERO tools
// while the daemon-scope registry reports it healthy. Observed 2026-08-05.
func mcpCredentialScope(projectID string, inheritedFromDaemon bool) string {
	if inheritedFromDaemon {
		return ""
	}
	return projectID
}

// applyMCPAuth resolves a server's `auth:` block into cfg.AuthHeaders /
// cfg.AuthEnv and reports whether the server may be registered.
//
// Three outcomes, and the difference between them is the whole point:
//
//   - resolved (or nothing to resolve) -> true, credentials attached.
//   - mode oauth -> true with no credentials, logged once. The config surface
//     ships before the flow (design §11 steps 3-5), so an operator can write
//     and validate the block today; the server stays usable meanwhile rather
//     than disappearing from every agent's tool catalog.
//   - anything else (an unresolvable secret, a block that reached here without
//     passing config validation) -> false. Fail closed: a server registered
//     without the credential it declares 401s on every call, which reads as a
//     vendor permissions problem rather than a local misconfiguration.
//
// A deliberate deviation from design §8, which specifies "config load error"
// for an unresolvable secret ref: presence of a secret is environment state,
// not config text, so checking it at load would make `config validate` pass or
// fail depending on which host ran it. Load-time validation stays syntactic and
// this is where presence is enforced — with an ERROR naming the secret, which
// is the diagnostic the operator needs either way.
func (c *Container) applyMCPAuth(cfg *mcp.ServerConfig, auth mcpauth.Auth, grants mcpauth.Grants, projectID, scope string) bool {
	if auth.IsZero() {
		return true
	}
	inj, err := mcpauth.Resolve(auth, cfg.Transport, mcpauth.EnvSecretSource{}, grants)
	switch {
	case errors.Is(err, mcpauth.ErrOAuthNotWired):
		// mode: oauth resolves from the token store rather than from config —
		// the credential is a grant a human gave, not a value an operator
		// wrote. Delegated to the connector, which refreshes as needed.
		return c.applyMCPOAuthToken(cfg, projectID, scope)
	case err != nil:
		// Never log the error's cause at a level that could carry a value:
		// mcpauth guarantees its messages name fields and secret NAMES only.
		c.Logger.Error().
			Err(err).
			Str("server", cfg.Name).
			Str("scope", scope).
			Msg("MCP: refusing to register server — its auth block could not be resolved")
		return false
	}
	cfg.AuthHeaders = inj.Headers
	cfg.AuthEnv = inj.Env
	return true
}

// daemonMCPServerConfigs converts the daemon-level MCP catalog (config.yaml's
// mcp.servers) into client configs with any `auth:` block resolved. Shared by
// the discovery registry so an authenticated daemon-scope server is probed WITH
// its credential — without it the catalog would report every such server
// unreachable.
func (c *Container) daemonMCPServerConfigs() []mcp.ServerConfig {
	declared := c.daemonMCPServers()
	servers := make([]mcp.ServerConfig, 0, len(declared))
	for _, s := range declared {
		cfg := mcp.ServerConfig{
			Name:           s.Name,
			Transport:      s.Transport,
			Command:        s.Command,
			Args:           s.Args,
			Env:            s.Env,
			URL:            s.URL,
			AllowedTools:   s.AllowedTools,
			TimeoutSeconds: s.TimeoutSeconds,
		}
		// Daemon-scope servers are admin-configured and have no project
		// allowlist to check against.
		if !c.applyMCPAuth(&cfg, s.Auth, mcpauth.Grants{Unrestricted: true}, "", "daemon") {
			continue
		}
		servers = append(servers, cfg)
	}
	return servers
}

// daemonMCPServers returns the CURRENT daemon-level MCP catalog: the value a
// reload published, or the boot config when no reload has happened yet.
//
// Every reader of the daemon catalog must come through here. Reading
// c.Config.MCP.Servers directly pins the caller to the boot value, because
// c.Config is never swapped on reload (see applyHotConfig) — that is exactly
// how an added server ended up invisible to the tool manager, the discovery
// registry and the connect path all at once.
func (c *Container) daemonMCPServers() []config.MCPServerConfig {
	if live := c.daemonMCPLive.Load(); live != nil {
		return *live
	}
	if c.Config == nil {
		return nil
	}
	return c.Config.MCP.Servers
}

// publishDaemonMCPServers makes a freshly-parsed catalog the live one. Called
// from the reload activator before the MCP subsystems reconcile against it.
func (c *Container) publishDaemonMCPServers(servers []config.MCPServerConfig) {
	snapshot := append([]config.MCPServerConfig(nil), servers...)
	c.daemonMCPLive.Store(&snapshot)
}

// publicOrigin returns the CURRENT public origin — the value a reload
// published, or the boot config's when no reload has happened yet.
//
// Goes through Config.PublicOrigin() rather than reading
// Server.PublicBaseURL, so the auth.external_base_url fallback is honoured
// here too; the connector used to read the narrower field directly.
func (c *Container) publicOrigin() string {
	if live := c.publicOriginLive.Load(); live != nil {
		return *live
	}
	if c.Config == nil {
		return ""
	}
	return c.Config.PublicOrigin()
}

// a2aBaseURLProvider keeps published A2A cards on the same canonical public
// origin as OAuth callbacks. The provider reads live so a config reload updates
// future discovery documents without a daemon restart.
func (c *Container) a2aBaseURLProvider() a2a.PublicBaseURLProvider {
	return a2a.PublicBaseURLFunc(c.publicOrigin)
}

// publishPublicOrigin makes a freshly-parsed origin the live one.
func (c *Container) publishPublicOrigin(origin string) {
	c.publicOriginLive.Store(&origin)
}

// daemonMCPServersByName indexes the daemon-level MCP server catalog
// (config.yaml's mcp.servers block) by name so mcpDesiredServers can
// look up connection details for a project entry that only supplies a
// name. Returns an empty map (not nil misuse) when cfg is nil or
// declares no daemon-level servers, so callers can index it
// unconditionally.
// indexMCPServersByName indexes a daemon catalog slice by server name.
//
// Takes the SLICE, not a *config.Config: the previous
// daemonMCPServersByName(cfg) shape is what made every caller reach for
// c.Config, which is pinned to boot and never swapped on reload. Callers now
// pass c.daemonMCPServers() so an added or removed server is seen live.
func indexMCPServersByName(servers []config.MCPServerConfig) map[string]config.MCPServerConfig {
	byName := make(map[string]config.MCPServerConfig, len(servers))
	for _, s := range servers {
		byName[s.Name] = s
	}
	return byName
}

// initMCPRegistry builds the daemon-level MCP discovery registry from
// config.MCP.Servers and runs an initial refresh so the surface comes
// up populated. Distinct from initMCP — the registry serves the
// discovery API (/api/v1/mcp/servers, /ui/mcp) only; it never grants
// tool access to projects. Empty / unset config block leaves
// c.mcpRegistry nil so handlers return an empty catalog.
func (c *Container) initMCPRegistry() {
	if len(c.daemonMCPServers()) == 0 {
		c.Logger.Debug().Msg("MCP daemon-level registry: no servers configured")
		return
	}
	servers := c.daemonMCPServerConfigs()
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

// refreshMCPRegistry re-points the daemon-level discovery registry at the
// newly-activated config and re-probes it. Called from the config-reload
// activator alongside initMCP.
//
// Handles the registry being nil at boot-time (no servers configured then) by
// building it on first use, so adding the very first MCP server through the
// hub works without a restart too — the case that would otherwise still look
// broken after this fix.
func (c *Container) refreshMCPRegistry() {
	servers := c.daemonMCPServerConfigs()
	if c.mcpRegistry == nil {
		if len(servers) == 0 {
			return
		}
		c.initMCPRegistry()
		return
	}
	c.mcpRegistry.SetServers(servers)
	// Re-probe off the reload path: RefreshAll dials every server, and the
	// reloader holds a lock that in-flight tool calls also want.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c.mcpRegistry.RefreshAll(ctx)
		c.Logger.Info().
			Int("servers", c.mcpRegistry.ServerCount()).
			Msg("MCP daemon-level registry refreshed after config reload")
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
		telegram.WithSkillRepository(c.repos.Skills),
		telegram.WithExecutionRepository(c.repos.Executions),
		telegram.WithArtifactRepository(c.repos.Artifacts),
		telegram.WithTaskCredentialRepository(c.repos.TaskCredentials),
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
	if c.Executor != nil {
		// Steering "Reject" taps cancel an approval task + cascade to its
		// children (reuses Executor.CancelChildren).
		botOpts = append(botOpts, telegram.WithChildCanceller(c.Executor))
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
		// Confidence floor for the fuzzy claim-refute path (0 → the
		// package default). See container_dispatcher.go / incident
		// 2026-07-31.
		corrector.MinRefuteScore = c.Config.Memory.MinRefuteScore
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

	// Say so at boot when the bot is reachable by anyone. A denial that
	// surprises the operator is bad; an OPEN bot that never announces itself
	// is worse, and until 2026-08-05 that was the silent default.
	if len(allowedUsers) == 0 {
		if c.Config.Telegram.AllowUnlistedUsers {
			c.Logger.Warn().
				Msg("telegram: allowed_users is empty and allow_unlisted_users is true — " +
					"ANY Telegram user who finds this bot can drive it. Intended for dev only.")
		} else {
			c.Logger.Warn().
				Msg("telegram: allowed_users is empty — every inbound message will be DENIED. " +
					"List the user IDs, or set telegram.allow_unlisted_users: true for dev.")
		}
	}

	bot, err := telegram.NewBot(
		telegram.BotConfig{
			Token:               cfg.BotToken,
			AllowedUsers:        allowedUsers,
			AllowUnlistedUsers:  c.Config.Telegram.AllowUnlistedUsers,
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

	// Wire the scraper-block → Telegram notify hook (design 2026-07-19). Inert
	// unless enabled with at least one curated portal. Runs on a daemon-lifetime
	// context; a portal/cooldown change needs a restart (MVP). Requires the MCP
	// manager (the hook lives at Manager.Execute) and the bot as the sink.
	if bnCfg := c.Config.Scraper.BlockNotify; bnCfg.Enabled && len(bnCfg.Portals) > 0 && c.mcpManager != nil {
		cooldown, _ := time.ParseDuration(bnCfg.Cooldown) // 0 on empty/invalid → defaults to 6h
		bn := mcp.NewBlockNotifier(
			bnCfg.Portals, cooldown, bot,
			c.Logger.With().Str("component", "scraper-block-notify").Logger(),
		)
		bn.Start(context.Background())
		c.mcpManager.SetBlockNotifier(bn)
		c.Logger.Info().
			Int("portals", len(bnCfg.Portals)).
			Str("cooldown", cooldown.String()).
			Msg("scraper block-notify: enabled")
	}
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

	// EU AI Act Art 50(1). Built here rather than lazily at each channel
	// so there is exactly one instance and one place a wiring mistake
	// could happen. Every dispatcher.ChannelReceiver takes it.
	c.AIDisclosure = aidisclosure.New(c.Config.AIDisclosure, c.repos.ChannelDisclosure)

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

// applyMCPOAuthToken attaches a stored OAuth grant's access token, refreshing it first when it is
// near expiry.
//
// Three outcomes, and the difference is what an operator can act on:
//
//   - a usable token -> attached as a bearer, exactly like mode: static.
//   - no grant yet, or one needing re-consent -> the server is REGISTERED without a credential.
//     Its calls will 401 at the vendor, which is honest and recoverable by `vornikctl mcp
//     connect`; withholding the server instead would make its tools vanish from every agent's
//     catalog and read as a missing integration rather than a missing consent.
//   - a real failure (the store is unreachable) -> withheld, as for any other unresolvable auth.
//
// Note the asymmetry with mode: static, where an unresolvable secret WITHHOLDS the server. There
// the credential is config an operator can fix in place; here it is a human consent that must be
// given through a browser, and hiding the server would hide the thing they need to connect.
func (c *Container) applyMCPOAuthToken(cfg *mcp.ServerConfig, projectID, scope string) bool {
	conn := c.mcpConnector()
	if conn == nil {
		c.Logger.Warn().
			Str("server", cfg.Name).
			Str("scope", scope).
			Msg("MCP: auth mode oauth is configured but no token store is wired — this server will connect unauthenticated")
		return true
	}
	ref, ok := c.mcpServerRef(projectID, cfg.Name)
	if !ok {
		// Should not happen: the caller just read this server out of the same
		// config. Register unauthenticated rather than dropping it.
		c.Logger.Warn().Str("server", cfg.Name).Str("scope", scope).
			Msg("MCP: could not resolve the server for OAuth injection — connecting unauthenticated")
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), mcpOAuthHTTPTimeout)
	defer cancel()
	token, err := conn.AccessToken(ctx, ref)
	switch {
	case errors.Is(err, mcpconnect.ErrNeedsReconnect):
		c.Logger.Warn().
			Str("server", cfg.Name).
			Str("scope", scope).
			Msg("MCP: the stored OAuth grant needs an operator reconnect — connecting unauthenticated (run: vornikctl mcp connect)")
		return true
	case err != nil:
		c.Logger.Error().Err(err).
			Str("server", cfg.Name).
			Str("scope", scope).
			Msg("MCP: refusing to register server — its OAuth grant could not be read")
		return false
	}
	if token == "" {
		c.Logger.Info().
			Str("server", cfg.Name).
			Str("scope", scope).
			Msg("MCP: auth mode oauth is configured but this server is not connected yet — connecting unauthenticated (run: vornikctl mcp connect)")
		return true
	}
	cfg.AuthHeaders = map[string]string{"Authorization": "Bearer " + token}
	return true
}

// roleToolCeiling resolves the ceiling (role allowedTools) for a task, for the
// grant_step_tools provider to validate a grant against.
//
// Returns nil at every resolution gap, which the provider treats as "unrestricted" —
// the same fail-open rule the advertise filter and the invoke gate use. A grant
// against a nil ceiling can still only narrow, so the invariant holds.
func (c *Container) roleToolCeiling(ctx context.Context, taskID string) (string, []string) {
	if c == nil || c.repos == nil || c.repos.Executions == nil || c.Registry == nil {
		return "", nil
	}
	exec, err := c.repos.Executions.GetByTaskID(ctx, taskID)
	if err != nil || exec == nil || exec.CurrentStepID == nil || *exec.CurrentStepID == "" {
		return "", nil
	}
	_, workflow, err := c.Registry.GetProjectWithWorkflow(exec.ProjectID)
	if err != nil || workflow == nil {
		return "", nil
	}
	step, ok := workflow.Steps[*exec.CurrentStepID]
	if !ok || step.Role == "" {
		return "", nil
	}
	_, swarm, err := c.Registry.GetProjectWithSwarm(exec.ProjectID)
	if err != nil || swarm == nil {
		return "", nil
	}
	for i := range swarm.Roles {
		if swarm.Roles[i].Name == step.Role {
			return step.Role, swarm.Roles[i].Permissions.AllowedTools
		}
	}
	return "", nil
}
