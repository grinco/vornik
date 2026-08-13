// Package storage owns the database backend dispatch + repository
// factory for the daemon. Today the only registered driver is
// "postgres"; the "sqlite" branch lands in phase 2 of the storage
// abstraction work (https://docs.vornik.io).
//
// Two entry points:
//
//   - Open(ctx, cfg) returns a Backend that owns the connection
//     lifecycle (Close, Migrate, IsReady). The Backend keeps a
//     *postgres.DB pointer for legacy callers that still issue raw
//     SQL directly (state-collectors in container_observability.go).
//
//   - Build(dbtx) constructs a Repositories struct populated with
//     all backend-agnostic repository interfaces the daemon depends
//     on, sharing one DBTX. Container code rebuilds Repositories
//     after metrics come online so every repo picks up the
//     instrumented DBTX.
package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// Repositories is the full set of backend-agnostic repository
// interfaces the daemon depends on, constructed from a single
// persistence.DBTX. Each field carries the persistence-package
// interface type so callers stay decoupled from the underlying
// driver.
type Repositories struct {
	Tasks      persistence.TaskRepository
	Executions persistence.ExecutionRepository
	Artifacts  persistence.ArtifactRepository
	Watchers   persistence.TaskWatcherRepository
	ToolAudit  persistence.ToolAuditRepository
	// ExecutionToolGrants stores per-execution tool grants — the lead narrowing which
	// MCP tools a step is advertised (registry design §10.1-§10.4). Append-only.
	ExecutionToolGrants persistence.ExecutionToolGrantRepository
	RecoveryEvents      persistence.RecoveryEventRepository
	Skills              persistence.SkillRepository
	ExecInjectedSkills  persistence.ExecutionInjectedSkillRepository
	Proposals           persistence.ProposalRepository
	// CostTuningCanaries backs the cost/quality canary + regression
	// auto-rollback guard (LLD 2026-07-24-cost-quality-canary-rollback §D).
	CostTuningCanaries persistence.CostTuningCanaryRepository
	AdminAudit         persistence.AdminAuditRepository
	SecretRedaction    persistence.SecretRedactionAuditRepository
	TaskCredentials    persistence.TaskCredentialRepository
	ChatAudit          persistence.ChatAuditRepository
	// ChannelDisclosure is the EU AI Act Art 50 disclosure record —
	// per-session state AND the Art 99 evidence trail.
	ChannelDisclosure persistence.ChannelDisclosureRepository
	// ChatMemoryWriteConfirmations backs the shared-scope memory-write
	// confirmation two-step (chat memory-write design §5.3, migration
	// 146). ChatMemoryWriteAudit is its append-only evidence companion,
	// written on grant before the pending row is deleted.
	ChatMemoryWriteConfirmations persistence.ChatMemoryWriteConfirmationRepository
	ChatMemoryWriteAudit         persistence.ChatMemoryWriteAuditRepository
	// MCPOAuthTokens holds per-project OAuth grants for MCP servers
	// (MCP server authentication design §6, migration 147). Keyed by
	// (project_id, server_name) with project_id = "" meaning the
	// daemon scope, so one operator consent serves every task in a
	// project — including autonomous runs, which have no user to
	// borrow a token from.
	MCPOAuthTokens persistence.MCPOAuthTokenRepository
	APIKeys        persistence.APIKeyRepository
	// Identity is the identity-core repository (users, groups,
	// channel bindings) backing internal/authz. Phase 2 of
	// oidc-identity-permissions-design.md.
	//
	// NIL ON THE SQLITE BRANCH — the identity tables ship only in
	// the Postgres migrations. Phase-3 wiring (authz.Service /
	// SessionBackend) must gate on the Postgres backend or
	// nil-check before constructing consumers.
	Identity persistence.IdentityRepository
	// UISessions is the browser login session repository (migration 91).
	//
	// NIL ON THE SQLITE BRANCH — the ui_sessions table ships only in
	// the Postgres migrations alongside the rest of the identity core.
	// Consumers must nil-check or gate on the Postgres backend.
	UISessions           persistence.UISessionRepository
	Webhooks             persistence.WebhookEventRepository
	Messages             persistence.TaskMessageRepository
	Scratchpads          persistence.TaskScratchpadRepository
	TelegramThreads      persistence.TelegramThreadRepository
	KnowledgeEntities    persistence.KnowledgeEntityRepository
	KnowledgeEdges       persistence.KnowledgeEdgeRepository
	EntityMentions       persistence.EntityMentionRepository
	ChunkGraphExtraction persistence.ChunkGraphExtractionRepository
	MemoryRetrievalAudit persistence.MemoryRetrievalAuditRepository
	MemoryIngestAudit    persistence.MemoryIngestAuditRepository
	MemorySearchStage    persistence.MemorySearchStageRepository
	CorpusEpochs         persistence.CorpusEpochRepository
	MemoryQuarantine     persistence.MemoryQuarantineRepository
	IngestQueue          persistence.IngestQueueRepository
	AutonomyEvaluations  persistence.AutonomyEvaluationRepository
	LLMUsage             persistence.TaskLLMUsageRepository
	BudgetReservations   persistence.BudgetReservationRepository
	A2APushConfigs       persistence.A2APushConfigRepository
	StepOutcomes         persistence.ExecutionStepOutcomeRepository
	JudgeVerdicts        persistence.TaskJudgeVerdictRepository
	PostMortems          persistence.TaskPostMortemRepository
	// Instincts backs the continuous-learning instinct layer
	// (migrations 85/86). Wired on both backends; the extraction
	// worker (internal/instinct) is gated behind instinct.enabled
	// (default off), so the repo sits idle until an operator opts in.
	Instincts persistence.InstinctRepository
	// InstinctLift backs the true-lift measurement layer (migration
	// 128): the latest snapshot store plus (in later tasks) the
	// per-domain treatment/complement outcome queries. Wired on both
	// backends.
	InstinctLift persistence.InstinctLiftRepository
	// ProjectWizardSessions backs Feature #2 — the conversational
	// project-setup wizard. Migration 49 wires the table; the
	// repo is nil-safe at every consumer (handler short-circuits
	// to 503 when the field is missing).
	ProjectWizardSessions persistence.ProjectWizardSessionRepository
	// InstallationOnboardingSessions backs the installation-scoped
	// first-run setup guide. Migration 111 wires the table; the repo
	// is nil-safe at every consumer.
	InstallationOnboardingSessions persistence.InstallationOnboardingSessionRepository
	// FixItSessions backs the Fix-It Doctor repair-chat sessions
	// (task 3.2). Migration 122 wires the table; the repo is nil-safe
	// at every consumer (the converse endpoint short-circuits to 503
	// when unwired).
	FixItSessions persistence.FixItSessionRepository
	// ExecutionHints backs Feature #3 Phase C — operator-injected
	// hints for live executions. Migration 50 wires the table;
	// nil-safe consumers.
	ExecutionHints persistence.ExecutionHintRepository
	// CrossProjectCalls backs the inter-project orchestration
	// Phase A. Migration 52 wires the table. Postgres-only in
	// v1 (the SQLite branch leaves the field nil); the executor
	// handler nil-checks and surfaces 503-style step failure
	// when unwired — same fail-soft contract every other
	// optional surface uses.
	CrossProjectCalls persistence.CrossProjectCallRepository
	// ProjectSpawns backs inter-project orchestration Phase B's
	// spawn_project step. Migration 53 wires the table.
	// Postgres-only in v1 (same SQLite-stub pattern as
	// CrossProjectCalls); the executor handler nil-checks.
	ProjectSpawns       persistence.ProjectSpawnRepository
	IntentVerdicts      persistence.IntentVerdictRepository
	TradingOrders       persistence.TradingOrderRepository
	TradingSafetyEvents persistence.TradingSafetyEventRepository
	TradingFills        persistence.TradingFillRepository
	TradingSnapshots    persistence.TradingPositionsSnapshotRepository
	// ExtractedDocuments backs the document-extraction pipeline
	// (Phase 0+). nil on the SQLite branch — the test build doesn't
	// wire this repo today; ingest paths nil-check before using.
	ExtractedDocuments persistence.ExtractedDocumentRepository
	// Reminders backs the scheduled-reminders heartbeat
	// (2026.7.0, migration 55). nil on the SQLite branch in v1
	// — operators can still receive reminders only on the
	// Postgres-backed deployment. Heartbeat nil-checks; CLI/UI
	// handlers return 503 when unwired.
	Reminders persistence.ReminderRepository
	// HealingTriggers backs the workflow-healing trigger ledger
	// (Autonomy Black Box Phase B, migration 69). nil on the
	// SQLite branch — the detector is Postgres-only because the
	// trigger insert relies on the partial unique index to dedup
	// open rows. SQLite deployments leave the API + UI surfaces
	// at 503.
	HealingTriggers persistence.WorkflowHealingTriggerRepository
	// HealingOverrides backs the Phase B per-(project, workflow,
	// trigger_class) operator-override surface (migration 81).
	// Same Postgres-only discipline as HealingTriggers; the
	// SQLite branch returns a stub that signals unsupported.
	HealingOverrides persistence.HealingTriggerOverrideRepository
	// HealingCandidates backs the Self-Healing Workflow Genome v1
	// candidate ledger (migration 87). A candidate is a trial-tracking
	// record linking a regression trigger to a memetic WorkflowProposal.
	// Same Postgres-only discipline as HealingTriggers; the SQLite
	// branch returns a stub that signals unsupported.
	HealingCandidates persistence.WorkflowHealingCandidateRepository
	// HealingTrials backs the Self-Healing Workflow Genome v1 trial
	// ledger (migration 88) — one row per trial run of a candidate.
	// Postgres-only; SQLite returns a stub.
	HealingTrials persistence.WorkflowHealingTrialRepository
	// MemoryPolicyEvaluations backs the Policy-Aware Memory
	// Firewall's audit trail (migration 80). Postgres-only;
	// SQLite leaves nil and the firewall surfaces 503.
	MemoryPolicyEvaluations persistence.MemoryPolicyEvaluationRepository
	// LeaderLocks backs the singleton-worker primitive
	// (2026.8.0 horizontal-scaling prep, migration 57). Each
	// worker that must NOT run concurrently across replicas
	// constructs a leaderelection.Elector pointing at this
	// repo. SQLite gets a stub that always grants the lock —
	// single-process deployments don't need contention
	// semantics.
	LeaderLocks persistence.DaemonLeaderLockRepository
	// ChannelSessions persists per-channel conversation state
	// (webchat / email / slack / github / future-telegram) across
	// daemon restarts and across replicas. Migration 58 added the
	// table; channel implementations read-through on Load and
	// write-through on Append, keeping their in-memory map as a
	// hot-path cache. SQLite gets a stub (Load → ErrNotFound,
	// Save/Delete → no-op) so single-process deployments behave
	// identically.
	ChannelSessions persistence.ChannelSessionRepository
	// LiveEvents persists the per-execution live-event stream so
	// a non-emitting replica can serve /executions/{id}/live and
	// late subscribers can replay (migration 59 + cross-replica
	// fanout). SQLite gets a stub; single-process deployments
	// rely on the in-memory livepubsub publisher exclusively.
	LiveEvents persistence.ExecutionLiveEventRepository
	// OperatorProfiles persists per-operator preferences +
	// free-form notes the dispatcher injects into the system
	// prompt on every turn (migration 60). Roadmapped read-
	// path-first slice: schema + repo + dispatcher read; the
	// agent-driven update_operator_profile tool ships in a
	// follow-up. SQLite gets a stub.
	OperatorProfiles persistence.OperatorProfileRepository
	// OperatorIdentityLinks persists cross-channel speaker-id
	// → canonical-operator-id mappings (migration 60). Powers
	// the `/link` slash command + `vornikctl operator link` so
	// the same human chatting from Telegram + webchat sees one
	// profile in both. SQLite gets a stub; single-process
	// deployments rarely span channels in practice.
	OperatorIdentityLinks persistence.OperatorIdentityLinkRepository
	// ProfileUseAudit persists per-turn audit rows recording
	// which operator-profile keys + notes the dispatcher
	// injected into the system prompt for one chat turn.
	// Powers `vornikctl operator audit`. Migration 64 (Phase B).
	// SQLite gets a stub.
	ProfileUseAudit persistence.ProfileUseAuditRepository
	// TelegramPollerState persists the long-poll offset
	// watermark (migration 61) so leader-failover doesn't
	// replay queued updates. SQLite gets a stub (single-
	// process deployments accept the brief restart-window
	// replay).
	TelegramPollerState persistence.TelegramPollerStateRepository
	// WorkflowProposals backs the memetic-workflows architect
	// (Slice 2; migration 65). Postgres-only in v1 — the SQLite
	// branch leaves it nil and the admin propose endpoint
	// fail-softs to 503 when unwired, same pattern as
	// CrossProjectCalls.
	WorkflowProposals persistence.WorkflowProposalRepository
	// ClusterNodes backs the fleet heartbeat registry (migration 95,
	// Slice C1). Every DB-having node (ui/worker/all) upserts its
	// own row; the /api/v1/cluster endpoint reads the table.
	// Both Postgres and SQLite get a real implementation.
	ClusterNodes persistence.ClusterNodeRepository
	// ExecutionNarration backs the narrator worker's persisted story
	// (Narrated Execution Phase 2.1, migration 121). Both Postgres
	// and SQLite get a real implementation — narration is the story
	// view's source of truth on every backend, unlike the live-only
	// cross-replica accelerant (LiveEvents).
	ExecutionNarration persistence.ExecutionNarrationRepository
}

