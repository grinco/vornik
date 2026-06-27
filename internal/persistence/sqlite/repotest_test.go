package sqlite_test

import (
	"context"
	"testing"

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
