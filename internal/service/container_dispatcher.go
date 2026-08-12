package service

import (
	"context"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/hallucination"
	"vornik.io/vornik/internal/intentjudge"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/memory/graph"
	"vornik.io/vornik/internal/memory/ned"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/registry"
)

// initDispatcher constructs the daemon's shared dispatcher.Agent
// from container-level dependencies and stores it on c.Dispatcher.
// All inbound channels (Telegram, GitHub App, future Slack / email)
// run through this one agent so retry budgets, intent-judge
// verdicts, memory tooling, output guard, and observability state
// stay coherent across surfaces.
//
// The build is conditional on a chat provider being wired: a
// deployment with chat disabled has no dispatcher and the channels
// fall back to inbound-only behaviour. Telegram-specific
// callbacks (FollowupRegistrar, BudgetNotifier, TaskWatchFunc)
// only fire when c.TelegramBot is non-nil, so GitHub-only
// deployments construct cleanly without forcing a stub bot.
//
// Must be called AFTER initTelegram (so the bot reference is
// resolved either way) and BEFORE Bot.Start (so the
// ConversationChannel receiver is wired before the poll loop
// starts handing messages to HandleMessage).
func (c *Container) initDispatcher() {
	if c.ChatClient == nil {
		c.Logger.Debug().Msg("dispatcher init skipped: chat client not configured")
		return
	}

	opts := []dispatcher.AgentOption{
		dispatcher.WithLogger(c.Logger.With().Str("component", "dispatcher").Logger()),
	}
	if c.Config.Chat.DeferredToolThreshold != 0 {
		opts = append(opts, dispatcher.WithDeferredToolThreshold(c.Config.Chat.DeferredToolThreshold))
	}

	// Tool-iteration cap: Telegram-specific override wins, then the
	// chat-wide setting. Mirrors the precedence in initTelegram so
	// behaviour doesn't drift between the two sites.
	maxIter := c.Config.Telegram.DispatcherMaxIterations
	if maxIter == 0 {
		maxIter = c.Config.Chat.MaxToolIterations
	}
	if maxIter > 0 {
		opts = append(opts, dispatcher.WithMaxIterations(maxIter))
	}

	// Bot-specific callbacks. Each is wired only when the bot is
	// present; without a Telegram bot the dispatcher still runs (the
	// GitHub @vornik reply flow doesn't need any of these — replies
	// are returned synchronously, there's no auto-resume to register,
	// and budget alerts land in the daemon log instead of a chat).
	if c.TelegramBot != nil {
		if c.repos != nil && c.repos.Watchers != nil {
			opts = append(opts, dispatcher.WithTaskWatchFunc(c.TelegramBot.WatchTask))
		}
		opts = append(opts, dispatcher.WithFollowupRegistrar(c.TelegramBot))
		opts = append(opts, dispatcher.WithBudgetNotifier(c.TelegramBot))
	}

	// Defensive primitives: hallucination check + output guard always on.
	opts = append(opts, dispatcher.WithHallucinationDetector(hallucination.NewDefault()))
	opts = append(opts, dispatcher.WithOutputGuard(true))

	// Two-tier intent judge: heuristic (sync) + LLM refiner (async)
	// gated by the IntentVerdicts repo. The refiner uses the same
	// chat provider; the model id pin defaults to whatever
	// intentjudge.refiner_model is configured to.
	if c.repos != nil && c.repos.IntentVerdicts != nil {
		refiner := &intentjudge.LLMRefiner{
			Provider:       c.ChatClient,
			Model:          c.Config.Intentjudge.RefinerModel,
			TimeoutSeconds: 15,
		}
		// Phase E expansion: thread the shared response cache so
		// repeated (tool, args, heuristic) triples skip the
		// upstream LLM call. Nil-safe — when the memory manager
		// or its cache isn't wired the refiner behaves exactly
		// as before.
		if c.memoryManager != nil && c.memoryManager.ResponseCache != nil {
			refiner.Cache = c.memoryManager.ResponseCache
		}
		opts = append(opts, dispatcher.WithIntentJudge(c.repos.IntentVerdicts, refiner, intentjudge.RiskMedium))
	}

	// Repository wiring — every option below is nil-tolerant: a
	// deployment missing the underlying table degrades the feature
	// rather than failing boot.
	if c.repos != nil && c.repos.Tasks != nil {
		opts = append(opts, dispatcher.WithGroundingTaskRepo(c.repos.Tasks))
	}
	// Give the chat agent the same MCP surface plus A2A consult tools. When
	// peers are configured, wrap the manager in a ComposedMCPExecutor (which
	// satisfies dispatcher.MCPExecutor structurally); otherwise pass the raw
	// manager unchanged.
	if cp := c.buildConsultProvider(); cp != nil {
		composed := &api.ComposedMCPExecutor{Consult: cp}
		if c.mcpManager != nil {
			composed.External = c.mcpManager
		}
		opts = append(opts, dispatcher.WithMCPManager(composed))
	} else if c.mcpManager != nil {
		opts = append(opts, dispatcher.WithMCPManager(c.mcpManager))
	}
	if c.repos != nil && c.repos.ToolAudit != nil {
		opts = append(opts, dispatcher.WithAuditRepository(c.repos.ToolAudit))
	}
	if c.repos != nil && c.repos.ChatAudit != nil {
		opts = append(opts, dispatcher.WithChatAuditRepo(c.repos.ChatAudit))
	}
	if c.repos != nil && c.repos.LLMUsage != nil {
		// Two wirings, two jobs: the repo for budget READS in the tool executor,
		// the recorder for BILLING chat turns.
		opts = append(opts, dispatcher.WithLLMUsageRepository(c.repos.LLMUsage))
		opts = append(opts, dispatcher.WithSpend(
			c.llmSpend(persistence.TaskLLMUsageSourceDispatcher, "dispatcher")))
	}
	if c.repos != nil && c.repos.BudgetReservations != nil {
		opts = append(opts, dispatcher.WithBudgetReservationRepository(c.repos.BudgetReservations))
	}
	if c.repos != nil && c.repos.OperatorProfiles != nil {
		// Read-path-first slice (roadmapped). Dispatcher fetches
		// per-operator profile + injects an <operator_profile>
		// block in the system prompt on every turn. Nil-safe;
		// SQLite stub returns ErrNotFound so the block is skipped.
		opts = append(opts, dispatcher.WithOperatorProfileRepository(c.repos.OperatorProfiles))
	}
	if c.repos != nil && c.repos.OperatorIdentityLinks != nil {
		// Cross-channel identity walking. When wired, the
		// dispatcher resolves a speaker id to its canonical
		// operator id before reading/writing the profile, so
		// a linked operator sees one profile across every
		// channel. See
		// https://docs.vornik.io
		opts = append(opts, dispatcher.WithOperatorIdentityLinkRepository(c.repos.OperatorIdentityLinks))
	}
	if c.repos != nil && c.repos.ProfileUseAudit != nil {
		// Phase B audit: per-turn row recording which
		// operator-profile keys + whether notes the
		// dispatcher injected into the system prompt. Backs
		// `vornikctl operator audit <id>`.
		opts = append(opts, dispatcher.WithProfileUseAuditRepository(c.repos.ProfileUseAudit))
	}
	if c.pricingTable != nil {
		opts = append(opts, dispatcher.WithPricing(c.pricingTable))
	}
	if c.rateLimiter != nil {
		opts = append(opts, dispatcher.WithRateLimiter(c.rateLimiter))
	}
	if model := c.Config.Runtime.AgentLLM.Model; model != "" {
		opts = append(opts, dispatcher.WithDefaultModel(model))
	}
	if pid := c.Config.Telegram.DispatcherProjectID; pid != "" {
		opts = append(opts, dispatcher.WithBillingProjectID(pid))
	}
	if c.memoryManager != nil {
		// RAG search lets the dispatcher answer from existing memory
		// instead of scheduling a research task for known topics.
		opts = append(opts, dispatcher.WithMemorySearcher(c.memoryManager.Searcher))
		corrector := memory.NewCorrector(c.memoryManager.Repository(), c.memoryManager.Searcher)
		// Confidence floor for the fuzzy claim-refute path (0 → the
		// package default). Guards against a weak "wrong claim" match
		// refuting an unrelated chunk (incident 2026-07-31).
		corrector.MinRefuteScore = c.Config.Memory.MinRefuteScore
		opts = append(opts, dispatcher.WithMemoryCorrector(corrector))
		// Knowledge-graph overlay on memory_search (LLD §6.2). Opt-in:
		// when the KG repos aren't wired the searcher is nil and the
		// tool stays chunk-only.
		// see https://docs.vornik.io §6
		if gs := c.newGraphSearcher(); gs != nil {
			opts = append(opts, dispatcher.WithGraphSearcher(gs))
		}
	}
	if c.artifactStore != nil {
		opts = append(opts, dispatcher.WithInputArtifactStore(c.artifactStore))
	}
	// Per-project uploads allow-list root for create_task input_files.
	// Resolved the same way the executor resolves its
	// ProjectWorkspacePath so the dispatcher's confinement gate and the
	// executor's staging guard agree on where channel uploads live
	// (incident-telegram-upload-input-roots-20260712).
	if pwp := resolveProjectWorkspacePath(c.Config.Runtime.ProjectWorkspacePath); pwp != "" {
		opts = append(opts, dispatcher.WithProjectWorkspacePath(pwp))
	}
	// Scheduled reminders — set_reminder tool. Wired only when the
	// reminders repo + Runner are available; nil-tolerant for tests.
	if c.repos != nil && c.repos.Reminders != nil {
		opts = append(opts, dispatcher.WithReminderRepository(c.repos.Reminders))
	}
	if c.reminderRunner != nil {
		opts = append(opts, dispatcher.WithReminderKicker(c.reminderRunner))
	}
	if c.repos != nil && c.repos.AdminAudit != nil {
		// Lets set_reminder emit a `reminder.set` admin-audit
		// row alongside the runner's `reminder.fired` + the
		// UI/API's `reminder.cancelled`. Operators get the full
		// lifecycle in /ui/admin/audit.
		opts = append(opts, dispatcher.WithAdminAuditRepository(c.repos.AdminAudit))
	}
	// Document-extraction auto-trigger for the dispatcher's
	// create_task path. Covers every channel that snapshots
	// uploads through artifactStore.StoreInput (Telegram, webchat,
	// API, CLI) — the email channel has its own parallel trigger
	// fired at attachment-arrival time. Wiring requires the
	// extracted_documents repo, the registry, and the artifact
	// repo; any missing piece downgrades silently to the
	// pre-Phase-3 pass-through behaviour.
	if reg := c.ExtractorRegistry(); reg != nil && c.repos != nil && c.repos.ExtractedDocuments != nil {
		var indexer *memory.Indexer
		if c.memoryManager != nil {
			indexer = c.memoryManager.Indexer
		}
		opts = append(opts, dispatcher.WithAttachmentAutoExtractor(
			newDispatcherAutoExtractor(
				reg,
				c.ExtractorRunner(),
				c.repos.ExtractedDocuments,
				indexer,
				c.artifactStore,
				c.Logger.With().Str("component", "dispatcher-extractor").Logger(),
			),
		))
	}

	// Supervised web-write actions (LLD 2026-07-21). Wired only when the
	// scraper MCP + DB are configured (webWriteComponents gates on both) — the
	// web_submit tool's nil-wiring HARD gate otherwise degrades it to "not
	// configured". WithWebWritesConfig is passed unconditionally so the daemon
	// tri-state toggle (off|on|insecure) is enforced inside the tool without a
	// rebuild when an operator flips it. The token store is SHARED with the UI
	// server (container_http.go) so an inbox approval's minted token reaches the
	// submit path. The AWAITING_APPROVAL task hook is deliberately NOT wired:
	// operator-chat-driven v1 has no autonomous run to park (LLD Components.5).
	if repo, store := c.webWriteComponents(); repo != nil {
		// C1 capability secret: the write client attaches it as daemon_auth on
		// every web_submit call so the scraper can tell a daemon-issued write
		// from an agent's direct call. Resolved fail-closed — a resolution error
		// (unreadable submit_secret_file) or an empty secret still builds the
		// client (writes-off deployments legitimately have no secret); the
		// resulting calls the scraper rejects. Config.Validate already refuses to
		// boot with web.writes on|insecure and no secret, so an empty secret here
		// means writes are off.
		submitSecret, err := c.Config.Web.ResolvedSubmitSecret()
		if err != nil {
			c.Logger.Warn().Err(err).Msg("web_submit capability secret unreadable; scraper will reject web writes")
			submitSecret = ""
		}
		swc := dispatcher.NewMCPScraperWriteClient(c.mcpManager, submitSecret)
		opts = append(opts,
			dispatcher.WithScraperWriteClient(swc),
			dispatcher.WithWebWriteRepo(repo),
			dispatcher.WithWebWritesConfig(c.Config.Web),
			dispatcher.WithWebWriteTokenStore(store),
		)
	}

	// query_api gateway client — built from config, fail-closed. A nil
	// client (disabled/unauthenticated) leaves the tool reporting "not
	// configured"; a config error disables it with a warning rather than
	// failing boot.
	if gwClient, err := newGatewayClient(c.Config.Gateway); err != nil {
		c.Logger.Warn().Err(err).Msg("query_api gateway client disabled: config error")
	} else if gwClient != nil {
		opts = append(opts, dispatcher.WithAPIClient(gwClient))
	}

	// Boot-time diagnostic (query_api provider-discovery design §4.3,
	// cross-task item from Task 2): warn on every project whose
	// permissions.api_providers allowlist names a provider absent from the
	// configured gateway.providers set. Deliberately independent of
	// whether newGatewayClient above returned a usable client — a
	// disabled/misconfigured gateway shouldn't suppress the drift warning
	// for an operator who's mid-way through wiring up providers. Nil-safe
	// on c.Registry for the early-init fixture paths that construct a
	// Container before the registry is loaded.
	if c.Registry != nil {
		knownProviders := make(map[string]bool, len(c.Config.Gateway.Providers))
		for name := range c.Config.Gateway.Providers {
			knownProviders[name] = true
		}
		registry.WarnUnknownAPIProviders(c.Logger, c.Registry.ListProjects(), knownProviders)
	}

	// dispatcher.NewAgent accepts nil for any of the three repos;
	// the c.repos guard handles the very early init paths used by
	// some test fixtures where repos hasn't been set yet.
	var taskRepo persistence.TaskRepository
	var execRepo persistence.ExecutionRepository
	var artifactRepo persistence.ArtifactRepository
	if c.repos != nil {
		taskRepo, execRepo, artifactRepo = c.repos.Tasks, c.repos.Executions, c.repos.Artifacts
	}
	c.Dispatcher = dispatcher.NewAgent(c.ChatClient, taskRepo, execRepo, artifactRepo, c.Registry, opts...)

	// Chat memory-write activation (slices 3+5). The gate is deny-by-default
	// (a channel writes only if a project's memory.write_channels lists it);
	// the confirmation + audit repos back the shared-scope two-step; the
	// pipeline + data-subject repo persist personal-scope writes and link them
	// to the operator for Art 17. All are late-bound setters so a nil DB /
	// unbuilt pipeline degrades cleanly (the tool reports "not built").
	c.wireChatMemoryWrite()

	if c.TelegramBot != nil {
		// ConversationChannel slice 2: attach a ChannelReceiver over
		// the bot so HandleMessage + auto-resume route every
		// dispatcher-bound turn through the receiver path.
		c.wireTelegramReceiver()
	}
	c.Logger.Info().
		Bool("telegram_callbacks", c.TelegramBot != nil).
		Msg("dispatcher initialised")
}