// Backend owns the underlying database connection lifecycle.
//
// Driver carries the active backend ("postgres" or "sqlite"). DB is
// the raw *sql.DB for either backend — callers that issue cross-
// backend SQL (state collectors that GROUP BY tasks/executions)
// hold it directly. PG is the live *postgres.DB only when
// Driver=="postgres", nil otherwise (used by Postgres-specific
// callers: pg_stat_user_tables sampling, migration runner inspection).
//
// Repos is the canonical repository set for the active backend.
// CLI tools and tests consume it directly; the daemon may rebuild
// it after metrics come online so the metrics-wrapped DBTX flows
// through every repo — see Repositories rebuild logic in
// internal/service.
type Backend struct {
	Driver          string
	DB              *sql.DB
	PG              *postgres.DB
	MigrationRunner *persistence.MigrationRunner
	Repos           *Repositories
	Close           func() error
	Migrate         func(ctx context.Context) error
	IsReady         func(ctx context.Context) error
}

// Open dispatches to the configured driver, establishes the
// connection, and runs pending migrations. Migration failures are
// returned to the caller — the daemon's historical behaviour of
// logging-but-not-failing on migration error stays in the container
// wrapper so the storage package can be reused from tests that
// expect strict semantics.
func Open(ctx context.Context, cfg config.DatabaseConfig) (*Backend, error) {
	driver := cfg.Driver
	if driver == "" {
		driver = "postgres"
	}
	switch driver {
	case "postgres":
		return openPostgres(ctx, cfg)
	case "sqlite":
		return openSQLite(ctx, cfg)
	default:
		return nil, fmt.Errorf("storage: unsupported database driver: %s", driver)
	}
}

