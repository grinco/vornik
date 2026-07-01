package service

// HTTP server wiring + timeout helpers extracted from container.go
// as part of the 2026-05-16 service-package split. Owns:
//   - initHTTPServer         (mux build, middleware chain,
//                             UI/API mount, http.Server config)
//   - parseDuration          (duration-string parser w/ default)
//   - uiSubtreeHandler       (extra logging wrapper for the UI tree)
//   - resolveChatTimeout     (chat.timeout config helper)
//   - effectiveServerWriteTimeout (raises server.write_timeout to
//                             exceed chat-proxy budget if too low)
//   - resolveDispatchTimeout (telegram.dispatcher_timeout helper)
//   - resolvePricingPath     (locates configs/pricing.yaml)

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/admin"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/conversation/a2a"
	"vornik.io/vornik/internal/email"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/httpx/realip"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/onboarding"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/postmortem"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/replay"
	"vornik.io/vornik/internal/retention"
	"vornik.io/vornik/internal/slack"
	"vornik.io/vornik/internal/taskcreate"
	"vornik.io/vornik/internal/templates"
	"vornik.io/vornik/internal/tradingauth"
	"vornik.io/vornik/internal/ui"
	"vornik.io/vornik/internal/workflowhealing"
	"vornik.io/vornik/internal/workflowtelemetry"
)