// chatMemoryConfirmations returns the shared-scope confirmation store the
// ChannelReceiver's acknowledgement hook stamps (chat memory-write design
// §5.3.2 step 2). Nil-safe: an early-init container without repos yields nil,
// which leaves the hook dormant (a shared write never advances past PROPOSED)
// rather than panicking. Wired at every receiver construction site so a human
// acknowledgement can discharge a pending write on any channel; the gate
// (§5.1) is what actually decides whether a shared write may be proposed.
func (c *Container) chatMemoryConfirmations() persistence.ChatMemoryWriteConfirmationRepository {
	if c == nil || c.repos == nil {
		return nil
	}
	return c.repos.ChatMemoryWriteConfirmations
}

// wireChatMemoryWrite activates the chat memory-write feature on the
// dispatcher (chat memory-write design, slices 3+5):
//
//   - the capability gate (§5.1) — deny-by-default, granted per channel in
//     project config (memory.write_channels). Registry-backed, no DB, so it
//     is always wired and reflects config the moment a project loads.
//   - the shared-scope confirmation + audit repos (slice 3, §5.3) — available
//     on both backends via c.repos, so the receiver's acknowledgement hook can
//     advance a PROPOSED write to AUTHORIZED.
//   - the personal-scope write path (slice 5, §2/D4.1) — the memory pipeline
//     persists a chat_memory chunk and the data-subject repository links it to
//     the operator for Art 17. The link repo is postgres-only, so the pair is
//     wired only on postgres; a sqlite dev daemon leaves the personal path
//     reporting "not built" rather than writing an unlinkable chunk.
func (c *Container) wireChatMemoryWrite() {
	if c.Dispatcher == nil {
		return
	}
	c.Dispatcher.SetMemoryWriteGate(newMemoryWriteGate(c.Registry))

	if c.repos != nil && c.repos.ChatMemoryWriteConfirmations != nil && c.repos.ChatMemoryWriteAudit != nil {
		c.Dispatcher.SetMemoryWriteConfirmations(c.repos.ChatMemoryWriteConfirmations, c.repos.ChatMemoryWriteAudit)
	}

	if c.memoryPipeline != nil && c.DB != nil && c.Config.Database.Driver == "postgres" {
		c.Dispatcher.SetChatMemoryWriter(c.memoryPipeline, postgres.NewDataSubjectRepository(c.DB))
		// The shared-scope pre-commit NED gate (slice 4, §6). It needs the same
		// extractor/resolver the KG worker builds (startGraphWorker) plus its
		// OWN billing wiring (LLMUsage + Pricing) so its extract/resolve spend
		// lands under chat_remember_ned rather than going unbilled — the
		// reranker/distiller failure class (D6.4). Postgres-only for the same
		// reason as the writer: the third-party data-subject link is postgres-only.
		if c.repos != nil && c.repos.KnowledgeEntities != nil && c.ChatClient != nil {
			c.Dispatcher.SetChatMemoryNED(c.buildChatMemoryNED())
		}
	}
}

