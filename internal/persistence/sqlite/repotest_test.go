package sqlite_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// newTestDB spins up a fresh in-memory SQLite database, applies the
// consolidated schema, and returns the *sqlite.DB. Each test case
// gets its own isolated database so cross-test row leakage is
// impossible — at the cost of paying the schema-apply once per
// suite, which is microseconds on :memory:.
func newTestDB(t *testing.T) *sqlite.DB {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Connect(ctx, sqlite.DefaultConfig())
	if err != nil {
		t.Fatalf("sqlite.Connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("sqlite.Migrate: %v", err)
	}
	return db
}

// TestTaskWatcherRepository_Contract runs the backend-agnostic suite
// against the SQLite implementation. A failure here means SQLite has
// diverged from the protocol contract — fix the SQLite side, not the
// suite.
func TestTaskWatcherRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunTaskWatcherSuite(t, sqlite.NewTaskWatcherRepository(db.DB))
}

// TestSkillRepository_Contract — knowledge-skill store. Both backends
// must agree on scope isolation, the version-bump-on-edit upsert, and
// the maturity/role list filters.
func TestSkillRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunSkillSuite(t, sqlite.NewSkillRepository(db.DB))
}

// TestProposalRepository_Contract — the control-plane proposal ledger
// contract (LLD 2026-07-07-control-plane-design, Phase 1): create/get/list,
// the 64 KiB field cap, guarded DRAFT→APPROVED/REJECTED transitions, and the
// no-self-approval constraint.
func TestProposalRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunProposalSuite(t, sqlite.NewProposalRepository(db.DB))
	repotest.RunProposalObservationSuite(t, sqlite.NewProposalRepository(db.DB))
}

// TestCostTuningCanaryRepository_Contract — the cost/quality canary guard's row
// store (design 2026-07-24 §4.3). Both backends must agree on the open/finalize
// lifecycle, the cooldown query keyed (swarm,role,knob), and the baseline JSON
// round-trip.
func TestCostTuningCanaryRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunCostTuningCanarySuite(t, sqlite.NewCostTuningCanaryRepository(db.DB))
}

// TestCostAutoApplyTrust_Contract — the cost-auto-apply trust primitives that
// span the canary + proposal repos (auto-apply design D1/D8): LastApplyActorForKnob
// (join) and StagePreApplySnapshot.
func TestCostAutoApplyTrust_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunCostAutoApplyTrustSuite(t, sqlite.NewCostTuningCanaryRepository(db.DB), sqlite.NewProposalRepository(db.DB))
}

// TestCostTuningCanaries_PartialIndex_SQLite asserts the WHERE status='open'
// partial index exists on the SQLite side too (design I5 parity — SQLite
// supports partial indexes).
func TestCostTuningCanaries_PartialIndex_SQLite(t *testing.T) {
	db := newTestDB(t)
	var sqlText string
	err := db.DB.QueryRowContext(context.Background(), `
		SELECT sql FROM sqlite_master
		WHERE type='index' AND name='idx_cost_tuning_canaries_open'`).Scan(&sqlText)
	if err != nil {
		t.Fatalf("partial index lookup: %v", err)
	}
	if !strings.Contains(sqlText, "status") || !strings.Contains(sqlText, "'open'") {
		t.Fatalf("idx_cost_tuning_canaries_open is not the expected partial index: %q", sqlText)
	}
}

// TestExecutionInjectedSkillRepository_Contract — the execution→skill
// association backing the maturity engine's "worked" credit.
func TestExecutionInjectedSkillRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunExecutionInjectedSkillSuite(t, sqlite.NewExecutionInjectedSkillRepository(db.DB))
}

// TestToolAuditRepository_Contract — same shape, on tool_audit_log.
func TestToolAuditRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunToolAuditSuite(t, sqlite.NewToolAuditRepository(db.DB))
}

func TestRecoveryEventRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunRecoveryEventSuite(t, sqlite.NewRecoveryEventRepository(db.DB))
}

// TestArtifactRepository_Contract — same shape, on artifacts.
func TestArtifactRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunArtifactSuite(t, sqlite.NewArtifactRepository(db.DB))
}

