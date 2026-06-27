package service

// Autonomy + retention + memory-graph worker wiring extracted
// from container.go as part of the 2026-05-16 service-package
// split. Owns:
//   - startGraphWorker  (KG-extraction worker)
//   - initAutonomy      (autonomous-task manager)
//   - initRetention     (retention sweeper + loop)
//   - runRetentionLoop  + runRetentionOnce

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"vornik.io/vornik/internal/autonomy"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/memory/graph"
	"vornik.io/vornik/internal/retention"
)

// initAutonomy starts the autonomous task manager.
// The manager is always created when chat is configured so that /autopilot
// Telegram commands work even before any project has autonomy.enabled=true in YAML.
// startGraphWorker constructs the four-stage KG extraction
// pipeline + the worker that drains chunks where
// needs_graph_extraction = TRUE. Per-stage models pin to cost-
// efficient Bedrock IDs by default; operators can override via
// memory.graph.{extractor,resolver,relationship,validator}_model.
//
// Bedrock model defaults (LLD §4.4a, pricing per
// configs/pricing.yaml as of 2026-05-10):
//
//	extractor    → openai.gpt-oss-20b-1:0   ($0.08 in / $0.20 out per 1M tokens)
//	resolver     → openai.gpt-oss-20b-1:0   (same)
//	validator    → openai.gpt-oss-20b-1:0   (same)
//	relationship → openai.gpt-oss-120b-1:0  ($0.15 in / $0.60 out)
//
// Each model ID gets routed to the bedrock sub-provider via the
// router's openai.* prefix; see configs/vornik.yaml router.routes.
//
// The chat router itself implements chat.Provider; per-stage
// model pinning happens via the Extractor/Resolver/etc.'s Model
// field which the LLM helper applies through ModelOverridable
// at call time.
func (c *Container) startGraphWorker(mgr *memory.Manager) {
	// Resolve per-stage model IDs from config, applying defaults.
	cfg := c.Config.Memory.Graph
	extractorModel := cfg.ExtractorModel
	if extractorModel == "" {
		extractorModel = "openai.gpt-oss-20b-1:0"
	}
	resolverModel := cfg.ResolverModel
	if resolverModel == "" {
		resolverModel = "openai.gpt-oss-20b-1:0"
	}
	validatorModel := cfg.ValidatorModel
	if validatorModel == "" {
		validatorModel = "openai.gpt-oss-20b-1:0"
	}
	relationshipModel := cfg.RelationshipModel
	if relationshipModel == "" {
		relationshipModel = "openai.gpt-oss-120b-1:0"
	}

	entityRepo := c.repos.KnowledgeEntities
	edgeRepo := c.repos.KnowledgeEdges
	mentionRepo := c.repos.EntityMentions
	chunkRepo := c.repos.ChunkGraphExtraction

	// Embedder closure: re-use the memory manager's Embedder so we
	// don't double-pay for endpoint config and so canonical_name
	// vectors come from the same model the chunk vectors came from
	// (must match for SimilarByEmbedding to work).
	embedFn := func(ctx context.Context, texts []string) ([][]float32, error) {
		if mgr.Embedder == nil {
			return nil, nil
		}
		return mgr.Embedder.Embed(ctx, texts)
	}

	extractor := graph.NewExtractor(c.ChatClient, extractorModel)
	// Phase E — KG-extract response cache shares mgr.ResponseCache.
	// Nil-safe when Memory.ResponseCacheEnabled is false; the
	// concrete *memory.responseCacheRepo satisfies graph.ResponseCache
	// via structural typing.
	if mgr.ResponseCache != nil {
		extractor.Cache = mgr.ResponseCache
	}
	pipeline := &graph.Pipeline{
		Extractor: extractor,
		Resolver:  graph.NewResolver(c.ChatClient, resolverModel, entityRepo, embedFn),
		Relations: graph.NewRelationshipExtractor(c.ChatClient, relationshipModel),
		Validator: graph.NewValidator(c.ChatClient, validatorModel),
		Entities:  entityRepo,
		Edges:     edgeRepo,
		Mentions:  mentionRepo,
		Embedder:  embedFn,
		LLMUsage:  c.repos.LLMUsage,
		Pricing:   c.pricingTable,
	}

	wcfg := graph.WorkerConfig{
		PollInterval:         time.Duration(cfg.PollIntervalSeconds) * time.Second,
		BatchSize:            cfg.BatchSize,
		MaxParallel:          cfg.MaxParallel,
		GaugeRefreshInterval: time.Duration(cfg.GaugeRefreshSeconds) * time.Second,
	}
	c.graphWorker = graph.NewWorker(
		chunkRepo,
		pipeline,
		c.Logger.With().Str("component", "memory").Str("worker", "kg-extract").Logger(),
		wcfg,
	)
	c.Logger.Info().
		Str("extractor_model", extractorModel).
		Str("resolver_model", resolverModel).
		Str("relationship_model", relationshipModel).
		Str("validator_model", validatorModel).
		Msg("KG extraction worker constructed")
}