// openSQLite opens a SQLite-backed Backend with the phase-2 starter
// repos populated. The four implemented repos (TaskWatchers,
// ToolAudit, Artifacts, Executions) carry SQLite handles; the
// remaining fields stay nil for now — phase-2 follow-on commits
// fill them in. Tests against this Backend must scope themselves to
// the implemented surface.
func openSQLite(ctx context.Context, cfg config.DatabaseConfig) (*Backend, error) {
	sqliteCfg := sqlite.Config{Path: cfg.Path}

	db, err := sqlite.Connect(ctx, sqliteCfg)
	if err != nil {
		return nil, fmt.Errorf("storage: connect sqlite: %w", err)
	}
	if err := db.Migrate(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: migrate sqlite: %w", err)
	}

	repos := buildSQLiteRepositories(db.DB)
	return &Backend{
		Driver:  "sqlite",
		DB:      db.DB,
		PG:      nil, // SQLite — no postgres handle
		Repos:   repos,
		Close:   db.Close,
		Migrate: db.Migrate,
		IsReady: db.IsReady,
	}, nil
}

// buildSQLiteRepositories constructs the SQLite-backed Repositories
// set. Split out of openSQLite (2026-07-10, task 2.1) purely to keep
// the connect/migrate/construct function under the funlen budget —
// no behaviour change.
func buildSQLiteRepositories(db *sql.DB) *Repositories {
	return &Repositories{
		Tasks:                          sqlite.NewTaskRepository(db),
		Executions:                     sqlite.NewExecutionRepository(db),
		Artifacts:                      sqlite.NewArtifactRepository(db),
		Watchers:                       sqlite.NewTaskWatcherRepository(db),
		ToolAudit:                      sqlite.NewToolAuditRepository(db),
		ExecutionToolGrants:            sqlite.NewExecutionToolGrantRepository(db),
		RecoveryEvents:                 sqlite.NewRecoveryEventRepository(db),
		Skills:                         sqlite.NewSkillRepository(db),
		ExecInjectedSkills:             sqlite.NewExecutionInjectedSkillRepository(db),
		Proposals:                      sqlite.NewProposalRepository(db),
		CostTuningCanaries:             sqlite.NewCostTuningCanaryRepository(db),
		AdminAudit:                     sqlite.NewAdminAuditRepository(db),
		SecretRedaction:                sqlite.NewSecretRedactionAuditRepository(db),
		TaskCredentials:                sqlite.NewTaskCredentialRepository(db),
		ChatAudit:                      sqlite.NewChatAuditRepository(db),
		ChannelDisclosure:              sqlite.NewChannelDisclosureRepository(db),
		ChatMemoryWriteConfirmations:   sqlite.NewChatMemoryWriteConfirmationRepository(db),
		MCPOAuthTokens:                 sqlite.NewMCPOAuthTokenRepository(db),
		ChatMemoryWriteAudit:           sqlite.NewChatMemoryWriteAuditRepository(db),
		APIKeys:                        sqlite.NewAPIKeyRepository(db),
		Webhooks:                       sqlite.NewWebhookEventRepository(db),
		Messages:                       sqlite.NewTaskMessageRepository(db),
		Scratchpads:                    sqlite.NewTaskScratchpadRepository(db),
		TelegramThreads:                sqlite.NewTelegramThreadRepository(db),
		AutonomyEvaluations:            sqlite.NewAutonomyEvaluationRepository(db),
		IntentVerdicts:                 sqlite.NewIntentVerdictRepository(db),
		JudgeVerdicts:                  sqlite.NewTaskJudgeVerdictRepository(db),
		PostMortems:                    sqlite.NewTaskPostMortemRepository(db),
		Instincts:                      sqlite.NewInstinctRepository(db),
		InstinctLift:                   sqlite.NewInstinctLiftRepository(db),
		ProjectWizardSessions:          sqlite.NewProjectWizardSessionRepository(db),
		InstallationOnboardingSessions: sqlite.NewInstallationOnboardingSessionRepository(db),
		FixItSessions:                  sqlite.NewFixItSessionRepository(db),
		ExecutionHints:                 sqlite.NewExecutionHintRepository(db),
		CrossProjectCalls:              sqlite.NewCrossProjectCallRepository(db),
		ProjectSpawns:                  sqlite.NewProjectSpawnRepository(db),
		MemoryRetrievalAudit:           sqlite.NewMemoryRetrievalAuditRepository(db),
		MemoryIngestAudit:              sqlite.NewMemoryIngestAuditRepository(db),
		MemorySearchStage:              sqlite.NewMemorySearchStageRepository(db),
		// Round 2 — financial.
		LLMUsage:                sqlite.NewTaskLLMUsageRepository(db),
		BudgetReservations:      sqlite.NewBudgetReservationRepository(db),
		A2APushConfigs:          sqlite.NewA2APushConfigRepository(db),
		TradingOrders:           sqlite.NewTradingOrderRepository(db),
		TradingFills:            sqlite.NewTradingFillRepository(db),
		TradingSafetyEvents:     sqlite.NewTradingSafetyEventRepository(db),
		TradingSnapshots:        sqlite.NewTradingSnapshotRepository(db),
		ExtractedDocuments:      sqlite.NewExtractedDocumentRepository(db),
		Reminders:               sqlite.NewReminderRepository(db),
		HealingTriggers:         sqlite.NewWorkflowHealingTriggerRepository(db),
		HealingOverrides:        sqlite.NewWorkflowHealingOverrideRepository(db),
		HealingCandidates:       sqlite.NewWorkflowHealingCandidateRepository(db),
		HealingTrials:           sqlite.NewWorkflowHealingTrialRepository(db),
		MemoryPolicyEvaluations: sqlite.NewMemoryPolicyEvaluationRepository(db),
		LeaderLocks:             sqlite.NewLeaderLockRepository(db),
		ClusterNodes:            sqlite.NewClusterNodeRepository(db),
		ChannelSessions:         sqlite.NewChannelSessionRepository(db),
		LiveEvents:              sqlite.NewExecutionLiveEventRepository(db),
		OperatorProfiles:        sqlite.NewOperatorProfileRepository(db),
		OperatorIdentityLinks:   sqlite.NewOperatorIdentityLinkRepository(db),
		ProfileUseAudit:         sqlite.NewProfileUseAuditRepository(db),
		TelegramPollerState:     sqlite.NewTelegramPollerStateRepository(db),
		WorkflowProposals:       sqlite.NewWorkflowProposalRepository(db),
		// Round 3 — memory + KG.
		StepOutcomes:         sqlite.NewExecutionStepOutcomeRepository(db),
		KnowledgeEntities:    sqlite.NewKnowledgeEntityRepository(db),
		KnowledgeEdges:       sqlite.NewKnowledgeEdgeRepository(db),
		EntityMentions:       sqlite.NewEntityMentionRepository(db),
		ChunkGraphExtraction: sqlite.NewChunkGraphExtractionRepository(db),
		CorpusEpochs:         sqlite.NewCorpusEpochRepository(db),
		MemoryQuarantine:     sqlite.NewMemoryQuarantineRepository(db),
		IngestQueue:          sqlite.NewIngestQueueRepository(db),
		ExecutionNarration:   sqlite.NewExecutionNarrationRepository(db),
		// Scratchpads already wired above; TaskScratchpadRepository
		// is the only remaining piece (see persistence interfaces).
	}
}