// TestTaskRepository_Contract — same shape, on the tasks table
// including the lease lifecycle. Lease semantics under SQLite
// serialize via BEGIN IMMEDIATE rather than running in parallel
// (no SKIP LOCKED in SQLite), but the correctness contract holds:
// concurrent callers each get a distinct task or ErrNoTasksAvailable.
func TestTaskRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunTaskRepositorySuite(t, sqlite.NewTaskRepository(db.DB))
}

// TestAPIKeyRepository_Contract — security-critical lookup contract.
// Both backends must agree on revoked / expired filtering.
func TestAPIKeyRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunAPIKeyRepositorySuite(t, sqlite.NewAPIKeyRepository(db.DB))
}

// TestTaskLLMUsageRepository_Contract — financial cost accounting.
// Both backends must agree on aggregator totals.
func TestTaskLLMUsageRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunTaskLLMUsageSuite(t, sqlite.NewTaskLLMUsageRepository(db.DB))
}

// TestAutonomyEvaluationRepository_Contract — per-tick autonomy
// audit; both backends must agree on group-by-outcome totals.
func TestAutonomyEvaluationRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunAutonomyEvaluationSuite(t, sqlite.NewAutonomyEvaluationRepository(db.DB))
}

// TestInstinctRepository_Contract — continuous-learning instinct layer
// (migrations 85/86). Both backends must agree on the upsert dedup,
// evidence idempotency, count-derivation, and retire semantics.
func TestInstinctRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunInstinctSuite(t, sqlite.NewInstinctRepository(db.DB))
}

// TestInstinctLiftRepository_Contract — lift-measurement snapshot store
// (migration 128). Both backends must agree on the upsert/get round-trip
// and the empty-input no-SQL short circuit.
func TestInstinctLiftRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunInstinctLiftSuite(t, sqlite.NewInstinctLiftRepository(db.DB), sqlite.NewInstinctRepository(db.DB),
		sqlite.NewExecutionStepOutcomeRepository(db.DB), sqlite.NewTaskRepository(db.DB), sqlite.NewWorkflowProposalRepository(db.DB))
}

// TestTradingOrderRepository_Contract — broker audit channel +
// load-bearing identity-mismatch safeguard against the NVDA
// corruption class.
func TestTradingOrderRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunTradingOrderSuite(t, sqlite.NewTradingOrderRepository(db.DB))
}

// TestWebhookEventRepository_Contract — webhook audit table.
func TestWebhookEventRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunWebhookEventSuite(t, sqlite.NewWebhookEventRepository(db.DB))
}

// TestTaskScratchpadRepository_Contract — single-row-per-task upsert.
func TestTaskScratchpadRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunTaskScratchpadSuite(t,
		sqlite.NewTaskScratchpadRepository(db.DB),
		sqlite.NewTaskRepository(db.DB))
}

// TestTelegramThreadRepository_Contract — forum-topic mapping +
// uniqueness on (chat_id, thread_id).
func TestTelegramThreadRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunTelegramThreadSuite(t,
		sqlite.NewTelegramThreadRepository(db.DB),
		sqlite.NewTaskRepository(db.DB))
}

// TestIntentVerdictRepository_Contract — two-tier verdict persistence.
func TestIntentVerdictRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunIntentVerdictSuite(t, sqlite.NewIntentVerdictRepository(db.DB))
}

// TestTaskJudgeVerdictRepository_Contract — one verdict per task +
// idempotency.
func TestTaskJudgeVerdictRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunTaskJudgeVerdictSuite(t,
		sqlite.NewTaskJudgeVerdictRepository(db.DB),
		sqlite.NewTaskRepository(db.DB))
}

// TestTaskPostMortemRepository_Contract — last-write-wins upsert.
func TestTaskPostMortemRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunTaskPostMortemSuite(t,
		sqlite.NewTaskPostMortemRepository(db.DB),
		sqlite.NewTaskRepository(db.DB))
}