func (c *Container) initAutonomy() {
	if c.ChatClient == nil || c.Registry == nil {
		return
	}

	// Resolve the workspace path using the same logic as the executor so the
	// autonomy manager can check for PROJECT_CONTEXT.md on the host filesystem.
	workspacePath := c.Config.Runtime.ProjectWorkspacePath
	if workspacePath == "" {
		if dataDir := os.Getenv("VORNIK_DATA_DIR"); dataDir != "" {
			workspacePath = filepath.Join(dataDir, "workspaces")
		}
	}

	taskRepo := c.repos.Tasks
	execRepo := c.repos.Executions
	auditRepo := c.repos.ToolAudit
	llmUsageRepo := c.repos.LLMUsage
	evalRepo := c.repos.AutonomyEvaluations

	autonomyOpts := []autonomy.Option{
		autonomy.WithLogger(c.Logger),
		autonomy.WithToolAuditRepository(auditRepo),
		autonomy.WithLLMUsageRepository(llmUsageRepo),
		autonomy.WithBudgetReservationRepository(c.repos.BudgetReservations),
		autonomy.WithSteeringNotifier(c.combinedSteeringNotifier()),
		autonomy.WithEvaluationRepository(evalRepo),
		autonomy.WithRateLimiter(c.rateLimiter),
	}
	if c.Config.Autonomy.DefaultEvaluateTimeout != "" {
		if d, err := time.ParseDuration(c.Config.Autonomy.DefaultEvaluateTimeout); err == nil && d > 0 {
			autonomyOpts = append(autonomyOpts, autonomy.WithDefaultEvaluateTimeout(d))
		} else {
			c.Logger.Warn().
				Str("default_evaluate_timeout", c.Config.Autonomy.DefaultEvaluateTimeout).
				Msg("invalid autonomy.default_evaluate_timeout in config — using compiled default")
		}
	}
	if c.TelegramBot != nil {
		autonomyOpts = append(autonomyOpts, autonomy.WithBudgetNotifier(c.TelegramBot))
	}
	if workspacePath != "" {
		autonomyOpts = append(autonomyOpts, autonomy.WithWorkspacePath(workspacePath))
	}
	if c.pricingTable != nil {
		autonomyOpts = append(autonomyOpts, autonomy.WithPricing(c.pricingTable))
	}
	if globalModel := c.Config.Runtime.AgentLLM.Model; globalModel != "" {
		autonomyOpts = append(autonomyOpts, autonomy.WithDefaultModel(globalModel))
	}
	// 2026.8.0 horizontal-scaling: gate the autonomy tick on the
	// elected leader so two daemons don't double-schedule via
	// create_task. Nil-safe when the lock repo isn't wired
	// (single-process default).
	c.autonomyElector = c.initWorkerElector("autonomy_manager")
	if c.autonomyElector != nil {
		autonomyOpts = append(autonomyOpts, autonomy.WithLeaderGate(c.autonomyElector))
	}

	mgr := autonomy.New(c.ChatClient, c.Registry, taskRepo, execRepo, autonomyOpts...)
	// Note: Start() spawns per-project goroutines that begin
	// ticking immediately. The leader-gate check inside each
	// tick handles the "haven't acquired yet" case by skipping
	// — Container.Run launches the elector goroutine + calls
	// BootstrapAcquire (see Run()'s "leader-election: workers"
	// block).
	mgr.Start()
	c.autonomyManager = mgr

	active := mgr.ActiveLoops()
	if active > 0 {
		c.Logger.Info().Int("active_loops", active).Msg("autonomous task manager initialized")
	} else {
		c.Logger.Debug().Msg("autonomous task manager initialized (no projects with autonomy enabled)")
	}

	// Wire into Telegram bot so /autopilot commands work.
	if c.TelegramBot != nil {
		c.TelegramBot.SetAutonomyManager(mgr)
	}
}