// initHTTPServer initializes the HTTP server with health endpoints.
func (c *Container) initHTTPServer() error {
	mux := http.NewServeMux()

	// Liveness + readiness probes are served by the API layer
	// (Server.Healthz / Server.Readyz, registered in routes.go).
	// The container previously registered shadow handlers on the
	// root mux, which silently took precedence over the API ones
	// — so the richer Readyz (DB ping + structured per-check
	// report + ReadinessCheck options) was unreachable. Removed
	// in the 2026-05-03 audit cleanup; mux.Handle("/", apiServer.Routes())
	// below now lets the API handlers serve those paths.
	// Shared task-creation core — funnels both REST API
	// (POST /api/v1/projects/{p}/tasks) and the UI form
	// (POST /ui/projects/{p}/tasks/new) through one validation /
	// rate-limit / budget / persist / enqueue pipeline. Before
	// this surfaced the two surfaces had drifted on rate-limit
	// ordering and budget semantics; the real-world E2E gap that
	// drove this work was operators having to use curl because
	// the UI lacked a proper task-creation form.
	taskCreatorOpts := []taskcreate.Option{
		taskcreate.WithTaskRepository(c.repos.Tasks),
		taskcreate.WithProjectRegistry(c.Registry),
		taskcreate.WithRateLimiter(c.rateLimiter),
		taskcreate.WithLLMUsageRepository(c.repos.LLMUsage),
		taskcreate.WithBudgetReservationRepository(c.repos.BudgetReservations),
		taskcreate.WithLogger(c.Logger.With().Str("component", "taskcreate").Logger()),
	}
	if c.TelegramBot != nil {
		taskCreatorOpts = append(taskCreatorOpts, taskcreate.WithBudgetNotifier(c.TelegramBot))
	}
	taskCreator := taskcreate.New(taskCreatorOpts...)

	apiOpts := []api.ServerOption{
		api.WithLogger(c.Logger),
		api.WithTaskRepository(c.repos.Tasks),
		api.WithExecutionRepository(c.repos.Executions),
		// Failure-forensics fork primitive (Feature #1 Phase B).
		// Adapter wraps a replay.Forker; the api package has its
		// own narrow ForkExecutor interface to avoid an import on
		// replay. Audit + metrics wired here (Phase C) so every
		// fork lands a row on /ui/admin/audit and ticks the
		// vornik_replay_forks_total counter.
		api.WithForker(newForkExecutorAdapter(&replay.Forker{
			Executions: c.repos.Executions,
			Outcomes:   c.repos.StepOutcomes,
			Tasks:      c.repos.Tasks,
			AuditAdmin: adminAuditOrNil(c),
			Metrics:    c.replayMetrics,
		})),
		// Conversational project-setup wizard (Feature #2).
		// Wired only when the chat router + sessions repo are
		// available — bare-bones deployments fall through to the
		// handler's 503.
		api.WithProjectWizard(buildProjectWizardOrNil(c)),
		func() api.ServerOption {
			var sessions persistence.InstallationOnboardingSessionRepository
			if c.repos != nil {
				sessions = c.repos.InstallationOnboardingSessions
			}
			return api.WithOnboardingDetector(onboarding.Detector{
				Sessions: sessions,
				Config:   c.Config,
			})
		}(),
		func() api.ServerOption {
			var sessions persistence.InstallationOnboardingSessionRepository
			if c.repos != nil {
				sessions = c.repos.InstallationOnboardingSessions
			}
			return api.WithSetupSessions(sessions)
		}(),
		api.WithSetupValidator(onboarding.NewChatValidator()),
		api.WithSetupMemoryValidator(onboarding.NewMemoryValidator()),
		api.WithSetupConfigPath(c.ConfigPath),
		api.WithSetupSecretsDir(onboardingSecretsDir(c.ConfigPath)),
		// Live observation subscriber (Feature #3 Phase B). The
		// publisher itself is wired into the executor at
		// construction; the WebSocket handler reads from the
		// same instance so taps and subscribers are zero-overhead
		// in-process. nil-safe when c.livePub didn't initialise.
		api.WithLiveSubscriber(c.livePub),
		// Feature #3 Phase C — hint endpoint repo. Wired only
		// when migration 50 has run + the repos struct populated
		// ExecutionHints; nil falls through to the handler's 503.
		api.WithExecutionHintRepository(c.repos.ExecutionHints),
		// Inter-project orchestration Phase D follow-on — wire
		// the CPC ledger so /api/v1/admin/cpc endpoints work.
		api.WithCrossProjectCallRepository(c.repos.CrossProjectCalls),
		api.WithReminderRepository(c.repos.Reminders),
		// Autonomy Black Box — assembles per-task unified traces from the nine
		// audit tables for the admin/SOC2 surface. Wired off Postgres only via the
		// EE BBTraceServiceFactory (Task 6 seam). SQLite deployments leave the
		// option un-set and the endpoint returns 503.
		api.WithBlackBoxService(c.buildBBTraceService()),
		// Phase C counterfactual replay engine. Engine returns nil when DB or
		// taskCreator isn't wired; the API surface 503s in that case. The
		// engine is now built via the EE BBReplayEngineFactory (Task 6 seam).
		api.WithBlackBoxEngine(c.buildBBReplayEngine(taskCreator)),
		// ReplaySafety: c.providers.ReplaySafety is set by enterprise.Providers()
		// (non-nil) or CommunityProviders() (nil). CE fails closed on nil.
		api.WithBlackBoxReplaySafety(c.providers.ReplaySafety),
		// Memory Firewall Phase C admin endpoints — read the
		// audit table + report current enforcement mode.
		api.WithMemoryPolicyEvaluations(c.repos.MemoryPolicyEvaluations),
		api.WithMemoryFirewallMode(c.memoryFirewallMode()),
		api.WithMemoryFirewallEditor(c.memoryFirewallEditor()),
		// Workflow-healing trigger admin endpoints (Phase B).
		api.WithHealingTriggerRepository(c.repos.HealingTriggers),
		api.WithHealingOverrideRepository(c.repos.HealingOverrides),
		// Self-Healing Workflow Genome v1 — candidate ledger (migration
		// 87). generate-candidate persists a candidate linked to the
		// architect proposal when this repo is wired.
		api.WithHealingCandidateRepository(c.repos.HealingCandidates),
		// Deterministic-recipe step selection (Self-Healing Genome v1
		// part 2): generate-candidate tallies per-step failures across a
		// trigger's evidence executions to pick the offending step, trying
		// a structural recipe before the LLM architect.
		api.WithExecutionStepOutcomeRepository(c.repos.StepOutcomes),
		api.WithArchiveService(c.archiveLifecycle),
		api.WithLLMUsageRepository(c.repos.LLMUsage),
		api.WithBudgetReservationRepository(c.repos.BudgetReservations),
		api.WithChatAuditRepository(c.repos.ChatAudit),
		api.WithWebhookEventRepository(c.repos.Webhooks),
		api.WithAPIKeyRepository(c.repos.APIKeys),
		api.WithAPIKeyLimiter(c.apiKeyLimiter),
		// Per-IP backstop (hardening sub-item 2). Same instance is
		// shared with the UI subtree's middleware below so a flood
		// targeting one surface can't sidestep the limit via the
		// other.
		api.WithPerIPLimiter(c.perIPLimiter,
			c.Config.API.RateLimit.PerIP.RPS,
			c.Config.API.RateLimit.PerIP.Burst),
		api.WithAutonomyEvaluationRepository(c.repos.AutonomyEvaluations),
		api.WithRateLimiter(c.rateLimiter),
		api.WithTaskCreator(taskCreator),
		api.WithConfig(c.Config),
	}
	// Edition gate (outer) — Admin interfaces. Both initHTTPServer passes
	// run this same function body, so the gate covers both automatically.
	// The log fires only on the first pass (adminCapabilityLogged guard).
	if c.providers.Admin {
		// admin-ui-design.md slice 1 — wire the admin gate config
		// + the audit repo into the api server so /api/v1/admin/audit
		// can enforce the same 404/403 matrix the UI uses.
		// Inner gate (c.Config.Admin) is the existing config check; it
		// stays unchanged — edition is the OUTER gate, config is inner.
		apiOpts = append(apiOpts, api.WithAdminConfig(c.Config.Admin))
		if c.repos != nil && c.repos.AdminAudit != nil {
			apiOpts = append(apiOpts, api.WithAdminAuditRepository(c.repos.AdminAudit))
		}
		if !c.adminCapabilityLogged {
			c.Logger.Info().
				Str("capability", "admin").
				Bool("edition_gate", true).
				Msg("capability registered")
			c.adminCapabilityLogged = true
		}
	} else if !c.adminCapabilityLogged {
		c.Logger.Info().
			Str("capability", "admin").
			Bool("edition_gate", false).
			Msg("capability omitted")
		c.adminCapabilityLogged = true
	}
	// Fleet observability (slice C1) — ClusterNodes + LeaderLocks back
	// GET /api/v1/cluster. Both are nil-safe: the handler returns 503 when
	// ClusterNodes is absent; LeaderLocks absence → empty leases slice.
	if c.repos != nil && c.repos.ClusterNodes != nil {
		apiOpts = append(apiOpts, api.WithClusterNodeRepository(c.repos.ClusterNodes))
	}
	if c.repos != nil && c.repos.LeaderLocks != nil {
		apiOpts = append(apiOpts, api.WithLeaderLockRepository(c.repos.LeaderLocks))
	}
	if c.repos != nil && c.repos.Instincts != nil {
		// Continuous-learning instinct layer surfaces: list / show /
		// retire (open) + admin recompute (admin-gated). Wired whenever
		// the repo exists regardless of instinct.enabled — the worker
		// gate only governs the write path; reading already-mined
		// instincts is always safe. The scorer mirrors the worker's so
		// the admin recompute uses the same Wilson+decay model.
		scorerOpts := []api.ServerOption{api.WithInstinctRepository(c.repos.Instincts)}
		if c.providers.InstinctScorerFactory != nil && c.Config != nil {
			scorerOpts = append(scorerOpts, api.WithInstinctScorer(c.providers.InstinctScorerFactory(c.Config.Instinct)))
		}
		apiOpts = append(apiOpts, scorerOpts...)
	}
	if c.repos != nil && c.repos.OperatorProfiles != nil {
		// Wires /api/v1/operators (list/show/set/forget) for the
		// `vornikctl operator` CLI. Same repo instance the
		// dispatcher's update_operator_profile tool and the
		// /ui/memory/operators admin UI use.
		apiOpts = append(apiOpts, api.WithOperatorProfileRepository(c.repos.OperatorProfiles))
	}
	if c.repos != nil && c.repos.OperatorIdentityLinks != nil {
		// Wires /api/v1/operators/{id}/links for the
		// `vornikctl operator link / unlink / show-links` CLI.
		// Same repo the dispatcher's canonical-id resolver
		// uses; nil-safe (CLI commands report 503).
		apiOpts = append(apiOpts, api.WithOperatorIdentityLinkRepository(c.repos.OperatorIdentityLinks))
	}
	if c.repos != nil && c.repos.ProfileUseAudit != nil {
		// Phase B audit endpoint: GET
		// /api/v1/operators/{id}/audit. Same repo the
		// dispatcher's per-turn audit write hits.
		apiOpts = append(apiOpts, api.WithProfileUseAuditRepository(c.repos.ProfileUseAudit))
	}
	if c.secretsDetector != nil {
		apiOpts = append(apiOpts, api.WithSecrets(c.secretsDetector, c.secretsActions))
	}
	if c.Executor != nil {
		apiOpts = append(apiOpts, api.WithExecutor(c.Executor))
	}
	// Shared per-project workspace lock — same instance the executor +
	// UI server hold. Stored on the api server for the git-over-HTTPS
	// handler (Task 2.4).
	if c.WorkspaceLock != nil {
		apiOpts = append(apiOpts, api.WithWorkspaceLock(c.WorkspaceLock))
	}
	// Git-over-HTTPS push guards (Task 2.4): re-assert
	// receive.denyCurrentBranch=updateInstead + the pre-receive hook on the
	// project's workspace repo immediately before receive-pack runs. The repo
	// lives at <ProjectWorkspacePath>/<projectID> — the SAME root the api
	// server's gitWorkspaceRoot resolves against — so the served repo and the
	// guarded repo are identical.
	{
		workspaceRoot := c.Config.Runtime.ProjectWorkspacePath
		logger := c.Logger
		apiOpts = append(apiOpts, api.WithGitReceiveGuards(func(ctx context.Context, projectID string) error {
			return executor.EnsureReceiveGuards(ctx, filepath.Join(workspaceRoot, projectID), logger)
		}))
	}
	if c.Registry != nil {
		apiOpts = append(apiOpts, api.WithProjectRegistry(c.Registry))
		// Forge-job classifier for the generic webhook path: stamps a
		// provider-neutral forge_job on tasks for forge-configured projects so
		// the deterministic forge.* system steps need no free-text parsing.
		if fc := c.newForgeClassifier(); fc != nil {
			apiOpts = append(apiOpts, api.WithForgeClassifier(fc))
		}
	}
	// A2A protocol surface — opt-in per workflow via the
	// `a2a.publish: true` frontmatter field. The handler reads
	// the project registry to enumerate published agents; task
	// submission delegates to the same taskCreator the REST API
	// uses; the SSE bridge reuses the existing live-pubsub
	// subscriber + execution/task repos. See
	// https://docs.vornik.io
	if c.Registry != nil && taskCreator != nil {
		baseURL := ""
		if c.Config != nil {
			baseURL = c.Config.Telegram.WebUIBaseURL
		}
		a2aHandler := &a2a.Handler{
			BaseURLProvider: a2a.PublicBaseURLFunc(func() string { return baseURL }),
			Registry:        c.Registry,
			TaskCreator:     a2aTaskCreatorAdapter{inner: taskCreator},
			LiveSubscriber:  c.livePub,
			Logger:          c.Logger,
		}
		// Push notifications: when the config repo is wired, the submit
		// handler accepts pushNotificationConfig + the agent card advertises
		// it, and the daemon POSTs task-state webhooks (see the push notifier
		// wiring in the scheduler/email-channels notifier multiplexers).
		if c.repos != nil && c.repos.A2APushConfigs != nil {
			a2aHandler.PushConfigStore = c.repos.A2APushConfigs
		}
		if c.repos != nil && c.repos.Executions != nil && c.repos.Tasks != nil {
			a2a.WireSSE(&a2a.SSEDeps{
				Executions: c.repos.Executions,
				Tasks:      c.repos.Tasks,
			})
		}
		apiOpts = append(apiOpts, api.WithA2AHandler(a2aHandler))
	}
	// Project-template gallery (2026.6.0 SaaS-readiness). Loads
	// templates from <configsDir>/project-templates/ at startup so
	// the gallery (API + web UI + CLI) all see the same catalog.
	// Missing templates dir is not an error — Load() returns an
	// empty catalog and the consumers return 503 with a clear
	// "not configured" message. Catalog reference is held outside
	// the API block so the UI server below can reuse it.
	var projectTemplatesCatalog *templates.Catalog
	var projectTemplatesConfigsDir string
	if configsDir := resolveRegistryConfigDir(c.ConfigPath); configsDir != "" {
		templatesDir := filepath.Join(configsDir, "project-templates")
		if templatesEnv := os.Getenv("VORNIK_TEMPLATES_DIR"); templatesEnv != "" {
			templatesDir = templatesEnv
		}
		if cat, terr := templates.Load(templatesDir); terr != nil {
			c.Logger.Warn().Err(terr).Str("dir", templatesDir).
				Msg("project-templates: load failed — gallery will return 503; check manifest YAML")
		} else {
			projectTemplatesCatalog = cat
			projectTemplatesConfigsDir = configsDir
			apiOpts = append(apiOpts, api.WithProjectTemplates(cat))
			apiOpts = append(apiOpts, api.WithConfigsDir(configsDir))
			c.Logger.Info().
				Str("dir", templatesDir).
				Int("templates", len(cat.List())).
				Msg("project-templates: catalog loaded")
		}
	}
	if reg := c.observabilityRegistry(); reg != nil {
		if c.rateLimitMetrics == nil {
			c.rateLimitMetrics = ratelimit.NewMetrics(reg)
		}
		// dryRunMetrics mirrors rateLimitMetrics: built ONLY when the
		// observability registry exists, so the counter lives on the
		// SAME registry /metrics serves from.
		//
		// TWO-PASS TRAP (2026-06-06 invisible-metric incident):
		// initHTTPServer runs twice — once inside NewContainer (registry
		// nil) and again from NewContainerWithObservability after the
		// registry exists (the deliberate rebuild at container.go
		// "Rebuild the HTTP server so /metrics uses the custom
		// registry"). An unconditional pass-1 build registers the
		// counter on prometheus.DefaultRegisterer, the once-only guard
		// then keeps that instance on pass 2, and /metrics — now serving
		// the custom registry — never shows it: denials increment
		// invisibly. Pass 1 therefore leaves dryRunMetrics nil (the
		// middleware is nil-safe: WARN logs still fire) and pass 2
		// builds it here on the served registry. The pass-1 server
		// never handles requests — the listener starts only after
		// NewContainerWithObservability returns.
		//
		// TYPED-NIL TRAP (2026-06-06 startup crash-loop; same class as
		// the route-queue registry panic in the ROADMAP): never pass
		// observabilityRegistry()'s *prometheus.Registry return into a
		// Registerer parameter without a concrete-pointer nil check —
		// a typed nil makes the interface non-nil and promauto
		// SIGSEGVs on Registry.Register. The reg != nil guard above is
		// that check.
		if c.dryRunMetrics == nil {
			c.dryRunMetrics = api.NewDryRunMetrics(reg)
		}
		// Same pass-2-only rule as dryRunMetrics (see the trap notes
		// above): build the chain-verdict counter on the served
		// registry, never on pass 1's DefaultRegisterer.
		if c.chainMetrics == nil {
			c.chainMetrics = api.NewAuthChainMetrics(reg)
		}
		// Same pass-2-only rule: the trading-series anomaly gauge must live
		// on the served registry. The probe (built in the trading block
		// below) reads c.tradingSeriesMetrics.Set as its metric sink.
		if c.tradingSeriesMetrics == nil {
			c.tradingSeriesMetrics = api.NewTradingSeriesMetrics(reg)
		}
		// Same pass-2-only rule for the equity cross-checker's gauges; the
		// trading subsystem reads c.equityCheckMetrics.Set as its sink.
		if c.equityCheckMetrics == nil {
			c.equityCheckMetrics = api.NewTradingEquityCheckMetrics(reg)
		}
		apiOpts = append(apiOpts, api.WithMetricsRegistry(reg))
		apiOpts = append(apiOpts, api.WithRateLimitMetrics(c.rateLimitMetrics))
		apiOpts = append(apiOpts, api.WithDryRunMetrics(c.dryRunMetrics))
		apiOpts = append(apiOpts, api.WithChainMetrics(c.chainMetrics))
	}
	if c.memoryManager != nil {
		apiOpts = append(apiOpts, api.WithMemorySearcher(newMemorySearchAdapter(c.memoryManager.Searcher)))
		apiOpts = append(apiOpts, api.WithMemoryStats(newMemoryStatsAdapter(c.memoryManager)))
		// LLD 22: wire the companion RAG adapter only when the
		// searcher, pipeline, AND repository are live.
		// newMemoryCompanionAdapter returns nil otherwise, which the
		// api.Server reads as "memory subsystem not wired" and surfaces
		// to recall/remember/recent_memory callers as a clean error
		// rather than panicking.
		if adapter := newMemoryCompanionAdapter(
			c.memoryManager.Searcher,
			c.memoryPipeline,
			c.memoryManager.Repository(),
		); adapter != nil {
			apiOpts = append(apiOpts, api.WithMemoryCompanionAdapter(adapter))
		}
		// Cache stats adapter — surfaces both Phase D embedding cache
		// and Phase E response cache through one endpoint
		// (/api/v1/memory/cache-stats) for the vornikctl CLI.
		apiOpts = append(apiOpts, api.WithMemoryCacheStats(newMemoryCacheStatsAdapter(c.memoryManager)))
		// Retrieval-audit repo backs GET /api/v1/projects/{p}/memory/feedback.
		apiOpts = append(apiOpts, api.WithMemoryAuditRepository(c.repos.MemoryRetrievalAudit))
		// Project gist reader backs GET /api/v1/projects/{p}/gist.
		// The consolidate worker UPSERTs rows; this is the read side.
		apiOpts = append(apiOpts, api.WithGistReader(c.memoryManager.Repository()))
	}
	// ArtifactRepository: required by the document-extraction
	// handler (source-artifact lookup) and tolerated by every other
	// surface that doesn't need it. The api.Server pre-existed
	// without this wiring; the document-extraction handler is the
	// first consumer that actually reaches for s.artifactRepo.
	if c.repos != nil && c.repos.Artifacts != nil {
		apiOpts = append(apiOpts, api.WithArtifactRepository(c.repos.Artifacts))
	}
	// Document-extraction pipeline: wire only when every dependency
	// is present. Each missing piece downgrades the endpoint to a
	// 503 rather than crashing other API surfaces.
	if reg := c.ExtractorRegistry(); reg != nil && c.repos != nil && c.repos.ExtractedDocuments != nil {
		var indexer api.ExtractedDocumentIndexer
		if c.memoryManager != nil {
			indexer = newExtractedDocumentIndexerAdapter(c.memoryManager.Indexer)
		}
		apiOpts = append(apiOpts, api.WithExtractorPipeline(
			reg,
			c.ExtractorRunner(),
			c.repos.ExtractedDocuments,
			indexer,
		))
		apiOpts = append(apiOpts, api.WithArtifactOpener(c.artifactStore))
	}
	// REST-side input-artifact snapshot store. Powers the
	// CreateTaskRequest.InputArtifacts → snapshot → auto-extract
	// pipeline. Same *artifacts.Store the dispatcher uses, so
	// snapshots produced by REST callers are immediately visible
	// to chat-driven flows (and vice versa).
	if c.artifactStore != nil {
		apiOpts = append(apiOpts, api.WithInputArtifactStore(c.artifactStore))
	}
	if c.memoryTitleBackfiller != nil {
		apiOpts = append(apiOpts, api.WithMemoryTitleBackfiller(newMemoryTitleBackfillAdapter(c.memoryTitleBackfiller)))
	}
	if c.memoryClassifyBackfiller != nil {
		apiOpts = append(apiOpts, api.WithMemoryClassifyBackfiller(newMemoryClassifyBackfillAdapter(c.memoryClassifyBackfiller)))
	}
	// KG re-flag endpoint — directly wires the chunk-graph repo
	// since the postgres impl already satisfies the narrow
	// MemoryGraphReflagger interface structurally. SQLite repos
	// satisfy it too (same method shape).
	if c.repos != nil && c.repos.ChunkGraphExtraction != nil {
		apiOpts = append(apiOpts, api.WithMemoryGraphReflagger(c.repos.ChunkGraphExtraction))
	}
	// Workflow telemetry rollup — Slice 1 of the memetic-workflows
	// arc. Single Service per daemon; wraps the raw DB.
	if c.DB != nil {
		apiOpts = append(apiOpts, api.WithWorkflowTelemetry(newWorkflowTelemetryAdapter(c.DB)))
	}
	// Workflow architect — Slice 2c of the memetic-workflows arc.
	// Nil-safe: missing chat client / proposals repo / config dir
	// causes the adapter to return nil and the endpoint surfaces
	// 503. ConfigPath points at the deployed configs tree (NOT
	// the source tree, per the two-trees discipline).
	var proposalsRepo persistence.WorkflowProposalRepository
	if c.repos != nil {
		proposalsRepo = c.repos.WorkflowProposals
	}
	// Consumer B (instinct layer) — pass the instinct repo into the
	// architect only when instinct.enabled && consumers.architect_priors.
	// nil keeps the architect's behaviour byte-for-byte unchanged.
	var architectInstincts persistence.InstinctRepository
	if c.Config != nil && c.Config.Instinct.Enabled && c.Config.Instinct.Consumers.ArchitectPriors &&
		c.repos != nil && c.repos.Instincts != nil {
		architectInstincts = c.repos.Instincts
	}
	archAdapter := newWorkflowArchitectAdapter(
		c.DB, c.ChatClient, proposalsRepo,
		resolveRegistryConfigDir(c.ConfigPath),
		architectInstincts,
		// TRACK ARCH-METRIC — thread the shared instinct metrics so the
		// architect's architect-evidence ApplicationsTotal counter is live.
		// Lazily created+cached here (observability is up by this second
		// initHTTPServer pass) and reused by wireComponentMetrics. nil when
		// observability is disabled — counter dark, rows still recorded.
		c.sharedInstinctMetrics(),
		c.Logger,
	)
	if archAdapter != nil {
		apiOpts = append(apiOpts, api.WithWorkflowArchitect(archAdapter))
		// Wire the rejection write-back recorder (Consumer B). The
		// accessor returns nil when the gate is off, so this is a
		// no-op in that case.
		if rec := archAdapter.RejectionRecorder(); rec != nil {
			apiOpts = append(apiOpts, api.WithWorkflowRejectionRecorder(rec))
		}
	}
	// Workflow proposals review surface — Slice 3a of the
	// memetic-workflows arc. Powers GET / GET / POST-decide
	// against /admin/workflow-proposals. Same nil-safety as the
	// architect wiring above; the endpoint surfaces 503 when
	// the proposals repo is missing.
	if proposalsRepo != nil {
		apiOpts = append(apiOpts, api.WithWorkflowProposals(proposalsRepo))
	}
	// Workflow applier — Slice 4 of the memetic-workflows arc.
	// Wires the filesystem writer / git committer / config-reload
	// bridge. Nil-safe: missing prerequisites surface 503 at the
	// endpoint. Built once; shared by the api + ui wirings below.
	workflowApplier := newWorkflowApplier(
		proposalsRepo, c.ConfigReloader,
		resolveRegistryConfigDir(c.ConfigPath),
	)
	if workflowApplier != nil {
		apiOpts = append(apiOpts, api.WithWorkflowApplier(&workflowApplierAdapter{a: workflowApplier}))
	}
	// Self-Healing Workflow Genome v1 — trial runner + promoter (Unit 5).
	// The candidate ledger is wired above; here we add the operator-
	// triggered run-trial / promote / reject actions on top. Both are
	// nil-safe at the endpoint (503 when unwired).
	//
	//   - The trial runner is constructed static-capable: the ReplayEngine
	//     is nil on this deployment, so replay trials return a clean
	//     "engine not wired" verdict and static trials work. There is NO
	//     background auto-trial loop — RunTrial is only reached via the
	//     run-trial endpoint (LLD non-negotiable #5).
	//   - The promoter reuses the memetic Applier (the SAME write→validate→
	//     commit→reload path the proposal-review UI calls) and refuses any
	//     candidate not in trial_passed (LLD non-negotiable #1).
	// uiHealingTrialRunner / uiHealingPromoter capture the UI-seam adapters
	// over the SAME concrete runner/promoter the API wires, so the
	// /ui/admin/blackbox/candidates surface (Unit 6) drives the identical
	// trial + promotion path. Assigned inside the block below; consumed in
	// the UI options block later in this function.
	var uiHealingTrialRunner ui.HealingTrialRunnerUI
	var uiHealingPromoter ui.HealingCandidatePromoterUI
	if c.repos != nil && c.repos.HealingCandidates != nil && c.repos.HealingTrials != nil {
		apiOpts = append(apiOpts, api.WithHealingTrialRepository(c.repos.HealingTrials))
		// ReplayEngine adapter (LLD § "Next slice — ReplayEngine
		// adapter"): wire the production replay path when the Phase-1c
		// HealingApplier seam is available (EE editions, Postgres deployments).
		// c.providers.Healing is nil in Community (seam not wired); non-nil in
		// Enterprise (set in enterprise.Providers()). When non-nil the container
		// upgrades the placeholder with a real DB-backed applier before use.
		// Built only when BOTH concrete deps are non-nil to avoid a
		// typed-nil interface masquerading as wired; otherwise the
		// runner stays static-only (replay trials error cleanly). The
		// candidate-genome side-effect safety is enforced by the
		// counterfactual MCP gate + sideeffects deny-list.
		var replayEngine workflowhealing.ReplayEngine
		if c.providers.Healing != nil {
			// Upgrade the EE marker with the real DB-backed HealingApplier via the
			// Group D BBReplayEngineFactory (Task 6 seam). The factory returns a
			// BBEngineComponents whose HealingApplier is nil when DB or taskCreator
			// is unavailable — the nil-guard below preserves the nil-replayEngine path.
			if comps := c.buildBBEngineComponents(taskCreator); comps.HealingApplier != nil {
				c.providers.Healing = comps.HealingApplier
				replayEngine = workflowhealing.NewReplayEngineAdapter(
					c.providers.Healing,
					c.repos.Tasks,
					c.repos.Executions,
					workflowhealing.ReplayAdapterOptions{},
					c.Logger.With().Str("component", "workflowhealing-replay").Logger(),
				)
			}
		}
		trialRunner := workflowhealing.NewTrialRunner(
			c.repos.HealingCandidates,
			c.repos.HealingTrials,
			replayEngine, // nil → static-only; non-nil → replay-gated promotion enabled
			workflowhealing.GateThresholds{},
			0, // minEvidence: fall back to DefaultMinEvidence.
			c.Logger.With().Str("component", "workflowhealing-trial").Logger(),
		).WithMetrics(c.healingObserverOnce()). // vornik_workflow_healing_trials_total / _trial_duration_seconds (EE only; nil-safe)
							WithRegistrar(c.Registry).            // route candidate-genome replays at a transient workflow id
							WithTriggers(c.repos.HealingTriggers) // empty evidence → fall back to the trigger's evidence set
		// Per-candidate gate thresholds sourced from the overrides repo
		// (keyed by trigger class). No override row → DefaultGateThresholds;
		// nil overrides repo → the runner's static gate.
		if c.repos.HealingOverrides != nil {
			gateResolver := workflowhealing.NewGateThresholdResolver(
				c.repos.HealingOverrides,
				c.Logger.With().Str("component", "workflowhealing-gate").Logger(),
			)
			trialRunner = trialRunner.WithGateResolver(
				newHealingGateResolverAdapter(gateResolver, c.repos.HealingTriggers),
			)
		}
		if runner := newHealingTrialRunnerAdapter(trialRunner); runner != nil {
			apiOpts = append(apiOpts, api.WithHealingTrialRunner(runner))
		}
		uiHealingTrialRunner = newHealingTrialRunnerUIAdapter(trialRunner)
		if workflowApplier != nil && proposalsRepo != nil {
			promoter := workflowhealing.NewPromoter(
				c.repos.HealingCandidates,
				proposalsRepo,
				workflowApplier,
				c.healingObserverOnce(), // RecordPromotion → vornik_workflow_healing_promotions_total (EE only; nil-safe)
				c.Logger.With().Str("component", "workflowhealing-promote").Logger(),
			)
			if p := newHealingPromoterAdapter(promoter); p != nil {
				apiOpts = append(apiOpts, api.WithHealingCandidatePromoter(p))
			}
			uiHealingPromoter = newHealingPromoterUIAdapter(promoter)
		}
	}
	// Synchronous config reload after create-from-template so the new
	// project registers in-memory before the client navigates to it
	// (avoids the file-watcher race → "project not found"). Lazy over
	// c.ConfigReloader so wiring order is irrelevant; nil-reloader
	// deployments fall back to the watcher.
	apiOpts = append(apiOpts, api.WithConfigReloadHook(func() error {
		if c.ConfigReloader == nil {
			return nil
		}
		return c.ConfigReloader.Reload()
	}))
	// Workflow rollbacker — Slice 5 of the memetic-workflows arc.
	// Mirror of the applier wiring; nil-safe at the endpoint.
	workflowRollbacker := newWorkflowRollbacker(
		proposalsRepo, c.ConfigReloader,
		resolveRegistryConfigDir(c.ConfigPath),
	)
	if workflowRollbacker != nil {
		apiOpts = append(apiOpts, api.WithWorkflowRollbacker(&workflowRollbackerAdapter{r: workflowRollbacker}))
	}
	// Phase 2/3 of memory hardening — wire the API endpoints
	// behind /api/v1/projects/{id}/memory/{epochs,rollback,
	// rollbacks,quarantine,health}. Each repo is nil-safe at the
	// handler level, so omitting the migration only loses these
	// surfaces, not the rest of the API.
	if c.memoryQuarantineRepo != nil {
		apiOpts = append(apiOpts, api.WithMemoryQuarantine(c.memoryQuarantineRepo))
	}
	if c.corpusEpochRepo != nil {
		apiOpts = append(apiOpts, api.WithCorpusEpochs(c.corpusEpochRepo))
	}
	if c.ingestQueueRepo != nil {
		apiOpts = append(apiOpts, api.WithIngestQueueRepo(c.ingestQueueRepo))
	}
	// Compose the MCP executor: external project-scoped servers
	// (mcp.Manager) + the built-in document_* tools backed by the
	// extracted_documents repo. Either side may be nil; the
	// composed executor handles partial wiring (e.g. test fixtures
	// without the extractor pipeline).
	var docProvider *api.DocumentToolProvider
	if c.repos != nil && c.repos.ExtractedDocuments != nil && c.repos.Artifacts != nil {
		docProvider = api.NewDocumentToolProvider(api.DocumentToolDeps{
			Repo:         c.repos.ExtractedDocuments,
			ArtifactRepo: c.repos.Artifacts,
		})
	}
	if c.mcpManager != nil || docProvider != nil {
		composed := &api.ComposedMCPExecutor{Builtin: docProvider}
		if c.mcpManager != nil {
			// Assign only when the underlying pointer is non-nil so
			// the interface field doesn't end up as a typed-nil
			// (which would defeat the nil-check inside
			// ComposedMCPExecutor.Tools).
			composed.External = c.mcpManager
		}
		apiOpts = append(apiOpts, api.WithMCPExecutor(composed))
	}
	if c.mcpRegistry != nil {
		// Daemon-level discovery surface. Wires GET /api/v1/mcp/servers
		// + the /ui/mcp inventory page off the same source. Separate
		// from MCPExecutor so the read-only discovery path never
		// accidentally participates in tool invocation.
		apiOpts = append(apiOpts, api.WithMCPRegistry(c.mcpRegistry))
	}
	if c.ChatClient != nil {
		// Wires the internal chat-completions proxy at
		// POST /api/v1/chat/completions so agent containers can route
		// their LLM traffic through the daemon's configured provider.
		apiOpts = append(apiOpts, api.WithChatProvider(c.ChatClient))

		// Explain endpoint: deterministic renderer over the same
		// data sources the (now removed) LLM explainer used. Free
		// + instant — no chat provider needed. The UI's "Post-
		// mortem" button still uses the LLM-backed
		// postmortem.Explainer.Generate path below for prose
		// elaboration; this endpoint is for routine "why did this
		// fail" CLI lookups.
		explainTaskRepo := c.repos.Tasks
		explainExecRepo := c.repos.Executions
		explainAuditRepo := c.repos.ToolAudit
		explainOutcomeRepo := c.repos.StepOutcomes
		var explainLogs postmortem.LogTailFetcher
		if ltf, ok := any(c.Executor).(postmortem.LogTailFetcher); ok {
			explainLogs = ltf
		}
		renderer := postmortem.NewRenderer(
			explainTaskRepo,
			explainExecRepo,
			explainOutcomeRepo,
			explainAuditRepo,
			explainLogs,
		)
		apiOpts = append(apiOpts, api.WithExplainRenderer(renderer))
	}
	// Realtime tool-audit ingest. Backs POST /api/v1/internal/tool-audit
	// — the per-call streaming the agent's entrypoint.sh uses to
	// write each tool invocation as it completes, so a crashed
	// agent doesn't lose its audit trail.
	apiOpts = append(apiOpts, api.WithToolAuditRepository(c.repos.ToolAudit))
	// Phase 24 — conversational task lifecycle. Repos are
	// nil-safe on the API side; if the migration didn't apply the
	// handlers respond 503 TASK_LIFECYCLE_DISABLED.
	apiOpts = append(apiOpts, api.WithTaskMessageRepository(c.repos.Messages))
	apiOpts = append(apiOpts, api.WithTaskScratchpadRepository(c.repos.Scratchpads))
	if c.Scheduler != nil {
		apiOpts = append(apiOpts, api.WithRescheduler(c.Scheduler))
	}
	// Trading order audit (broker→daemon channel). Same realtime-
	// streaming pattern as tool-audit + llm-usage; the broker
	// MCP's AuditWriter posts one row per place_order call. The
	// row joins with the daemon-side equity sampler (writes
	// trading_positions_snapshots every 5 min) to give the
	// project-page soak panel a complete picture.
	apiOpts = append(apiOpts, api.WithTradingOrderRepository(c.repos.TradingOrders))
	// Phase 2 of the broker→daemon audit channel: safety
	// events (kill-switch toggles, breaker trips, cap refusals,
	// replay hits). Same writer + retry machinery on the broker
	// side; same trust boundary on the daemon side.
	apiOpts = append(apiOpts, api.WithTradingSafetyEventRepository(c.repos.TradingSafetyEvents))
	// Phase 3: trading fills. Broker's poll loop streams one
	// row per observed fill; soak panel tiles + per-symbol P&L
	// switch from approximate (limit-price summed) to precise.
	apiOpts = append(apiOpts, api.WithTradingFillRepository(c.repos.TradingFills))
	// Audit T5: the equity-snapshot history feeds the broker's
	// breaker HWM + daily-loss baseline recovery on the
	// trading-state-replay path, so a broker restart doesn't launder
	// a drawdown.
	apiOpts = append(apiOpts, api.WithTradingPositionsSnapshotRepository(c.repos.TradingSnapshots))
	// Trading-series feature-doctor probe: validates each trading-enabled
	// project's equity-sampler series (cadence gaps / duplicate timestamps /
	// staleness / implausible values). DB-only; surface-only (doctor result +
	// the anomaly gauge). Built only when the snapshot history + registry exist
	// AND the edition provides the probe factory. The probe itself relocated to
	// internal/enterprise/trading in Phase 2c; Community leaves the factory nil
	// so the trading-series surface reports "not available" (no behaviour change
	// — CE has no trading subsystem populating the snapshot series either way).
	if c.providers.TradingSeriesProbeFactory != nil && c.repos.TradingSnapshots != nil && c.Registry != nil {
		var record func(projectID, code string, count int)
		if c.tradingSeriesMetrics != nil {
			record = c.tradingSeriesMetrics.Set
		}
		tradingProbe := c.providers.TradingSeriesProbeFactory(c.Registry, c.repos.TradingSnapshots, record, c.Logger)
		apiOpts = append(apiOpts, api.WithFeatureTradingProbe(tradingProbe))
	}
	// HMAC request-authentication on the /internal/trading-* endpoints
	// (backlog: "HMAC/mTLS on /internal/trading-*"). Wired only when
	// trading.auth.enabled — otherwise the verifier stays nil and the
	// endpoints keep bearer-only auth (backward-compatible rollout).
	if c.Config != nil && c.Config.Trading.Auth.Enabled {
		skew := tradingauth.DefaultClockSkew
		if c.Config.Trading.Auth.ClockSkew != "" {
			if d, err := time.ParseDuration(c.Config.Trading.Auth.ClockSkew); err == nil {
				skew = d
			}
		}
		apiOpts = append(apiOpts, api.WithTradingAuthVerifier(
			tradingauth.NewVerifier(c.Config.Trading.Auth.Secret, skew)))
	}
	// Per-project trading-order rate limit (backlog: "per-project
	// rate-limit before a 2nd trading project"). Dedicated in-process
	// limiter — single-daemon trading scope makes the postgres backend
	// unnecessary here. Caps come from each project's trading_rate_limit
	// block; a zero-cap project is unlimited.
	apiOpts = append(apiOpts, api.WithTradingRateLimiter(ratelimit.New()))
	// Pass the pricing path so GET /api/v1/models can crosswalk the
	// discovered model list. Same path the doctor handler uses.
	// Also feeds the chat-proxy recordChatAPIUsage helper that
	// books cost on third-party /v1/* + /api/chat calls.
	if pricingPath := resolvePricingPath(c.ConfigPath); pricingPath != "" {
		apiOpts = append(apiOpts, api.WithPricingPath(pricingPath))
	}
	// External-API billing project. When set, third-party chat-
	// proxy calls land under this project on the spend dashboard
	// instead of the synthetic "_external" bucket.
	if pid := strings.TrimSpace(c.Config.API.ExternalAPIBillingProjectID); pid != "" {
		apiOpts = append(apiOpts, api.WithExternalAPIBillingProjectID(pid))
	}
	if mode := strings.TrimSpace(c.Config.Chat.PromptCacheMode); mode != "" {
		apiOpts = append(apiOpts, api.WithPromptCacheMode(mode))
	}
	// Budget breach → Telegram alerts. The Bot implements budget.Notifier
	// (see NotifyBudgetBreach). When no telegram bot is wired the option
	// is skipped and the handlers keep the log-only behaviour.
	if c.TelegramBot != nil {
		apiOpts = append(apiOpts, api.WithBudgetNotifier(c.TelegramBot))
		apiOpts = append(apiOpts, api.WithFillNotifier(c.TelegramBot))
	}

	// RelayMode (DMZ): build the mTLS relay client from node.relay.* PEM
	// files and inject it into the API server. c.webhookRelayClient is
	// stored so Task 8 can register the relay_upstream readiness probe.
	// This block runs BEFORE the readiness-check section below so the
	// client is populated when the probe closure is registered.
	caps := c.capabilities()
	// Phase 2c: the relay client is built by the EE ClusteringProvider (it owns
	// the webhookrelay package now). A RelayMode CE build has no clustering
	// provider, so it constructs no relay client — but a relay node is an EE
	// deployment by definition (clustering feature), so cp is non-nil there.
	if caps.RelayMode && c.providers.Clustering != nil {
		relayClient, err := c.providers.Clustering.NewWebhookRelayClient(c.Config.Node.Relay)
		if err != nil {
			return fmt.Errorf("build webhook relay client: %w", err)
		}
		c.webhookRelayClient = relayClient // stored for the readiness probe (Task 8)
		apiOpts = append(apiOpts, api.WithWebhookRelay(relayClient))
	}

	// Deep /readyz checks. Each dependency the daemon relies on
	// registers a cheap bounded probe; /readyz runs them all under a
	// 3 s deadline and returns 503 on the first failure. Without this
	// the daemon could be wedged (stalled scheduler, dead subscription
	// token) and still report healthy.
	if caps.RunWorkers && c.Scheduler != nil {
		sched := c.Scheduler
		apiOpts = append(apiOpts, api.WithReadinessCheck("scheduler_heartbeat", func(ctx context.Context) error {
			last := sched.LastTick()
			if last.IsZero() {
				// Scheduler has not ticked yet — acceptable during the
				// first PollInterval of startup. Flag only once we've
				// definitely missed a tick.
				return nil
			}
			poll := sched.PollInterval()
			if poll <= 0 {
				poll = 30 * time.Second
			}
			// Allow 2× interval + 5 s leeway so a busy tick doesn't
			// flap the probe. Scheduler ticks should be sub-second in
			// steady state.
			threshold := 2*poll + 5*time.Second
			if age := time.Since(last); age > threshold {
				return fmt.Errorf("scheduler last ticked %s ago (> %s)", age.Round(time.Second), threshold)
			}
			return nil
		}))
	}
	if c.ChatClient != nil {
		// Chat provider readiness: a quick reach test against whatever
		// backend is configured. We do NOT call the LLM — that's
		// expensive and flaky. Instead we invoke the provider's Pinger
		// (chat.Pinger) when it implements one: HTTP clients GET
		// /v1/models, CLI subprocesses exec `--version`, subscription
		// clients parse the on-disk auth token. No tokens consumed; a
		// nil error confirms the provider can serve real completions.
		// Providers that don't implement Pinger fall back to a nil
		// reference check — the historical behaviour.
		chatRef := c.ChatClient
		apiOpts = append(apiOpts, api.WithReadinessCheck("chat_provider", func(ctx context.Context) error {
			if chatRef == nil {
				return fmt.Errorf("chat provider reference is nil")
			}
			if pg, ok := chatRef.(chat.Pinger); ok {
				probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				return pg.Ping(probeCtx)
			}
			return nil
		}))
	}
	if caps.RunWorkers && c.retentionDone != nil {
		doneCh := c.retentionDone
		apiOpts = append(apiOpts, api.WithReadinessCheck("retention_sweeper", func(ctx context.Context) error {
			select {
			case <-doneCh:
				return fmt.Errorf("retention sweeper has stopped")
			default:
				return nil
			}
		}))
	}
	// DB readiness is already covered by the handler's taskRepo-based
	// "database" check (api/handlers.go), which is nil-guarded — a no-DB
	// webhook/relay node has no taskRepo and so registers no DB check.
	// (Slice B finalizes the DB-less webhook node construction.)
	if caps.RelayMode && c.webhookRelayClient != nil {
		relayClient := c.webhookRelayClient
		apiOpts = append(apiOpts, api.WithReadinessCheck("relay_upstream", func(ctx context.Context) error {
			probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			return relayClient.Ping(probeCtx)
		}))
	}
	// Slice 4D of the ConversationChannel rollout — construct the
	// GitHub App channel from per-project config and mount its
	// webhook handler. Channel is one-per-daemon today; future
	// per-project channels are gated on the design doc's
	// multi-installation open question. No project configured →
	// nothing to mount, the route 404s.
	//
	// Slice 4F (2026-05-17): inject a TaskCreator built from the
	// daemon's shared task repository so issues.labeled +
	// pull_request.opened events actually land tasks. The closure
	// receives the picked project (the channel is pinned to one
	// project today) and returns nil when the task repo isn't yet
	// available — early-boot test paths and SQLite-only setups
	// fall back to the no-op log path the channel inherits.
	var ghProjects []*registry.Project
	if c.Registry != nil {
		ghProjects = c.Registry.ListProjects()
	}
	var ghTaskRepo persistence.TaskRepository
	if c.repos != nil {
		ghTaskRepo = c.repos.Tasks
	}
	ghChannel, ghEnabledProjects, err := buildGitHubChannelWithTaskCreator(
		ghProjects,
		taskCreatorFromRepo(ghTaskRepo, nil, c.Logger.With().Str("component", "github_task_creator").Logger()),
	)
	if err != nil {
		return fmt.Errorf("github-app channel: %w", err)
	}
	if ghChannel != nil {
		c.GitHubChannel = ghChannel
		c.GitHubProjects = ghEnabledProjects
		// Back-compat: GitHubProject mirrors the single-installation
		// pin when exactly one project is configured. Multi-install
		// deployments leave it nil; the per-session project routing
		// is the source of truth in that mode.
		if len(ghEnabledProjects) == 1 {
			c.GitHubProject = ghEnabledProjects[0]
		}
		logEvt := c.Logger.Info().
			Int("installations", len(ghEnabledProjects))
		if len(ghEnabledProjects) == 1 {
			p := ghEnabledProjects[0]
			logEvt = logEvt.
				Str("project_id", p.ID).
				Int64("app_id", p.GitHubApp.AppID).
				Int64("installation_id", p.GitHubApp.InstallationID).
				Bool("outbound_configured", p.GitHubApp.PrivateKeyPath != "")
		} else {
			ids := make([]string, 0, len(ghEnabledProjects))
			for _, p := range ghEnabledProjects {
				ids = append(ids, p.ID)
			}
			logEvt = logEvt.Strs("project_ids", ids)
		}
		logEvt.Msg("github-app channel constructed")
		apiOpts = append(apiOpts, api.WithGitHubAppWebhookHandler(ghChannel.HandleWebhook))
	}

	// Slice 1 of the email ConversationChannel rollout: construct
	// the channel from per-project config. Like the GitHub channel,
	// one-per-daemon today (first project with an `email` block
	// wins). The Start goroutine that drives the IMAP poll loop is
	// spawned later in lifecycle by Container.Start so the
	// dispatcher is bound before inbound delivery begins.
	var emailProjects []*registry.Project
	if c.Registry != nil {
		emailProjects = c.Registry.ListProjects()
	}
	var artifactRepoForEmail persistence.ArtifactRepository
	if c.repos != nil {
		artifactRepoForEmail = c.repos.Artifacts
	}
	// Build the email auto-extractor only when every dependency
	// is present. nil flows through as "no auto-extraction" —
	// channel falls back to attachment-persist-only behaviour.
	var emailAutoExtractor email.AttachmentAutoExtractor
	if reg := c.ExtractorRegistry(); reg != nil && c.repos != nil && c.repos.ExtractedDocuments != nil {
		var indexer *memory.Indexer
		if c.memoryManager != nil {
			indexer = c.memoryManager.Indexer
		}
		emailAutoExtractor = newEmailAutoExtractor(
			reg,
			c.ExtractorRunner(),
			c.repos.ExtractedDocuments,
			indexer,
			c.artifactStore,
			c.Logger.With().Str("component", "email-extractor").Logger(),
		)
	}
	emChannels, emProjects, err := buildEmailChannels(emailProjects, artifactRepoForEmail, c.emailAttachmentDir(), emailAutoExtractor)
	if err != nil {
		return fmt.Errorf("email channel: %w", err)
	}
	if len(emChannels) > 0 {
		c.EmailChannels = emChannels
		c.EmailProjects = emProjects
		for i, ch := range emChannels {
			p := emProjects[i]
			_ = ch // channel construction is logged per-project below
			c.Logger.Info().
				Str("project_id", p.ID).
				Str("imap_host", p.Email.IMAPHost).
				Bool("outbound_configured", p.Email.SMTPHost != "").
				Bool("auth_verifier_on", p.Email.VerifyInboundAuth).
				Msg("email channel constructed")
		}
		// Wire the send_email tool's backend now that the per-project
		// channels exist. The dispatcher agent was built earlier in
		// boot (before channels), so we late-bind via SetEmailSender.
		if c.Dispatcher != nil {
			if sender := newEmailSender(adaptChannels(emChannels), emProjects); sender != nil {
				c.Dispatcher.SetEmailSender(sender)
				c.Logger.Info().
					Int("projects", len(emProjects)).
					Msg("email: send_email dispatcher tool enabled")
			}
		}
	}

	// Slack ConversationChannel — one per project with a configured
	// `slack` block. Each gets its own Events API webhook handler
	// gated by its own signing secret + per-workspace allowlists.
	// The MuxHandler fans the single /api/v1/slack/webhook route out
	// to the right channel by team_id; per-channel dispatcher
	// receivers are spawned later in lifecycle by Container.Start so
	// the dispatcher is bound before inbound delivery begins.
	var slackProjects []*registry.Project
	if c.Registry != nil {
		slackProjects = c.Registry.ListProjects()
	}
	skChannels, skProjects, err := buildSlackChannels(slackProjects, c.voiceSTT, c.voiceTTS)
	if err != nil {
		return fmt.Errorf("slack channel: %w", err)
	}
	if len(skChannels) > 0 {
		c.SlackChannels = skChannels
		c.SlackProjects = skProjects
		muxLogger := c.Logger.With().Str("component", "slack_mux").Logger()
		apiOpts = append(apiOpts, api.WithSlackWebhookHandler(
			slack.NewMuxHandler(skChannels, muxLogger).ServeHTTP,
		))
		for i, ch := range skChannels {
			p := skProjects[i]
			_ = ch
			c.Logger.Info().
				Str("project_id", p.ID).
				Str("team_id", p.Slack.TeamID).
				Bool("outbound_configured", p.Slack.BotTokenEnv != "").
				Int("channel_allowlist_size", len(p.Slack.ChannelAllowlist)).
				Int("sender_allowlist_size", len(p.Slack.SenderAllowlist)).
				Msg("slack channel constructed")
		}
	}

	// Browser login (github-login phase 3). Postgres-gated; nil when
	// not configured or not supported on this backend. Built before
	// the api server so the session backend can join its auth chain.
	sessionLogin := c.buildSessionLogin()
	if sessionLogin != nil {
		apiOpts = append(apiOpts, api.WithServerSessionBackend(sessionLogin.backend))
	}

	apiServer := api.NewServer(apiOpts...)
	c.apiServer = apiServer
	// Set config handlers if reloader is available
	if c.ConfigReloader != nil {
		api.SetConfigHandlers(api.NewConfigHandlers(c.ConfigReloader))
	}
	// Doctor endpoint for vornikctl doctor
	var doctorH *api.DoctorHandlers
	if c.DB != nil {
		dh := api.NewDoctorHandlers(c.DB)
		if c.Registry != nil {
			dh.SetConfigDir(c.Registry.GetConfigDir())
		}
		dh.SetServerConfig(c.Config)
		dh.SetConfigPath(c.ConfigPath)
		dh.SetPricingPath(resolvePricingPath(c.ConfigPath))
		if c.repos != nil && c.repos.LeaderLocks != nil {
			dh.SetLeaderLockRepository(c.repos.LeaderLocks)
		}
		api.SetDoctorHandlers(dh)
		doctorH = dh
	}
	// Wire the support-report collectors now that the apiServer (and,
	// when DB-backed, the doctor handler) exist — the doctor + health
	// adapters depend on the fully-built server. A nil doctorH leaves
	// the doctor section to degrade gracefully (best-effort, §7).
	// See https://docs.vornik.io
	c.wireSupportReportCollectors(apiServer, doctorH)
	mux.Handle("/", apiServer.Routes())
	// APIMetrics is constructed inside Routes() — after that call
	// the server pointer has the registered counter set, so the
	// doctor handlers can read live attribution counts. Wired
	// post-Routes to avoid an ordering dance inside the api
	// package.
	if c.DB != nil && apiServer.APIMetrics() != nil {
		api.WireDoctorAPIMetrics(apiServer.APIMetrics())
	}
	// Wire the API Server back-reference and the ConfigReloader into the
	// singleton DoctorHandlers so feature-doctor read endpoints can build
	// featuredoctor.Deps, and the enable endpoint can write+reload.
	api.WireDoctorServer(apiServer)
	if c.ConfigReloader != nil {
		api.WireDoctorReloader(c.ConfigReloader)
	}

	// UI Server - Web dashboard
	// Pre-init the archive-sweeper so the "Delete now" button can
	// kick it. Run()'s loop later starts the actual goroutine; the
	// pre-init only constructs the value object.
	if c.archiveSweeper == nil {
		c.archiveSweeper = c.initArchiveSweeper()
	}
	// Shared archive lifecycle service. UI handlers + REST API both
	// hold pointers to this same instance so YAML mutations land
	// through one code path. The UI package provides the patcher
	// closure to avoid an import cycle (projectarchive→ui).
	if c.archiveLifecycle == nil {
		c.archiveLifecycle = c.initArchiveLifecycle(ui.LifecyclePatcher())
	}
	uiOpts := []ui.ServerOption{
		ui.WithLogger(c.Logger.With().Str("component", "ui").Logger()),
		ui.WithTaskRepository(c.repos.Tasks),
		ui.WithExecutionRepository(c.repos.Executions),
		ui.WithArtifactRepository(c.repos.Artifacts),
		// Route UI blob reads (changelog inline render +
		// /artifacts/{id} download) through the backend-aware
		// Store. Without this, the S3 backend can't serve any
		// artifact via the dashboard.
		ui.WithArtifactReader(c.artifactStore),
		ui.WithProjectRegistry(c.Registry),
		ui.WithToolAuditRepository(c.repos.ToolAudit),
		ui.WithWebhookEventRepository(c.repos.Webhooks),
		ui.WithAPIKeyRepository(c.repos.APIKeys),
		ui.WithLLMUsageRepository(c.repos.LLMUsage),
		ui.WithStepOutcomeRepository(c.repos.StepOutcomes),
		ui.WithJudgeVerdictRepository(c.repos.JudgeVerdicts),
		ui.WithRecoveryEventRepository(c.repos.RecoveryEvents),
		ui.WithTradingSnapshotRepository(c.repos.TradingSnapshots),
		ui.WithTradingOrderRepository(c.repos.TradingOrders),
		ui.WithTradingSafetyRepository(c.repos.TradingSafetyEvents),
		ui.WithTradingFillRepository(c.repos.TradingFills),
		ui.WithPostMortemRepository(c.repos.PostMortems),
		// Continuous-learning Consumer A (slice 3): advisory "similar
		// failures here resolved by …" panel on the failed-task page.
		// Double-gated — instinct.enabled AND
		// instinct.consumers.failure_playbooks must both be on; with
		// either off the panel never renders and the page is unchanged.
		ui.WithInstinctPlaybooks(
			c.repos.Instincts,
			c.Config != nil && c.Config.Instinct.Enabled && c.Config.Instinct.Consumers.FailurePlaybooks,
		),
		// Phase C — multi-hop replay tree dependencies.
		ui.WithCrossProjectCallRepository(c.repos.CrossProjectCalls),
		ui.WithReminderRepository(c.repos.Reminders),
		// Autonomy Black Box read-side service — built via the EE BBTraceServiceFactory
		// (Task 6 seam); nil in Community/SQLite (UI page hides the section).
		ui.WithBlackBoxService(c.buildBBTraceService()),
		ui.WithMemoryPolicyEvaluations(c.repos.MemoryPolicyEvaluations),
		ui.WithMemoryFirewallMode(c.memoryFirewallMode()),
		ui.WithMemoryFirewallEditor(c.uiFirewallEditor()),
		// Phase B triggers panel — list lives behind the same
		// repo the detector writes to. Nil-safe (SQLite gets the
		// section hidden).
		ui.WithHealingTriggerRepository(c.repos.HealingTriggers),
		ui.WithHealingOverrideRepository(c.repos.HealingOverrides),
		// Self-Healing Workflow Genome v1 candidate + trial views (Unit 6).
		// Nil-safe — SQLite deployments get the candidates page empty state.
		ui.WithHealingCandidateRepository(c.repos.HealingCandidates),
		ui.WithHealingTrialRepository(c.repos.HealingTrials),
		ui.WithProjectSpawnRepository(c.repos.ProjectSpawns),
		ui.WithArtifactBasePath(c.Config.Storage.ArtifactsPath),
		// Per-project filesystem-artifact management surface. Reads
		// from runtime.project_workspace_path so the /ui/projects/{id}/artifacts
		// page can list / view / delete files under the project's
		// persistent workspace tree. Empty config disables the page.
		ui.WithProjectWorkspaceRoot(c.Config.Runtime.ProjectWorkspacePath),
		ui.WithRateLimiter(c.rateLimiter),
		// /ui/projects/{id}/documents listing — Phase 6.
		ui.WithExtractedDocumentsRepository(func() persistence.ExtractedDocumentRepository {
			if c.repos == nil {
				return nil
			}
			return c.repos.ExtractedDocuments
		}()),
		// /ui/projects/{id}/documents/{docID}/re-extract POST — same
		// extraction pipeline the email + dispatcher + REST hooks use.
		ui.WithDocumentReExtractor(func() ui.DocumentReExtractor {
			reg := c.ExtractorRegistry()
			if reg == nil || c.repos == nil || c.repos.ExtractedDocuments == nil || c.repos.Artifacts == nil {
				return nil
			}
			var indexer *memory.Indexer
			if c.memoryManager != nil {
				indexer = c.memoryManager.Indexer
			}
			return newUIDocumentReExtractor(
				reg, c.ExtractorRunner(), c.repos.ExtractedDocuments, c.repos.Artifacts, indexer,
				c.artifactStore,
				c.Logger.With().Str("component", "ui-doc-reextract").Logger(),
			)
		}()),
		// Shared rate-limit primitives: same instances the API
		// subtree enforces with. UI reads bucket levels + recent
		// event ring to render the homepage "approaching limit"
		// banner without scraping Prometheus.
		ui.WithAPIKeyLimiter(c.apiKeyLimiter),
		ui.WithRateLimitMetrics(c.rateLimitMetrics),
		ui.WithTaskCreator(taskCreator),
		// Landing-page tiles: autonomy ETA projections + active-chats
		// counter. Both optional; the dashboard hides tiles whose
		// data source isn't wired.
		ui.WithAutonomyEvaluationRepository(c.repos.AutonomyEvaluations),
		// Auth-off operator fallback — same value the wizard handlers
		// stamp on requests that arrive without X-Operator-Id. Empty
		// API config falls back to the daemon default `local:dev`;
		// auth-enabled deployments ignore it and use the verified
		// principal only.
		ui.WithSingleTenantOperatorID(api.SingleTenantOperatorIDFromConfig(c.Config)),
		// Git-over-HTTPS panel: derive the full clone URL from the
		// operator-configured public base URL. Empty string is handled
		// gracefully — the panel renders a relative path + hint.
		ui.WithPublicBaseURL(c.Config.Server.PublicBaseURL),
	}
	// Edition gate — the /trading dashboard route. Registered only when the EE
	// trading capability is present; Community builds 404 /trading instead of
	// leaking the dashboard (the trading-* repos above stay wired for the
	// audit-ingest path regardless of edition).
	if c.providers.Trading != nil {
		uiOpts = append(uiOpts, ui.WithTradingEnabled())
	}
	if c.archiveSweeper != nil {
		uiOpts = append(uiOpts, ui.WithArchiveSweeper(c.archiveSweeper))
	}
	if c.archiveLifecycle != nil {
		uiOpts = append(uiOpts, ui.WithArchiveLifecycle(&ui.LifecycleServiceAdapter{Service: c.archiveLifecycle}))
	}
	if c.mcpRegistry != nil {
		// /ui/mcp daemon-level discovery page. Same source the
		// API server uses for /api/v1/mcp/servers — keeping the
		// two surfaces coherent.
		uiOpts = append(uiOpts, ui.WithMCPRegistry(c.mcpRegistry))
		// Project config form's MCP servers section: adapt the
		// same registry to the form's narrower interface so
		// operators see the daemon-known servers + subscribe via
		// per-project checkboxes instead of hand-editing YAML.
		uiOpts = append(uiOpts, ui.WithMCPFormRegistrySource(&mcpFormRegistryAdapter{registry: c.mcpRegistry}))
	}
	if c.TelegramBot != nil {
		uiOpts = append(uiOpts, ui.WithBudgetNotifier(c.TelegramBot))
		uiOpts = append(uiOpts, ui.WithActiveChatSource(c.TelegramBot))
	}
	// Project-template gallery — same catalog the API server uses.
	// Without it the /ui/projects/new page renders a "no catalog
	// installed" empty state.
	if projectTemplatesCatalog != nil {
		uiOpts = append(uiOpts, ui.WithProjectTemplates(projectTemplatesCatalog))
	}
	if projectTemplatesConfigsDir != "" {
		uiOpts = append(uiOpts, ui.WithConfigsDir(projectTemplatesConfigsDir))
	}

	// Retention defaults — surfaced on the per-project page so
	// operators can see which fields are inherited vs overridden.
	// Reads from the same config block the sweeper consumes so
	// the panel can't drift from what's actually being applied.
	uiOpts = append(uiOpts, ui.WithRetentionDefaults(registry.ProjectRetention{
		TaskLLMUsageDays: c.Config.Retention.TaskLLMUsageDays,
		ToolAuditDays:    c.Config.Retention.ToolAuditDays,
		TasksDays:        c.Config.Retention.TasksDays,
		ExecutionsDays:   c.Config.Retention.ExecutionsDays,
		ArtifactsDays:    c.Config.Retention.ArtifactsDays,
	}))
	// Retention preview — dry-run estimate panel (P1 UI batch).
	// Wraps a fresh retention.Sweeper (stateless; safe alongside
	// the background sweeper goroutine) so the preview reflects
	// the same policy + tables as the next real sweep.
	if c.DB != nil {
		uiOpts = append(uiOpts, ui.WithRetentionPreviewer(newRetentionPreviewAdapter(
			retention.New(c.DB, c.Logger.With().Str("component", "retention-preview").Logger()),
			c.Config.Retention,
			c.Config.Storage.ArtifactsPath,
			c.Registry,
		)))
	}
	// Phase 26+27 — conversational task lifecycle. Wires the UI's
	// conversation pane + per-action POST handlers.
	uiOpts = append(uiOpts, ui.WithTaskMessageRepository(c.repos.Messages))
	uiOpts = append(uiOpts, ui.WithTaskScratchpadRepository(c.repos.Scratchpads))
	// 2026-05-26 unified-timeline refactor: surface task-scoped
	// steering hints alongside messages in the conversation thread.
	uiOpts = append(uiOpts, ui.WithHintRepository(c.repos.ExecutionHints))
	if c.Scheduler != nil {
		uiOpts = append(uiOpts, ui.WithRescheduler(c.Scheduler))
	}
	// Phase 2-4 memory hardening surfaces — populate the /ui/memory
	// section. Each is nil-safe.
	uiOpts = append(uiOpts, ui.WithMemoryConfigured(c.Config.Memory.Enabled))
	if c.memoryQuarantineRepo != nil {
		uiOpts = append(uiOpts, ui.WithMemoryQuarantineRepository(c.memoryQuarantineRepo))
	}
	if c.corpusEpochRepo != nil {
		uiOpts = append(uiOpts, ui.WithCorpusEpochRepository(c.corpusEpochRepo))
	}
	if c.ingestQueueRepo != nil {
		uiOpts = append(uiOpts, ui.WithIngestQueueRepository(c.ingestQueueRepo))
	}
	if c.Config.Memory.Graph.Enabled {
		uiOpts = append(uiOpts, ui.WithChunkGraphRepository(c.repos.ChunkGraphExtraction))
		// Knowledge-graph READ surface for the entity browser /
		// detail / subgraph pages (LLD §7). *graph.Searcher
		// satisfies ui.KnowledgeGraphReader directly. nil-safe —
		// the pages render a "not enabled" state without it.
		// see https://docs.vornik.io §7
		if gs := c.newGraphSearcher(); gs != nil {
			uiOpts = append(uiOpts, ui.WithKnowledgeGraphReader(gs))
		}
	}
	if c.memoryManager != nil && c.memoryManager.Repository() != nil {
		uiOpts = append(uiOpts, ui.WithVectorVizSource(
			newVectorVizAdapter(memory.NewVizSource(c.memoryManager.Repository())),
		))
		// Hard-eviction surface + audit-log viewer. The corrector
		// wraps the same Repository — Searcher isn't needed for
		// evict (it operates on explicit chunk IDs).
		corrector := memory.NewCorrector(c.memoryManager.Repository(), nil)
		uiOpts = append(uiOpts, ui.WithMemoryEvictor(
			newMemoryEvictorAdapter(corrector, c.memoryManager.Repository()),
		))
		// Embedding cache stats — postgres-backed cache only.
		// Adapter returns nil when the EmbedCache is the disabled
		// stub (no Cache field set) so the spend panel just shows
		// the disabled placeholder.
		if c.memoryManager.Embedder != nil && c.memoryManager.Embedder.Cache != nil {
			if src := newEmbeddingCacheStatsAdapter(c.memoryManager.Embedder.Cache); src != nil {
				uiOpts = append(uiOpts, ui.WithEmbeddingCacheStatsSource(src))
			}
		}
		// Phase E response cache stats — opt-in via
		// Memory.ResponseCacheEnabled. Adapter degrades to nil
		// when the cache isn't wired.
		if c.memoryManager.ResponseCache != nil {
			if src := newResponseCacheStatsAdapter(c.memoryManager.ResponseCache); src != nil {
				uiOpts = append(uiOpts, ui.WithResponseCacheStatsSource(src))
			}
		}
	}
	// Wizard drafts banner (Feature #2 Phase C). nil-safe — banner
	// hides if the wizard sessions repo isn't wired. Gated on the chat
	// provider being wired (the wizard's converse turn needs an LLM):
	// without chat the wizard feature is off, so stale draft rows stay
	// hidden instead of nagging the operator to resume a draft they
	// can't open.
	if c.repos != nil && c.repos.ProjectWizardSessions != nil {
		uiOpts = append(uiOpts, ui.WithWizardSessionLister(c.repos.ProjectWizardSessions))
	}
	if c.ChatClient != nil {
		uiOpts = append(uiOpts, ui.WithWizardEnabled(true))
	}
	{
		var sessions persistence.InstallationOnboardingSessionRepository
		if c.repos != nil {
			sessions = c.repos.InstallationOnboardingSessions
		}
		uiOpts = append(uiOpts, ui.WithOnboardingDetector(onboarding.Detector{
			Sessions: sessions,
			Config:   c.Config,
		}))
	}
	if c.memoryManager != nil && c.memoryManager.Searcher != nil {
		uiOpts = append(uiOpts, ui.WithMemorySearcher(
			newUIMemorySearchAdapter(c.memoryManager.Searcher),
		))
	}
	if c.memoryPipeline != nil {
		uiOpts = append(uiOpts, ui.WithPipelineDryRunner(
			newPipelineDryRunAdapter(c.memoryPipeline),
		))
	}
	if exp := c.buildPostMortemExplainer(); exp != nil {
		uiOpts = append(uiOpts, ui.WithPostMortemExplainer(exp))
		c.Logger.Info().Msg("post-mortem explainer wired")
	}
	if c.ConfigReloader != nil {
		uiOpts = append(uiOpts, ui.WithConfigReloader(c.ConfigReloader))
	}
	if c.Executor != nil {
		uiOpts = append(uiOpts, ui.WithExecutor(uiExecutorAdapter{c.Executor}))
	}
	// Shared per-project workspace lock — same instance the executor +
	// api server hold. The UI artifact-delete path takes it around its
	// git add+commit so deletes serialise per project with execution.
	if c.WorkspaceLock != nil {
		uiOpts = append(uiOpts, ui.WithWorkspaceLock(c.WorkspaceLock))
	}

	// Phase 2 — prompt-writing assistant. Wire whatever
	// chat.Provider the daemon initialised (HTTP, claude-cli,
	// codex-cli, claude-subscription, codex-subscription,
	// router) behind ui.AssistantLLM. The provider path works
	// for every backend the rest of the daemon supports —
	// previously this wiring built a raw OpenAI HTTP client
	// from chat.endpoint + chat.api_key, which left
	// subscription / CLI operators with the 503 "assistant not
	// configured" message even when their chat layer was
	// working. See https://docs.vornik.io
	// (phase 2).
	if c.ChatClient != nil {
		adapter, err := ui.NewProviderAssistant(c.ChatClient)
		if err != nil {
			c.Logger.Warn().Err(err).Msg("ui prompt-writing assistant: provider adapter failed; assistant disabled")
		} else {
			uiOpts = append(uiOpts, ui.WithAssistantLLM(adapter))
			c.Logger.Info().
				Str("provider_model", c.ChatClient.Model()).
				Msg("ui prompt-writing assistant wired to chat provider")
		}
	}
	if c.Config.Chat.Model != "" {
		uiOpts = append(uiOpts, ui.WithAssistantDefaultModel(c.Config.Chat.Model))
	} else if c.ChatClient != nil {
		// Fall back to whatever model the provider was
		// constructed with (subscription providers don't
		// surface a Chat.Model in the YAML).
		uiOpts = append(uiOpts, ui.WithAssistantDefaultModel(c.ChatClient.Model()))
	}
	if c.pricingTable != nil {
		uiOpts = append(uiOpts, ui.WithAssistantPricing(c.pricingTable))
	}
	// Web chat surface — /ui/projects/<id>/chat. Wires the same
	// dispatcher.Agent the telegram bot consumes, so /ui chat
	// turns get the full tool-calling loop. nil-safe: the UI
	// chat page renders a "chat not configured" banner when no
	// dispatcher is wired.
	if c.Dispatcher != nil {
		uiOpts = append(uiOpts, ui.WithChatDispatcher(c.Dispatcher))
	}
	// DB-backed webchat session store. Each per-project
	// SessionStore the UI lazily constructs will write-through
	// every post-turn history slice so a daemon restart or
	// replica failover doesn't drop the in-flight conversation.
	// nil persister (SQLite single-process or unwired repo) is
	// safe — channelSessionPersister returns nil, the UI's
	// SetPersister call no-ops, behaviour matches pre-feature.
	if p := c.channelSessionPersister("webchat"); p != nil {
		uiOpts = append(uiOpts, ui.WithChatSessionPersister(p))
	}
	// Externally-reachable base URL — same value the telegram
	// bot uses for /start onboarding deep links. Used by the
	// chat surface to render absolute deliverable-link URLs.
	if c.Config.Telegram.WebUIBaseURL != "" {
		uiOpts = append(uiOpts, ui.WithWebUIBaseURL(c.Config.Telegram.WebUIBaseURL))
	}

	// Edition gate (outer) — Admin UI surface.
	// Inner nil-checks on each repo are the existing inner gates;
	// they stay unchanged. The outer c.providers.Admin gate is the
	// edition-level switch that Community builds keep false.
	if c.providers.Admin {
		uiOpts = append(uiOpts, c.adminUIOptions(adminUIDeps{
			apiServer:          apiServer,
			workflowApplier:    workflowApplier,
			workflowRollbacker: workflowRollbacker,
			archAdapter:        archAdapter,
			healingTrialRunner: uiHealingTrialRunner,
			healingPromoter:    uiHealingPromoter,
		})...)
	} // end providers.Admin

	// Browser-login UI wiring (github-login phase 3) — provider
	// buttons + the logout handler. Only when session login is
	// configured + supported (Postgres).
	if sessionLogin != nil {
		uiOpts = append(uiOpts,
			ui.WithLoginProviders(sessionLogin.providerNames),
			ui.WithLogoutHandler(sessionLogin.logoutHandler),
		)
	}

	if c.capabilities().ServeUI {
		uiServer := ui.NewServer(uiOpts...)
		uiHandler := wrapUIAdminGate(
			c.Logger.With().Str("component", "ui").Logger(),
			c.Config.Admin,
			uiServer.Handler(),
		)

		// The UI tree exposes state-changing endpoints (project config save,
		// task cancel/retry, autonomy toggle) and must require the same API
		// key and per-project scope the data-plane API requires. Without
		// these wraps any caller that can reach the daemon port can POST to
		// /ui/projects/{id}/config and inject arbitrary YAML, or use a key
		// scoped to one project to mutate another project's UI routes.
		// Wire the same DB-backed API-key surface the api router uses so
		// the UI and API agree on which bearer tokens authenticate.
		uiAPIKeyRepo := c.repos.APIKeys
		uiHandler = api.ProjectAuthMiddleware()(uiHandler)
		uiAuthOpts := []api.AuthConfigOption{
			api.WithAPIKeyLookup(uiAPIKeyRepo),
			api.WithAPIKeyToucher(uiAPIKeyRepo),
			api.WithAuthAPIKeyLimiter(c.apiKeyLimiter),
			api.WithAuthRateLimitMetrics(c.rateLimitMetrics),
			api.WithAuthPerIPLimiter(c.perIPLimiter,
				c.Config.API.RateLimit.PerIP.RPS,
				c.Config.API.RateLimit.PerIP.Burst),
			api.WithAuthDryRunMetrics(c.dryRunMetrics),
			api.WithAuthChainMetrics(c.chainMetrics),
		}
		// Share the SAME session backend instance with the UI subtree so
		// a cookie minted at login authenticates the browser on /ui pages
		// (the redirect-to-login + CSRF gate live in AuthMiddleware).
		if sessionLogin != nil {
			uiAuthOpts = append(uiAuthOpts, api.WithSessionBackend(sessionLogin.backend))
		}
		uiHandler = api.AuthMiddleware(api.BuildAuthConfig(c.Config, uiAuthOpts...))(uiHandler)

		mux.Handle("/ui", uiHandler)
		mux.Handle("/ui/", uiHandler)
		c.Logger.Info().Msg("ui routes mounted at /ui and /ui/")
	} else {
		c.Logger.Info().Msg("node profile: serve_ui=false; UI routes not mounted on this node")
	}

	// OAuth routes (github-login phase 3) — mounted DIRECTLY on the
	// main mux, NOT behind AuthMiddleware: login must work before the
	// browser has any credential. The handler is its own router for
	// /auth/<provider>/{start,callback}. The per-IP backstop is
	// applied here (closed 2026-06-06) using the SAME limiter instance
	// as the main router so a flood can't double its budget by
	// splitting requests across /auth/* and /api/*.
	if sessionLogin != nil {
		authHandler := sessionLogin.loginHandler
		authHandler = api.PerIPLimit(
			c.perIPLimiter,
			c.rateLimitMetrics,
			c.Config.API.RateLimit.PerIP.RPS,
			c.Config.API.RateLimit.PerIP.Burst,
		)(authHandler)
		mux.Handle("/auth/", authHandler)
		c.Logger.Info().
			Strs("providers", sessionLogin.providerNames).
			Str("external_base_url", c.Config.Auth.ExternalBaseURL).
			Bool("per_ip_limit", c.perIPLimiter != nil).
			Msg("browser login mounted at /auth/ with per-IP limiter")
	}

	// Create HTTP server with timeouts. WriteTimeout must exceed the
	// chat-provider per-call timeout — the proxy at
	// /api/v1/chat/completions holds the connection open for the entire
	// LLM turn, so a WriteTimeout below that cuts responses off before
	// the provider finishes.
	writeTimeout := effectiveServerWriteTimeout(c.Config.Server.WriteTimeout, c.Config.Chat.Timeout)
	if configured := resolveChatTimeout(c.Config.Server.WriteTimeout); configured > 0 && configured < writeTimeout {
		c.Logger.Warn().
			Str("configured_write_timeout", c.Config.Server.WriteTimeout).
			Str("chat_timeout", c.Config.Chat.Timeout).
			Dur("effective_write_timeout", writeTimeout).
			Msg("server.write_timeout is below chat proxy budget — raising effective HTTP write timeout")
	}
	// Trusted-proxy real-client-IP resolution. This wraps the ENTIRE mux
	// (API at /, UI at /ui, browser login at /auth/) as the OUTERMOST
	// middleware so the vetted client IP is in the request context before
	// any auth / rate-limit / CSRF / audit code runs. All six historical
	// client-IP derivations now read realip.ClientIPFromContext.
	// see LLD § https://docs.vornik.io
	rootHandler, err := c.wrapRealIP(mux)
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              c.Config.Server.Address,
		Handler:           rootHandler,
		ReadTimeout:       parseDuration(c.Config.Server.ReadTimeout, 30*time.Second),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       60 * time.Second,
	}

	c.HTTPServer = server
	return nil
}