// TestMemoryRetrievalAuditRepository_Contract — Record-only contract.
// FeedbackStats + UnretrievedChunkIDs stay per-backend (need
// project_memory_chunks seed).
func TestMemoryRetrievalAuditRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunMemoryRetrievalAuditSuite(t, sqlite.NewMemoryRetrievalAuditRepository(db.DB))
}

// Round-2 trading-event contracts.
func TestTradingFillRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunTradingFillSuite(t,
		sqlite.NewTradingFillRepository(db.DB),
		sqlite.NewTradingOrderRepository(db.DB))
}
func TestTradingSafetyEventRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunTradingSafetyEventSuite(t, sqlite.NewTradingSafetyEventRepository(db.DB))
}
func TestTradingSnapshotRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunTradingSnapshotSuite(t, sqlite.NewTradingSnapshotRepository(db.DB))
}

// Round-3 memory + KG contracts.
func TestExecutionStepOutcomeRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunExecutionStepOutcomeSuite(t, sqlite.NewExecutionStepOutcomeRepository(db.DB))
}
func TestKnowledgeEntityRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunKnowledgeEntitySuite(t, sqlite.NewKnowledgeEntityRepository(db.DB))
}
func TestKnowledgeEdgeRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunKnowledgeEdgeSuite(t,
		sqlite.NewKnowledgeEdgeRepository(db.DB),
		sqlite.NewKnowledgeEntityRepository(db.DB))
}
func TestMemoryQuarantineRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunMemoryQuarantineSuite(t,
		sqlite.NewMemoryQuarantineRepository(db.DB),
		sqlite.NewArtifactRepository(db.DB))
}
func TestCorpusEpochRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunCorpusEpochSuite(t, sqlite.NewCorpusEpochRepository(db.DB))
}
func TestIngestQueueRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunIngestQueueSuite(t,
		sqlite.NewIngestQueueRepository(db.DB),
		sqlite.NewArtifactRepository(db.DB))
}

// Round-4: Execution + ExtractedDocument + MemoryIngestAudit
// coverage closes the highest-leverage repotest gaps identified
// in the pre-refactor coverage audit (2026-05-28).
func TestExecutionRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunExecutionRepositorySuite(t,
		sqlite.NewExecutionRepository(db.DB),
		sqlite.NewTaskRepository(db.DB))
}
func TestExtractedDocumentRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunExtractedDocumentSuite(t, sqlite.NewExtractedDocumentRepository(db.DB))
}
func TestMemoryIngestAuditRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunMemoryIngestAuditSuite(t, sqlite.NewMemoryIngestAuditRepository(db.DB))
}

// Coverage-gap sweep (2026-06-18): three repos whose SQLite side is a
// real durable implementation and so prove the same backend-agnostic
// contract the Postgres side does. (The other five gap repos are
// Postgres-only — their SQLite stubs are asserted in
// stub_contract_test.go instead.)
func TestA2APushConfigRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunA2APushConfigSuite(t, sqlite.NewA2APushConfigRepository(db.DB))
}
func TestBudgetReservationRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunBudgetReservationSuite(t, sqlite.NewBudgetReservationRepository(db.DB))
}
func TestProjectWizardSessionRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunProjectWizardSessionSuite(t, sqlite.NewProjectWizardSessionRepository(db.DB))
}
func TestInstallationOnboardingSessionRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunInstallationOnboardingSessionSuite(t, sqlite.NewInstallationOnboardingSessionRepository(db.DB))
}
func TestFixItSessionRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunFixItSessionSuite(t, sqlite.NewFixItSessionRepository(db.DB))
}

// TestExecutionNarrationRepository_Contract — the narrator worker's
// persisted story (Narrated Execution Phase 2.1). Both backends must
// agree on per-execution seq assignment + list ordering.
func TestExecutionNarrationRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunExecutionNarrationSuite(t, sqlite.NewExecutionNarrationRepository(db.DB),
		sqlite.NewExecutionRepository(db.DB), sqlite.NewTaskRepository(db.DB))
}