// initRetention starts the background retention sweeper when enabled in
// config. Safe to no-op: absent config → no sweeper. The sweeper iterates
// projects on each tick and prunes data older than the resolved windows.
// Touches project_memory_chunks only when MemoryChunksDays > 0 (operator
// opt-in escape hatch on top of per-class TTL).
func (c *Container) initRetention() {
	cfg := c.Config.Retention
	if !cfg.Enabled {
		c.Logger.Debug().Msg("retention sweeper disabled (set retention.enabled=true to enable)")
		return
	}
	interval, err := time.ParseDuration(cfg.Interval)
	if err != nil || interval <= 0 {
		interval = 6 * time.Hour
	}
	sweeper := retention.New(c.DB, c.Logger.With().Str("component", "retention").Logger())

	defaults := retention.Policy{
		TaskLLMUsageDays:          cfg.TaskLLMUsageDays,
		ToolAuditDays:             cfg.ToolAuditDays,
		TasksDays:                 cfg.TasksDays,
		ExecutionsDays:            cfg.ExecutionsDays,
		ArtifactsDays:             cfg.ArtifactsDays,
		TaskMessagesDays:          cfg.TaskMessagesDays,
		MemoryChunksDays:          cfg.MemoryChunksDays,
		MemoryIngestAuditDays:     cfg.MemoryIngestAuditDays,
		MemoryPolicyEvalAllowDays: cfg.MemoryPolicyEvalAllowDays,
		MemoryPolicyEvalBlockDays: cfg.MemoryPolicyEvalBlockDays,
		ArtifactsRoot:             c.Config.Storage.ArtifactsPath,
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.retentionCancel = cancel
	c.retentionDone = make(chan struct{})

	// Leader-gate the sweep so multi-replica deployments only
	// prune once per interval globally instead of once per
	// replica. Nil elector (SQLite branch) preserves the
	// single-process behaviour. See
	// https://docs.vornik.io §3
	// Slice 1.
	c.retentionElector = c.initWorkerElector("retention_sweeper")
	if c.retentionElector != nil {
		c.retentionElector.BootstrapAcquire(ctx)
		go c.retentionElector.Run(ctx)
	}

	go func() {
		defer close(c.retentionDone)
		c.runRetentionLoop(ctx, sweeper, defaults, interval)
	}()
	c.Logger.Info().Dur("interval", interval).Msg("retention sweeper started")
}

// runRetentionLoop runs the sweeper on the configured interval until ctx is
// cancelled. Iterates every project in the registry on each tick.
//
// Leader-gated: in a multi-instance deployment c.retentionElector
// holds the row in daemon_leader_locks; non-leaders skip the tick.
// Nil elector (single-process / SQLite branch) runs every tick.
func (c *Container) runRetentionLoop(ctx context.Context, sweeper *retention.Sweeper, defaults retention.Policy, interval time.Duration) {
	// Initial delay so the first sweep doesn't race with startup recovery.
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if c.retentionElector == nil || c.retentionElector.IsLeader() {
				c.runRetentionOnce(ctx, sweeper, defaults)
			}
			timer.Reset(interval)
		}
	}
}

func (c *Container) runRetentionOnce(ctx context.Context, sweeper *retention.Sweeper, defaults retention.Policy) {
	// Global (non-project-scoped) caches run once per cycle BEFORE
	// the per-project loop. ResponseCacheDays defaults to 0 which
	// short-circuits inside SweepGlobal; operators opt in via
	// retention.response_cache_days (recommended: 30).
	if g, err := sweeper.SweepGlobal(ctx, c.Config.Retention.ResponseCacheDays); err != nil {
		c.Logger.Warn().Err(err).Msg("retention global sweep partially failed")
	} else if g.ResponseCache > 0 {
		c.Logger.Info().Int("response_cache", g.ResponseCache).Msg("retention global sweep pruned rows")
	}

	if c.Registry == nil {
		return
	}
	projects := c.Registry.ListProjects()
	for _, p := range projects {
		if p == nil {
			continue
		}
		policy := retention.Resolve(p.ID, retention.Policy{
			TaskLLMUsageDays:          p.Retention.TaskLLMUsageDays,
			ToolAuditDays:             p.Retention.ToolAuditDays,
			TasksDays:                 p.Retention.TasksDays,
			ExecutionsDays:            p.Retention.ExecutionsDays,
			ArtifactsDays:             p.Retention.ArtifactsDays,
			TaskMessagesDays:          p.Retention.TaskMessagesDays,
			MemoryChunksDays:          p.Retention.MemoryChunksDays,
			MemoryIngestAuditDays:     p.Retention.MemoryIngestAuditDays,
			MemoryPolicyEvalAllowDays: p.Retention.MemoryPolicyEvalAllowDays,
			MemoryPolicyEvalBlockDays: p.Retention.MemoryPolicyEvalBlockDays,
		}, defaults)

		counts, err := sweeper.Sweep(ctx, policy)
		if err != nil {
			c.Logger.Warn().Err(err).Str("project", p.ID).Msg("retention sweep partially failed")
		}
		if counts.TaskLLMUsage+counts.ToolAudit+counts.Tasks+counts.Executions+counts.Artifacts+counts.TaskMessages+counts.MemoryChunks+counts.MemoryIngestAudit+counts.MemoryPolicyEvalAllow+counts.MemoryPolicyEvalBlock > 0 {
			c.Logger.Info().
				Str("project", p.ID).
				Int("llm_usage", counts.TaskLLMUsage).
				Int("tool_audit", counts.ToolAudit).
				Int("tasks", counts.Tasks).
				Int("executions", counts.Executions).
				Int("artifacts", counts.Artifacts).
				Int("artifact_files", counts.ArtifactFiles).
				Int("task_messages", counts.TaskMessages).
				Int("memory_chunks", counts.MemoryChunks).
				Int("memory_ingest_audit", counts.MemoryIngestAudit).
				Int("memory_policy_eval_allow", counts.MemoryPolicyEvalAllow).
				Int("memory_policy_eval_block", counts.MemoryPolicyEvalBlock).
				Msg("retention sweep pruned rows")
		}
	}
}
