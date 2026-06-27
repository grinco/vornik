package service

import (
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/hallucination"
	"vornik.io/vornik/internal/intentjudge"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
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
	if c.mcpManager != nil {
		opts = append(opts, dispatcher.WithMCPManager(c.mcpManager))
	}
	if c.repos != nil && c.repos.ToolAudit != nil {
		opts = append(opts, dispatcher.WithAuditRepository(c.repos.ToolAudit))
	}
	if c.repos != nil && c.repos.ChatAudit != nil {
		opts = append(opts, dispatcher.WithChatAuditRepo(c.repos.ChatAudit))
	}
	if c.repos != nil && c.repos.LLMUsage != nil {
		opts = append(opts, dispatcher.WithLLMUsageRepository(c.repos.LLMUsage))
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