func onboardingSecretsDir(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "secrets")
}

// adminUIDeps carries the locally-computed dependencies that
// adminUIOptions needs from initHTTPServer's scope. Extracted to
// reduce nestif complexity of the if c.providers.Admin block.
type adminUIDeps struct {
	apiServer          *api.Server
	workflowApplier    ui.WorkflowApplierUI
	workflowRollbacker ui.WorkflowRollbackerUI
	archAdapter        *workflowArchitectAdapter
	healingTrialRunner ui.HealingTrialRunnerUI
	healingPromoter    ui.HealingCandidatePromoterUI
}

// adminUIOptions returns the ui.ServerOption slice for all Admin UI
// surfaces — admin-ui-design.md slice 1. Only called when
// c.providers.Admin is true (Enterprise edition). Container fields
// (repos, DB, Registry, …) are read directly; the few locally-computed
// values from initHTTPServer are passed via deps.
func (c *Container) adminUIOptions(deps adminUIDeps) []ui.ServerOption { //nolint:gocognit // admin wiring is inherently wide; each block is a nil-gate for a separate /ui/admin surface
	var opts []ui.ServerOption
	// Admin UI wiring — admin-ui-design.md slice 1. Each option is
	// nil-safe; the UI handlers degrade with empty states when a
	// source isn't supplied. We always wire the audit repo so the
	// landing tile and the audit page work even when other admin
	// sources are missing.
	if c.repos != nil && c.repos.AdminAudit != nil {
		opts = append(opts, ui.WithAdminAuditRepository(c.repos.AdminAudit))
	}
	// /ui/admin/users login approval needs the identity core (postgres).
	// Nil-safe: the page renders a "requires the identity core" notice.
	if c.repos != nil && c.repos.Identity != nil {
		opts = append(opts, ui.WithIdentityRepository(c.repos.Identity))
	}
	if c.repos != nil && c.repos.UISessions != nil {
		opts = append(opts, ui.WithUISessionRepository(c.repos.UISessions))
	}
	if c.repos != nil && c.repos.ChatAudit != nil {
		opts = append(opts, ui.WithAdminChatAuditRepository(c.repos.ChatAudit))
	}
	// B-16: /ui/admin/memory-audit needs both audit repos. Each is
	// nil-safe; the page renders a per-tab "not wired" hint when
	// only one is configured (or neither).
	if c.repos != nil && c.repos.MemoryRetrievalAudit != nil {
		opts = append(opts, ui.WithMemoryRetrievalAuditRepository(c.repos.MemoryRetrievalAudit))
	}
	if c.repos != nil && c.repos.MemoryIngestAudit != nil {
		opts = append(opts, ui.WithMemoryIngestAuditRepository(c.repos.MemoryIngestAudit))
	}
	if c.repos != nil && c.repos.WorkflowProposals != nil {
		opts = append(opts, ui.WithWorkflowProposalsRepository(c.repos.WorkflowProposals))
		// §8.5 diff panel: the proposal detail page renders the
		// current on-disk WORKFLOW.md against the proposed YAML. Reuse
		// the same filesystem source the architect reads from so the
		// "current" side matches what apply would overwrite.
		opts = append(opts, ui.WithWorkflowSourceUI(
			&fsWorkflowSource{configDir: resolveRegistryConfigDir(c.ConfigPath)},
		))
		// Slice 3 predicted-impact: the detail page's impact panel
		// shows the workflow's CURRENT cost / failure-rate baseline
		// from execution telemetry (the baseline the proposal targets,
		// not a forecast). Reuse the same rollup service the architect
		// reads from so the operator sees the same numbers the architect
		// reasoned over. Nil-safe: only wired when a DB is present; the
		// panel falls back to the heuristic summary otherwise.
		// see https://docs.vornik.io §Slice-3
		if c.DB != nil {
			opts = append(opts, ui.WithWorkflowRollupSource(
				&memeticTelemetrySource{svc: workflowtelemetry.NewService(c.DB)},
			))
		}
	}
	// Slice 4: UI Apply button. The memetic.Applier returns
	// *persistence.WorkflowProposal directly, which matches the
	// UI's narrow interface verbatim.
	if deps.workflowApplier != nil {
		opts = append(opts, ui.WithWorkflowApplierUI(deps.workflowApplier))
	}
	// Slice 5: UI Rollback button. Same shape.
	if deps.workflowRollbacker != nil {
		opts = append(opts, ui.WithWorkflowRollbackerUI(deps.workflowRollbacker))
	}
	// Black Box Phase B follow-on: the "Generate candidate" button
	// on the workflow-healing trigger detail page calls the same
	// architect the api surface uses. Reuses the api adapter's
	// underlying *memetic.Architect via .UI(); nil-safe (the trigger
	// detail page renders the "architect not wired" hint when absent).
	if uiArch := deps.archAdapter.UI(); uiArch != nil {
		opts = append(opts, ui.WithBlackBoxArchitect(uiArch))
	}
	// Self-Healing Workflow Genome v1 — the run-trial / promote / reject
	// buttons on /ui/admin/blackbox/candidates drive the SAME concrete
	// runner + promoter the api surface wires (Unit 6). Nil-safe: when the
	// candidate ledger isn't wired both buttons stay hidden.
	if deps.healingTrialRunner != nil {
		opts = append(opts, ui.WithHealingTrialRunner(deps.healingTrialRunner))
	}
	if deps.healingPromoter != nil {
		opts = append(opts, ui.WithHealingCandidatePromoter(deps.healingPromoter))
	}
	opts = append(opts,
		ui.WithAdminReadinessProvider(newAdminReadinessFromAPI(deps.apiServer)),
		ui.WithAdminLeaseAuditSource(newAdminLeaseAudit(c.DB)),
		ui.WithAdminStuckExecutionSource(newAdminStuckExecs(c.DB)),
		ui.WithAdminMCPConfigSource(newAdminMCPConfig(c.Registry)),
		ui.WithRuntimeReadinessSource(newRuntimeReadinessProbe(c.Config)),
	)
	// Cluster + worker observability — reads daemon_leader_locks
	// straight via the repo. The repo's SQLite stub returns nil on
	// List, so SQLite deploys safely render the empty-state hint.
	if c.repos != nil && c.repos.LeaderLocks != nil {
		opts = append(opts, ui.WithLeaderLockSource(c.repos.LeaderLocks))
	}
	// Fleet view (cluster_nodes registry) for /ui/admin/health/cluster —
	// surfaces webhook/relay nodes the lease tables can't show.
	if c.repos != nil && c.repos.ClusterNodes != nil {
		opts = append(opts, ui.WithClusterNodeSource(c.repos.ClusterNodes))
	}
	// Operator-profile read surface for /ui/memory/operators.
	// SQLite stub returns empty so the page renders the "no
	// rows yet" state instead of "not wired".
	if c.repos != nil && c.repos.OperatorProfiles != nil {
		opts = append(opts, ui.WithOperatorProfileSource(c.repos.OperatorProfiles))
	}
	// Audit-trail reader for the operator-profile detail
	// page's "Recent changes" panel. Same admin_audit table
	// the dispatcher tool writes to.
	if c.repos != nil && c.repos.AdminAudit != nil {
		opts = append(opts, ui.WithOperatorProfileAuditSource(c.repos.AdminAudit))
	}
	if inv := newEmailChannelInventory(c.EmailChannels, c.EmailProjects); inv != nil {
		opts = append(opts, ui.WithEmailChannelInventory(inv))
	}
	if inv := newDispatcherToolInventory(c.Dispatcher); inv != nil {
		opts = append(opts, ui.WithDispatcherToolInventory(inv))
	}
	if c.mcpManager != nil {
		opts = append(opts,
			ui.WithAdminMCPInventory(newAdminMCPInventory(c.mcpManager)),
			ui.WithAdminMCPRefresher(newAdminMCPRefresher(c.mcpManager, c.Registry)),
		)
	}
	return opts
}