// buildChatMemoryNED constructs the shared-scope NED gate, mirroring
// startGraphWorker's extractor/resolver construction (same models, catalog
// repo, and embedder) but with its own billed call path.
func (c *Container) buildChatMemoryNED() *ned.Gate {
	cfg := c.Config.Memory.Graph
	extractorModel := cfg.ExtractorModel
	if extractorModel == "" {
		extractorModel = "openai.gpt-oss-20b-1:0"
	}
	resolverModel := cfg.ResolverModel
	if resolverModel == "" {
		resolverModel = "openai.gpt-oss-20b-1:0"
	}

	extractor := graph.NewExtractor(c.ChatClient, extractorModel)
	if c.memoryManager != nil && c.memoryManager.ResponseCache != nil {
		extractor.Cache = c.memoryManager.ResponseCache
	}
	var embedFn graph.EmbedFn
	if c.memoryManager != nil && c.memoryManager.Embedder != nil {
		embedFn = func(ctx context.Context, projectID string, texts []string) ([][]float32, error) {
			return c.memoryManager.Embedder.Embed(ctx,
				memory.EmbedScope{ProjectID: projectID, CallSite: memory.EmbedCallSiteKGResolve}, texts)
		}
	}
	resolver := graph.NewResolver(c.ChatClient, resolverModel, c.repos.KnowledgeEntities, embedFn)

	return &ned.Gate{
		Extractor: extractor,
		Resolver:  resolver,
		Spend: c.llmSpend(
			persistence.TaskLLMUsageSourceChatRememberNED, ned.RoleExtractor),
	}
}