func openPostgres(ctx context.Context, cfg config.DatabaseConfig) (*Backend, error) {
	pgCfg := postgres.Config{
		Host:            cfg.Host,
		Port:            cfg.Port,
		Database:        cfg.Name,
		User:            cfg.User,
		Password:        cfg.Password,
		SSLMode:         cfg.SSLMode,
		MaxOpenConns:    25,
		MaxIdleConns:    5,
		ConnMaxLifetime: 5 * time.Minute,
		ConnectTimeout:  10 * time.Second,
	}

	pgDB, err := postgres.Connect(ctx, pgCfg)
	if err != nil {
		return nil, fmt.Errorf("storage: connect postgres: %w", err)
	}

	return &Backend{
		Driver:          "postgres",
		DB:              pgDB.DB,
		PG:              pgDB,
		MigrationRunner: pgDB.MigrationRunner(),
		Repos:           Build(pgDB.DB),
		Close:           pgDB.Close,
		Migrate:         pgDB.Migrate,
		IsReady:         pgDB.IsReady,
	}, nil
}

// Build constructs the Repositories struct from a DBTX. Today only
// the postgres-package implementations are wired; the SQLite branch
// lands in phase 2 and will pick by examining a sentinel on the DBTX
// (or via a separate sqlite-specific BuildXxx method).
func Build(dbtx persistence.DBTX) *Repositories {
	return &Repositories{
		Tasks:                          postgres.NewTaskRepository(dbtx),
		Executions:                     postgres.NewExecutionRepository(dbtx),
		Artifacts:                      postgres.NewArtifactRepository(dbtx),
		Watchers:                       postgres.NewTaskWatcherRepository(dbtx),
		ToolAudit:                      postgres.NewToolAuditRepository(dbtx),
		ExecutionToolGrants:            postgres.NewExecutionToolGrantRepository(dbtx),
		RecoveryEvents:                 postgres.NewRecoveryEventRepository(dbtx),
		Skills:                         postgres.NewSkillRepository(dbtx),
		ExecInjectedSkills:             postgres.NewExecutionInjectedSkillRepository(dbtx),
		Proposals:                      postgres.NewProposalRepository(dbtx),
		CostTuningCanaries:             postgres.NewCostTuningCanaryRepository(dbtx),
		AdminAudit:                     postgres.NewAdminAuditRepository(dbtx),
		SecretRedaction:                postgres.NewSecretRedactionAuditRepository(dbtx),
		TaskCredentials:                postgres.NewTaskCredentialRepository(dbtx),
		ChatAudit:                      postgres.NewChatAuditRepository(dbtx),
		ChannelDisclosure:              postgres.NewChannelDisclosureRepository(dbtx),
		ChatMemoryWriteConfirmations:   postgres.NewChatMemoryWriteConfirmationRepository(dbtx),
		MCPOAuthTokens:                 postgres.NewMCPOAuthTokenRepository(dbtx),
		ChatMemoryWriteAudit:           postgres.NewChatMemoryWriteAuditRepository(dbtx),
		APIKeys:                        postgres.NewAPIKeyRepository(dbtx),
		Identity:                       postgres.NewIdentityRepository(dbtx),
		UISessions:                     postgres.NewUISessionRepository(dbtx),
		Webhooks:                       postgres.NewWebhookEventRepository(dbtx),
		Messages:                       postgres.NewTaskMessageRepository(dbtx),
		Scratchpads:                    postgres.NewTaskScratchpadRepository(dbtx),
		TelegramThreads:                postgres.NewTelegramThreadRepository(dbtx),
		KnowledgeEntities:              postgres.NewKnowledgeEntityRepository(dbtx),
		KnowledgeEdges:                 postgres.NewKnowledgeEdgeRepository(dbtx),
		EntityMentions:                 postgres.NewEntityMentionRepository(dbtx),
		ChunkGraphExtraction:           postgres.NewChunkGraphExtractionRepository(dbtx),
		MemoryRetrievalAudit:           postgres.NewMemoryRetrievalAuditRepository(dbtx),
		MemoryIngestAudit:              postgres.NewMemoryIngestAuditRepository(dbtx),
		MemorySearchStage:              postgres.NewMemorySearchStageRepository(dbtx),
		CorpusEpochs:                   postgres.NewCorpusEpochRepository(dbtx),
		MemoryQuarantine:               postgres.NewMemoryQuarantineRepository(dbtx),
		IngestQueue:                    postgres.NewIngestQueueRepository(dbtx),
		AutonomyEvaluations:            postgres.NewAutonomyEvaluationRepository(dbtx),
		LLMUsage:                       postgres.NewTaskLLMUsageRepository(dbtx),
		BudgetReservations:             postgres.NewBudgetReservationRepository(dbtx),
		A2APushConfigs:                 postgres.NewA2APushConfigRepository(dbtx),
		StepOutcomes:                   postgres.NewExecutionStepOutcomeRepository(dbtx),
		JudgeVerdicts:                  postgres.NewTaskJudgeVerdictRepository(dbtx),
		PostMortems:                    postgres.NewTaskPostMortemRepository(dbtx),
		Instincts:                      postgres.NewInstinctRepository(dbtx),
		InstinctLift:                   postgres.NewInstinctLiftRepository(dbtx),
		ProjectWizardSessions:          postgres.NewProjectWizardSessionRepository(dbtx),
		InstallationOnboardingSessions: postgres.NewInstallationOnboardingSessionRepository(dbtx),
		FixItSessions:                  postgres.NewFixItSessionRepository(dbtx),
		ExecutionHints:                 postgres.NewExecutionHintRepository(dbtx),
		CrossProjectCalls:              postgres.NewCrossProjectCallRepository(dbtx),
		ProjectSpawns:                  postgres.NewProjectSpawnRepository(dbtx),
		IntentVerdicts:                 postgres.NewIntentVerdictRepository(dbtx),
		TradingOrders:                  postgres.NewTradingOrderRepository(dbtx),
		TradingSafetyEvents:            postgres.NewTradingSafetyEventRepository(dbtx),
		TradingFills:                   postgres.NewTradingFillRepository(dbtx),
		TradingSnapshots:               postgres.NewTradingSnapshotRepository(dbtx),
		ExtractedDocuments:             postgres.NewExtractedDocumentRepository(dbtx),
		Reminders:                      postgres.NewReminderRepository(dbtx),
		HealingTriggers:                postgres.NewWorkflowHealingTriggerRepository(dbtx),
		HealingOverrides:               postgres.NewWorkflowHealingOverrideRepository(dbtx),
		HealingCandidates:              postgres.NewWorkflowHealingCandidateRepository(dbtx),
		HealingTrials:                  postgres.NewWorkflowHealingTrialRepository(dbtx),
		MemoryPolicyEvaluations:        postgres.NewMemoryPolicyEvaluationRepository(dbtx),
		LeaderLocks:                    postgres.NewLeaderLockRepository(dbtx),
		ClusterNodes:                   postgres.NewClusterNodeRepository(dbtx),
		ChannelSessions:                postgres.NewChannelSessionRepository(dbtx),
		LiveEvents:                     postgres.NewExecutionLiveEventRepository(dbtx),
		OperatorProfiles:               postgres.NewOperatorProfileRepository(dbtx),
		OperatorIdentityLinks:          postgres.NewOperatorIdentityLinkRepository(dbtx),
		ProfileUseAudit:                postgres.NewProfileUseAuditRepository(dbtx),
		TelegramPollerState:            postgres.NewTelegramPollerStateRepository(dbtx),
		WorkflowProposals:              postgres.NewWorkflowProposalRepository(dbtx),
		ExecutionNarration:             postgres.NewExecutionNarrationRepository(dbtx),
	}
}
