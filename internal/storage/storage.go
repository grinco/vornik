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
	Tasks          persistence.TaskRepository
	Executions     persistence.ExecutionRepository
	Artifacts      persistence.ArtifactRepository
	Watchers       persistence.TaskWatcherRepository
	ToolAudit      persistence.ToolAuditRepository
	RecoveryEvents persistence.RecoveryEventRepository
	AdminAudit     persistence.AdminAuditRepository
	ChatAudit      persistence.ChatAuditRepository
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
	// ProjectWizardSessions backs Feature #2 — the conversational
	// project-setup wizard. Migration 49 wires the table; the
	// repo is nil-safe at every consumer (handler short-circuits
	// to 503 when the field is missing).
	ProjectWizardSessions persistence.ProjectWizardSessionRepository
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

	repos := &Repositories{
		Tasks:                 sqlite.NewTaskRepository(db.DB),
		Executions:            sqlite.NewExecutionRepository(db.DB),
		Artifacts:             sqlite.NewArtifactRepository(db.DB),
		Watchers:              sqlite.NewTaskWatcherRepository(db.DB),
		ToolAudit:             sqlite.NewToolAuditRepository(db.DB),
		RecoveryEvents:        sqlite.NewRecoveryEventRepository(db.DB),
		AdminAudit:            sqlite.NewAdminAuditRepository(db.DB),
		ChatAudit:             sqlite.NewChatAuditRepository(db.DB),
		APIKeys:               sqlite.NewAPIKeyRepository(db.DB),
		Webhooks:              sqlite.NewWebhookEventRepository(db.DB),
		Messages:              sqlite.NewTaskMessageRepository(db.DB),
		Scratchpads:           sqlite.NewTaskScratchpadRepository(db.DB),
		TelegramThreads:       sqlite.NewTelegramThreadRepository(db.DB),
		AutonomyEvaluations:   sqlite.NewAutonomyEvaluationRepository(db.DB),
		IntentVerdicts:        sqlite.NewIntentVerdictRepository(db.DB),
		JudgeVerdicts:         sqlite.NewTaskJudgeVerdictRepository(db.DB),
		PostMortems:           sqlite.NewTaskPostMortemRepository(db.DB),
		Instincts:             sqlite.NewInstinctRepository(db.DB),
		ProjectWizardSessions: sqlite.NewProjectWizardSessionRepository(db.DB),
		ExecutionHints:        sqlite.NewExecutionHintRepository(db.DB),
		CrossProjectCalls:     sqlite.NewCrossProjectCallRepository(db.DB),
		ProjectSpawns:         sqlite.NewProjectSpawnRepository(db.DB),
		MemoryRetrievalAudit:  sqlite.NewMemoryRetrievalAuditRepository(db.DB),
		MemoryIngestAudit:     sqlite.NewMemoryIngestAuditRepository(db.DB),
		// Round 2 — financial.
		LLMUsage:                sqlite.NewTaskLLMUsageRepository(db.DB),
		BudgetReservations:      sqlite.NewBudgetReservationRepository(db.DB),
		A2APushConfigs:          sqlite.NewA2APushConfigRepository(db.DB),
		TradingOrders:           sqlite.NewTradingOrderRepository(db.DB),
		TradingFills:            sqlite.NewTradingFillRepository(db.DB),
		TradingSafetyEvents:     sqlite.NewTradingSafetyEventRepository(db.DB),
		TradingSnapshots:        sqlite.NewTradingSnapshotRepository(db.DB),
		ExtractedDocuments:      sqlite.NewExtractedDocumentRepository(db.DB),
		Reminders:               sqlite.NewReminderRepository(db.DB),
		HealingTriggers:         sqlite.NewWorkflowHealingTriggerRepository(db.DB),
		HealingOverrides:        sqlite.NewWorkflowHealingOverrideRepository(db.DB),
		HealingCandidates:       sqlite.NewWorkflowHealingCandidateRepository(db.DB),
		HealingTrials:           sqlite.NewWorkflowHealingTrialRepository(db.DB),
		MemoryPolicyEvaluations: sqlite.NewMemoryPolicyEvaluationRepository(db.DB),
		LeaderLocks:             sqlite.NewLeaderLockRepository(db.DB),
		ClusterNodes:            sqlite.NewClusterNodeRepository(db.DB),
		ChannelSessions:         sqlite.NewChannelSessionRepository(db.DB),
		LiveEvents:              sqlite.NewExecutionLiveEventRepository(db.DB),
		OperatorProfiles:        sqlite.NewOperatorProfileRepository(db.DB),
		OperatorIdentityLinks:   sqlite.NewOperatorIdentityLinkRepository(db.DB),
		ProfileUseAudit:         sqlite.NewProfileUseAuditRepository(db.DB),
		TelegramPollerState:     sqlite.NewTelegramPollerStateRepository(db.DB),
		WorkflowProposals:       sqlite.NewWorkflowProposalRepository(db.DB),
		// Round 3 — memory + KG.
		StepOutcomes:         sqlite.NewExecutionStepOutcomeRepository(db.DB),
		KnowledgeEntities:    sqlite.NewKnowledgeEntityRepository(db.DB),
		KnowledgeEdges:       sqlite.NewKnowledgeEdgeRepository(db.DB),
		EntityMentions:       sqlite.NewEntityMentionRepository(db.DB),
		ChunkGraphExtraction: sqlite.NewChunkGraphExtractionRepository(db.DB),
		CorpusEpochs:         sqlite.NewCorpusEpochRepository(db.DB),
		MemoryQuarantine:     sqlite.NewMemoryQuarantineRepository(db.DB),
		IngestQueue:          sqlite.NewIngestQueueRepository(db.DB),
		// Scratchpads already wired above; TaskScratchpadRepository
		// is the only remaining piece (see persistence interfaces).
	}
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
		Tasks:                   postgres.NewTaskRepository(dbtx),
		Executions:              postgres.NewExecutionRepository(dbtx),
		Artifacts:               postgres.NewArtifactRepository(dbtx),
		Watchers:                postgres.NewTaskWatcherRepository(dbtx),
		ToolAudit:               postgres.NewToolAuditRepository(dbtx),
		RecoveryEvents:          postgres.NewRecoveryEventRepository(dbtx),
		AdminAudit:              postgres.NewAdminAuditRepository(dbtx),
		ChatAudit:               postgres.NewChatAuditRepository(dbtx),
		APIKeys:                 postgres.NewAPIKeyRepository(dbtx),
		Identity:                postgres.NewIdentityRepository(dbtx),
		UISessions:              postgres.NewUISessionRepository(dbtx),
		Webhooks:                postgres.NewWebhookEventRepository(dbtx),
		Messages:                postgres.NewTaskMessageRepository(dbtx),
		Scratchpads:             postgres.NewTaskScratchpadRepository(dbtx),
		TelegramThreads:         postgres.NewTelegramThreadRepository(dbtx),
		KnowledgeEntities:       postgres.NewKnowledgeEntityRepository(dbtx),
		KnowledgeEdges:          postgres.NewKnowledgeEdgeRepository(dbtx),
		EntityMentions:          postgres.NewEntityMentionRepository(dbtx),
		ChunkGraphExtraction:    postgres.NewChunkGraphExtractionRepository(dbtx),
		MemoryRetrievalAudit:    postgres.NewMemoryRetrievalAuditRepository(dbtx),
		MemoryIngestAudit:       postgres.NewMemoryIngestAuditRepository(dbtx),
		CorpusEpochs:            postgres.NewCorpusEpochRepository(dbtx),
		MemoryQuarantine:        postgres.NewMemoryQuarantineRepository(dbtx),
		IngestQueue:             postgres.NewIngestQueueRepository(dbtx),
		AutonomyEvaluations:     postgres.NewAutonomyEvaluationRepository(dbtx),
		LLMUsage:                postgres.NewTaskLLMUsageRepository(dbtx),
		BudgetReservations:      postgres.NewBudgetReservationRepository(dbtx),
		A2APushConfigs:          postgres.NewA2APushConfigRepository(dbtx),
		StepOutcomes:            postgres.NewExecutionStepOutcomeRepository(dbtx),
		JudgeVerdicts:           postgres.NewTaskJudgeVerdictRepository(dbtx),
		PostMortems:             postgres.NewTaskPostMortemRepository(dbtx),
		Instincts:               postgres.NewInstinctRepository(dbtx),
		ProjectWizardSessions:   postgres.NewProjectWizardSessionRepository(dbtx),
		ExecutionHints:          postgres.NewExecutionHintRepository(dbtx),
		CrossProjectCalls:       postgres.NewCrossProjectCallRepository(dbtx),
		ProjectSpawns:           postgres.NewProjectSpawnRepository(dbtx),
		IntentVerdicts:          postgres.NewIntentVerdictRepository(dbtx),
		TradingOrders:           postgres.NewTradingOrderRepository(dbtx),
		TradingSafetyEvents:     postgres.NewTradingSafetyEventRepository(dbtx),
		TradingFills:            postgres.NewTradingFillRepository(dbtx),
		TradingSnapshots:        postgres.NewTradingSnapshotRepository(dbtx),
		ExtractedDocuments:      postgres.NewExtractedDocumentRepository(dbtx),
		Reminders:               postgres.NewReminderRepository(dbtx),
		HealingTriggers:         postgres.NewWorkflowHealingTriggerRepository(dbtx),
		HealingOverrides:        postgres.NewWorkflowHealingOverrideRepository(dbtx),
		HealingCandidates:       postgres.NewWorkflowHealingCandidateRepository(dbtx),
		HealingTrials:           postgres.NewWorkflowHealingTrialRepository(dbtx),
		MemoryPolicyEvaluations: postgres.NewMemoryPolicyEvaluationRepository(dbtx),
		LeaderLocks:             postgres.NewLeaderLockRepository(dbtx),
		ClusterNodes:            postgres.NewClusterNodeRepository(dbtx),
		ChannelSessions:         postgres.NewChannelSessionRepository(dbtx),
		LiveEvents:              postgres.NewExecutionLiveEventRepository(dbtx),
		OperatorProfiles:        postgres.NewOperatorProfileRepository(dbtx),
		OperatorIdentityLinks:   postgres.NewOperatorIdentityLinkRepository(dbtx),
		ProfileUseAudit:         postgres.NewProfileUseAuditRepository(dbtx),
		TelegramPollerState:     postgres.NewTelegramPollerStateRepository(dbtx),
		WorkflowProposals:       postgres.NewWorkflowProposalRepository(dbtx),
	}
}