// TestPendingOutcomeBackstop_Contract — the pending_validation backstop
// (2026-08-18): a row under an already-terminal execution must never stay
// pending_validation, while a live or just-terminal execution's rows are left
// for their own consumer/terminal path to finalize.
func TestPendingOutcomeBackstop_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunPendingOutcomeBackstopSuite(t,
		sqlite.NewExecutionStepOutcomeRepository(db.DB),
		sqlite.NewExecutionRepository(db.DB),
		sqlite.NewTaskRepository(db.DB))
}

func TestStepLatency_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunStepLatencySuite(t,
		sqlite.NewExecutionStepOutcomeRepository(db.DB),
		sqlite.NewExecutionRepository(db.DB),
		sqlite.NewTaskRepository(db.DB))
}

// TestChannelDisclosureRepository_Contract — the EU AI Act Art 50 disclosure
// record (migration 139). Unlike ChannelSessionRepository, the SQLite side is
// a real implementation, not a no-op stub: this table is the Art 99 evidence
// trail and a stub would leave single-node deployments unable to prove they
// disclosed. The shared suite runs against Postgres too.
func TestChannelDisclosureRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunChannelDisclosureSuite(t, sqlite.NewChannelDisclosureRepository(db.DB))
}

// TestForgePRReviewStateRepository_Contract — per-PR re-review state
// (migration 171). Durable on SQLite, not a stub: the ABSORBING claim is what
// stops a push burst enqueueing one review per push, so a no-op here would
// silently restore the cost multiplier on every single-node deployment. The
// shared suite runs against Postgres too.
func TestForgePRReviewStateRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunForgePRReviewStateSuite(t, sqlite.NewForgePRReviewStateRepository(db.DB))
}

// TestOpenCheckpointRepair_Contract — a dangling tasks.open_checkpoint_id must
// not survive the read that reports it missing.
//
// This backend is where it can actually happen: sqlite runs with
// foreign_keys(OFF) (sqlite.go), so deleting a checkpoint message leaves the
// pointer behind, where postgres's ON DELETE SET NULL clears it. The shared
// suite runs against Postgres too, asserting the same OBSERVABLE contract each
// backend reaches by its own route.
func TestOpenCheckpointRepair_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunOpenCheckpointRepairSuite(t,
		sqlite.NewTaskMessageRepository(db.DB),
		sqlite.NewTaskRepository(db.DB),
		func(id string) error {
			_, err := db.Exec(`DELETE FROM task_messages WHERE id = ?`, id)
			return err
		},
	)
}

// TestChatMemoryWriteConfirmationRepository_Contract — the shared-scope memory-write
// confirmation two-step (chat memory-write design §5.3). Real durable implementations on
// SQLite, not stubs: a shared write cannot authorize without a persisted acknowledgement. The
// shared suite runs against Postgres too, so a dialect divergence surfaces as a test failure.
func TestChatMemoryWriteConfirmationRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunChatMemoryWriteConfirmationSuite(t,
		sqlite.NewChatMemoryWriteConfirmationRepository(db.DB),
		sqlite.NewChatMemoryWriteAuditRepository(db.DB))
}

// TestMCPOAuthTokenRepository_Contract — the MCP OAuth token store (MCP server authentication
// design §6). The rotation guard and the daemon-scope key are the properties worth pinning on
// both backends; this is the SQLite half.
func TestMCPOAuthTokenRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunMCPOAuthTokenSuite(t, sqlite.NewMCPOAuthTokenRepository(db.DB))
}

// TestExecutionToolGrantRepository_Contract — per-execution tool grants (registry
// design §10.1-§10.4). The store carries a privilege decision, so both backends must
// agree that a REFUSED grant never becomes current, that a superseding grant appends
// rather than replaces, and that escalations count refused attempts.
func TestExecutionToolGrantRepository_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunExecutionToolGrantSuite(t, sqlite.NewExecutionToolGrantRepository(db.DB))
}

// TestProjectFirstSeen_Contract — the ledger behind project_created telemetry.
// CE defaults to SQLite, and the adoption gap this closes is largest exactly
// where CE deployments live, so this lane is the one that matters most.
func TestProjectFirstSeen_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunProjectFirstSeenSuite(t, sqlite.NewProjectFirstSeenRepository(db.DB))
}