// wrapRealIP builds the trusted-proxy real-IP config from the daemon
// config (applying the deprecated api.rate_limit.per_ip.trusted_proxies
// fallback) and wraps next with the resolver middleware. A malformed
// trusted-proxy entry is a fail-at-load error — a typo in the trust list
// is a security-relevant misconfiguration the operator must see at startup,
// not discover via a spoofed lockout.
//
// Startup warnings:
//   - deprecated fallback in use → point operators at server.real_ip.
//   - auth enabled but real_ip unconfigured → behind a tunnel every caller
//     collapses to the proxy IP and the lockout becomes a foot-gun.
//
// see LLD § https://docs.vornik.io
func (c *Container) wrapRealIP(next http.Handler) (http.Handler, error) {
	resolved := c.Config.ResolveRealIP()
	cfg, err := realip.NewConfig(resolved.Enabled, resolved.TrustedProxies, resolved.Header)
	if err != nil {
		return nil, fmt.Errorf("server.real_ip.trusted_proxies: %w", err)
	}

	if resolved.DeprecatedFallback {
		c.Logger.Warn().
			Strs("trusted_proxies", resolved.TrustedProxies).
			Msg("server.real_ip.trusted_proxies is empty; falling back to DEPRECATED api.rate_limit.per_ip.trusted_proxies — migrate to server.real_ip")
	}
	if c.Config.API.AuthEnabled && !c.Config.RealIPConfigured() {
		c.Logger.Warn().
			Msg("auth is enabled but server.real_ip is unconfigured — behind a reverse proxy / Cloudflare tunnel every caller collapses to the proxy IP, so the per-IP lockout can be abused to block all clients; set server.real_ip.trusted_proxies to the proxy host")
	}

	if registry := c.observabilityRegistry(); registry != nil && c.realipMetrics == nil {
		c.realipMetrics = realip.NewMetrics(registry)
	}
	var onUntrusted func()
	if c.realipMetrics != nil {
		onUntrusted = c.realipMetrics.UntrustedHeaderTotal.Inc
	}

	return realip.Middleware(cfg, onUntrusted)(next), nil
}

// parseDuration parses a duration string with a fallback default.
func parseDuration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

// wrapUIAdminGate assembles the UI subtree: the admin gate
// (admin-ui-design.md §10) sits INSIDE the /ui prefix strip so it
// matches the stripped "/admin/..." paths its config, unit tests,
// and pathInGate prefix ("/admin") all assume.
//
// ORDER IS LOAD-BEARING. Pre-2026-06-05 the gate wrapped OUTSIDE
// uiSubtreeHandler and saw the un-stripped "/ui/admin/..." path —
// pathInGate never matched, the gate silently disengaged, and every
// /ui/admin/* page was reachable by any authenticated caller (the
// API-side /api/v1/admin/* endpoints were unaffected; they use the
// path-independent requireAdminGate). Pinned end-to-end by
// container_http_admin_gate_test.go — change the order only with
// those tests in front of you.
//
// The gate still runs AFTER api.AuthMiddleware (which wraps the
// returned handler at the call site), so the api context stamps the
// extractors read are present.
func wrapUIAdminGate(logger zerolog.Logger, adminCfg config.AdminConfig, inner http.Handler) http.Handler {
	gated := admin.Middleware(
		adminCfg,
		func(r *http.Request) string {
			return api.APIKeyFromContext(r.Context())
		},
		// Auth-enabled checker — disengages the admin gate when
		// API auth is off (single-operator dev / homelab mode).
		// Closes the 2026-05-24 bug where /ui/memory/operators
		// returned 403 "admin scope required" for every caller
		// because no admin key could ever be matched without
		// auth on. See https://docs.vornik.io §10.
		func(r *http.Request) bool {
			return api.IsAuthEnabledFromContext(r.Context())
		},
		// Session-admin checker (github-login phase 3) — a browser
		// session whose principal resolved to role=admin passes the
		// gate alongside the api-key admin allowlist.
		func(r *http.Request) bool {
			return api.SessionRoleFromContext(r.Context()) == "admin"
		},
		"/admin",
	)(inner)
	return uiSubtreeHandler(logger, gated)
}