// TestStepPrompt_Contract — the content-addressed step-prompt store and the
// hashes on the outcome row (step-prompt persistence design §4).
func TestStepPrompt_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunStepPromptSuite(t, sqlite.NewStepPromptRepository(db.DB), sqlite.NewExecutionStepOutcomeRepository(db.DB))
}

// seedChunkForSuite is the SQLite half of repotest.SeedChunk: chunks are
// cross-table state with backend-specific required columns, so each contract
// file supplies its own raw insert (backend-contract coverage design §8).
func seedChunkForSuite(db *sqlite.DB) repotest.SeedChunk {
	return func(ctx context.Context, id, projectID, content string, needsExtraction bool, createdAt time.Time) error {
		flag := 0
		if needsExtraction {
			flag = 1
		}
		_, err := db.ExecContext(ctx, `INSERT INTO project_memory_chunks
			(id, project_id, content, content_hash, needs_graph_extraction, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`, id, projectID, content, "h-"+id, flag, createdAt.UTC().Format(time.RFC3339Nano))
		return err
	}
}

// The thirteen dual-backend repositories the coverage gate listed on
// 2026-09-04 with no shared suite (backend-contract coverage design §8). Each
// test below deletes one line from cmd/lint-lld-contracts/repo_backend_allowlist.txt.
func TestAdminAudit_Contract(t *testing.T) {
	repotest.RunAdminAuditSuite(t, sqlite.NewAdminAuditRepository(newTestDB(t).DB))
}

func TestCapabilityUsage_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunCapabilityUsageSuite(t, sqlite.NewCapabilityUsageRepository(db.DB), sqlite.NewTaskRepository(db.DB))
}

func TestChatAudit_Contract(t *testing.T) {
	repotest.RunChatAuditSuite(t, sqlite.NewChatAuditRepository(newTestDB(t).DB))
}

func TestChunkGraphExtraction_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunChunkGraphExtractionSuite(t, sqlite.NewChunkGraphExtractionRepository(db.DB), seedChunkForSuite(db))
}

func TestClusterNode_Contract(t *testing.T) {
	repotest.RunClusterNodeSuite(t, sqlite.NewClusterNodeRepository(newTestDB(t).DB))
}

func TestLeaderLock_Contract(t *testing.T) {
	repotest.RunLeaderLockSuite(t, sqlite.NewLeaderLockRepository(newTestDB(t).DB))
}

func TestEntityMention_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunEntityMentionSuite(t, sqlite.NewEntityMentionRepository(db.DB), sqlite.NewKnowledgeEntityRepository(db.DB), seedChunkForSuite(db))
}

func TestExecutionHint_Contract(t *testing.T) {
	repotest.RunExecutionHintSuite(t, sqlite.NewExecutionHintRepository(newTestDB(t).DB))
}

func TestExecutionQualityScore_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunExecutionQualityScoreSuite(t, sqlite.NewExecutionQualityScoreRepository(db.DB), sqlite.NewExecutionRepository(db.DB), sqlite.NewTaskRepository(db.DB))
}

func TestMemorySearchStage_Contract(t *testing.T) {
	repotest.RunMemorySearchStageSuite(t, sqlite.NewMemorySearchStageRepository(newTestDB(t).DB))
}

func TestOperatorProfile_Contract(t *testing.T) {
	repotest.RunOperatorProfileSuite(t, sqlite.NewOperatorProfileRepository(newTestDB(t).DB))
}

func TestSecretRedactionAudit_Contract(t *testing.T) {
	repotest.RunSecretRedactionAuditSuite(t, sqlite.NewSecretRedactionAuditRepository(newTestDB(t).DB))
}

func TestTaskCredential_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunTaskCredentialSuite(t, sqlite.NewTaskCredentialRepository(db.DB), sqlite.NewTaskRepository(db.DB))
}

// TestLLMExchange_Contract — the recorded model exchanges of an agent step
// (llm-exchange record/replay design §3).
func TestLLMExchange_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunLLMExchangeSuite(t, sqlite.NewLLMExchangeRepository(db.DB), sqlite.NewExecutionRepository(db.DB), sqlite.NewTaskRepository(db.DB))
}