func uiSubtreeHandler(logger zerolog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		path := strings.TrimPrefix(r.URL.Path, "/ui")
		if path == "" {
			path = "/"
		}
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}

		cloned := r.Clone(r.Context())
		cloned.URL.Path = path
		logger.Debug().
			Str("method", r.Method).
			Str("request_path", r.URL.Path).
			Str("rewritten_path", cloned.URL.Path).
			Str("query", cloned.URL.RawQuery).
			Msg("ui request received")
		next.ServeHTTP(w, cloned)
		logger.Debug().
			Str("method", r.Method).
			Str("request_path", r.URL.Path).
			Dur("duration", time.Since(start)).
			Msg("ui request completed")
	})
}

// resolveChatTimeout parses a chat.timeout duration string and returns it
// as a time.Duration. Empty string or parse errors return 0 so the caller
// (BotConfig.DispatchTimeout → effectiveDispatchTimeout) falls back to the
// compiled-in default. Keeping the parse here avoids adding a time-parsing
// helper to every BotConfig consumer.
func resolveChatTimeout(raw string) time.Duration {
	if raw == "" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

func effectiveServerWriteTimeout(writeRaw, chatRaw string) time.Duration {
	chatTimeout := resolveChatTimeout(chatRaw)
	if chatTimeout <= 0 {
		chatTimeout = chat.DefaultTimeout
	}
	minWrite := chatTimeout + 60*time.Second

	writeTimeout := resolveChatTimeout(writeRaw)
	if writeTimeout <= 0 || writeTimeout < minWrite {
		return minWrite
	}
	return writeTimeout
}

// resolveDispatchTimeout picks the total interactive-turn budget for the
// dispatcher, which is distinct from the per-LLM-call timeout. An
// explicit chat.dispatch_timeout wins; otherwise the budget is derived
// from chat.timeout (× 3, floored at 15 minutes) so a turn that makes
// several tool calls + LLM round-trips can finish. Returns 0 only if
// neither value parses, letting the bot fall back to its own hardcoded
// default.
func resolveDispatchTimeout(dispatchRaw, chatRaw string) time.Duration {
	if d := resolveChatTimeout(dispatchRaw); d > 0 {
		return d
	}
	base := resolveChatTimeout(chatRaw)
	if base <= 0 {
		return 0
	}
	derived := base * 3
	if floor := 15 * time.Minute; derived < floor {
		derived = floor
	}
	return derived
}

// buildBBTraceService builds the api.BlackBoxTraceService adapter via the Group D
// BBTraceServiceFactory (set by enterprise.Providers()). Returns nil when the factory
// is not set (Community edition), when the DB is unavailable, or when the driver is
// not Postgres — the API handler then returns 503 BLACKBOX_DISABLED.
func (c *Container) buildBBTraceService() api.BlackBoxTraceService {
	if c == nil || c.providers.BBTraceServiceFactory == nil {
		return nil
	}
	if c.DB == nil || (c.Config != nil && c.Config.Database.Driver != "postgres") {
		return nil
	}
	// Pass a true-nil prometheus.Registerer (not a typed-nil *Registry) when
	// observability isn't wired yet — observabilityRegistry() returns a concrete
	// *prometheus.Registry that is nil during early init, and a typed-nil
	// interface crash-loops the blackbox metrics constructor (2026-06-27). Same
	// guard pattern as container_chat.go; sharedMetrics also defends against it.
	var reg prometheus.Registerer
	if r := c.observabilityRegistry(); r != nil {
		reg = r
	}
	return c.providers.BBTraceServiceFactory(c.DB, reg)
}

// buildBBEngineComponents builds the BBEngineComponents (replay engine adapter +
// upgraded HealingApplier) via the Group D BBReplayEngineFactory. Returns empty
// BBEngineComponents when the factory is nil, the DB is unavailable, or the driver
// is not Postgres.
func (c *Container) buildBBEngineComponents(taskCreator *taskcreate.Creator) BBEngineComponents {
	if c == nil || c.providers.BBReplayEngineFactory == nil {
		return BBEngineComponents{}
	}
	if c.DB == nil || taskCreator == nil {
		return BBEngineComponents{}
	}
	if c.Config != nil && c.Config.Database.Driver != "postgres" {
		return BBEngineComponents{}
	}
	var taskRepo persistence.TaskRepository
	if c.repos != nil {
		taskRepo = c.repos.Tasks
	}
	// True-nil Registerer, not a typed-nil *Registry (see buildBBTraceService).
	var reg prometheus.Registerer
	if r := c.observabilityRegistry(); r != nil {
		reg = r
	}
	return c.providers.BBReplayEngineFactory(c.DB, taskCreator, taskRepo, reg)
}

// buildBBReplayEngine builds only the api.BlackBoxReplayEngine adapter.
// Used by the API option block where the HealingApplier upgrade is handled separately.
func (c *Container) buildBBReplayEngine(taskCreator *taskcreate.Creator) api.BlackBoxReplayEngine {
	return c.buildBBEngineComponents(taskCreator).ReplayEngine
}

// resolvePricingPath picks a pricing.yaml location using the same search
// order as the registry config dir. Returns the first candidate that
// exists as a regular file, or the repo-relative default so a missing
// file still surfaces a sensible path in logs.
func resolvePricingPath(configPath string) string {
	candidates := make([]string, 0, 4)
	if env := os.Getenv("VORNIK_PRICING_PATH"); env != "" {
		candidates = append(candidates, env)
	}
	if dir := resolveRegistryConfigDir(configPath); dir != "" {
		candidates = append(candidates, filepath.Join(dir, "pricing.yaml"))
	}
	if configPath != "" {
		baseDir := filepath.Dir(configPath)
		candidates = append(candidates,
			filepath.Join(baseDir, "pricing.yaml"),
			filepath.Join(baseDir, "configs", "pricing.yaml"),
		)
	}
	candidates = append(candidates, "configs/pricing.yaml")

	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return "configs/pricing.yaml"
}
