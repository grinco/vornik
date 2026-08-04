//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence/repotest"
)

// Postgres-side wiring of the backend-agnostic repository contract
// suite from internal/persistence/repotest. Each test connects to
// the integration test database, applies migrations, builds the
// real Postgres repo, and delegates to the same suite the SQLite
// tests run. A failure here that doesn't reproduce on SQLite (or
// vice versa) is a protocol divergence — fix the diverging backend,
// not the suite.
//
// Run with:
//   make test-int
// or:
//   go test -tags=integration ./internal/persistence/postgres/...

// TestMain purges the proj-*/other-* test fixtures the shared
// repotest suites seed via uniqueID("proj") and uniqueID("other").
// Without this hook the contract tests accumulated ~1500 rows per
// run in the shared vornik_test database (operator-noticed
// 2026-05-17: $1100 in fake task_llm_usage cost and growing).
// The DELETE pattern is intentionally LIKE-prefixed to those two
// namespaces — real project IDs (assistant, snake, janka,
// ibkr-trader, ...) don't overlap.
func TestMain(m *testing.M) {
	// Bootstrap the dedicated integration database before any test
	// opens a connection. Without this every test would have to
	// special-case "database does not exist" on first run.
	if err := ensureIntegrationDB(); err != nil {
		fmt.Fprintf(os.Stderr, "integration db bootstrap failed: %v\n", err)
		// Fall through — tests will Skip via their own connect-failure
		// path. Skipping here would mask a misconfigured CI image.
	}
	code := m.Run()
	if err := purgeRepotestLeftovers(); err != nil {
		// Cleanup failure shouldn't mask test failures, but should
		// surface so the operator knows the DB is accumulating
		// fixtures again.
		fmt.Fprintf(os.Stderr, "repotest cleanup failed: %v\n", err)
	}
	os.Exit(code)
}

// purgeRepotestLeftovers runs the FK-safe DELETE plan inside one
// transaction so a mid-flight failure leaves no partial state. The
// ordering matches /tmp/cleanup-test-leftovers.sql (kept in shell
// history; this is the canonical version going forward).
//
// 2026-05-28 follow-up: the original sweep only matched
// uniqueID-prefixed namespaces (`proj-%`, `other-%`), but repotest.go
// also hardcodes the short literals "p", "p1", "p2", "proj-1" in
// ~30 call sites. Those leak into the shared DB whenever a developer
// runs the suite against POSTGRES_DB=vornik_test instead of the
// dedicated vornik_integration_test. The sweep now also matches
// those literals and includes `extracted_documents` +
// `memory_ingest_audit` — both project-scoped, both missed by the
// original list.

// repotestLeftoverPredicate is the SQL WHERE-fragment that selects
// rows owned by integration-test fixtures. Two shapes:
//
//   - LIKE 'proj-%' / 'other-%' — the uniqueID-stamped namespaces
//     produced by repotest.uniqueID().
//   - IN ('p','p1','p2','proj-1','proj-2') — short literals
//     hardcoded by repotest.go call sites that predate uniqueID().
//
// Real project IDs (assistant, snake, janka, ibkr-trader,
// companion-example, troubleshooter, headmatch, n8n-agents, vornik,
// vornik-autocoder) and the `_external` attribution bucket are
// multi-character and never overlap either shape.
//
// Exposed at package scope so TestPurgeRepotestLeftovers_Literals
// can pin the literal list — see repotest_integration_sweep_test.go.
const repotestLeftoverPredicate = "(project_id LIKE 'proj-%' OR project_id LIKE 'other-%' OR project_id IN ('p','p1','p2','proj-1','proj-2'))"

func purgeRepotestLeftovers() error {
	cfg := Config{
		Host:            getEnvOrDefault("POSTGRES_HOST", "localhost"),
		Port:            integrationPort(),
		Database:        getEnvOrDefault("POSTGRES_DB", integrationDBName),
		User:            getEnvOrDefault("POSTGRES_USER", "vornik"),
		Password:        getEnvOrDefault("POSTGRES_PASSWORD", "vornik"),
		SSLMode:         "disable",
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: 1 * time.Minute,
		ConnectTimeout:  5 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := Connect(ctx, cfg)
	if err != nil {
		// No DB available — tests skipped, nothing to clean.
		return nil
	}
	defer func() { _ = db.Close() }()

	tx, err := db.DB.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// FK-safe order: leaves → children → parents. Each statement
	// targets both the uniqueID-prefixed namespaces (proj-*, other-*)
	// AND the short literal IDs ("p", "p1", "p2", "proj-1") that
	// ~30 repotest.go call sites pass straight through to Create().
	// `_external` is the real attribution-fallback bucket — DO NOT
	// match it. Real project IDs (assistant, snake, janka, ibkr-trader,
	// companion-example, …) are multi-character and don't start with
	// `proj-` or `other-`, so the IN-list keeps a tight blast radius.
	const cond = repotestLeftoverPredicate
	statements := []string{
		// Coverage-gap sweep (2026-06-18): leaves seeded by the new
		// contract suites. uniqueID() stamps test rows with the
		// prefixes matched below ('task-', 'op-', 'bot-', 'kind-',
		// 'proj-'); real IDs use 'task_' (underscore), emails, or
		// multi-char project names, so the blast radius stays tight.
		// These reference tasks via FK, so they precede the tasks
		// delete further down.
		"DELETE FROM a2a_push_configs WHERE task_id LIKE 'task-%'",
		"DELETE FROM budget_reservations WHERE " + cond,
		"DELETE FROM cross_project_calls WHERE caller_project LIKE 'proj-%' OR callee_project LIKE 'proj-%' OR caller_task_id LIKE 'task-%'",
		"DELETE FROM dispatcher_reminders WHERE operator_id LIKE 'op-%'",
		"DELETE FROM project_wizard_sessions WHERE operator_id LIKE 'op-%'",
		"DELETE FROM channel_sessions WHERE kind LIKE 'kind-%'",
		"DELETE FROM telegram_poller_state WHERE bot_id LIKE 'bot-%'",
		"DELETE FROM profile_use_audit WHERE operator_id LIKE 'op-%'",
		// Round-3: KG + memory + ingest leaves
		"DELETE FROM project_ingest_queue WHERE " + cond,
		"DELETE FROM project_memory_quarantine WHERE " + cond,
		"DELETE FROM memory_retrieval_audit WHERE " + cond,
		"DELETE FROM memory_ingest_audit WHERE " + cond,
		"DELETE FROM knowledge_edges WHERE " + cond,
		"DELETE FROM knowledge_entities WHERE " + cond,
		"DELETE FROM corpus_rollbacks WHERE " + cond,
		"DELETE FROM corpus_epochs_active WHERE " + cond,
		"DELETE FROM corpus_epochs WHERE " + cond,
		"DELETE FROM project_memory_chunks WHERE " + cond,
		"DELETE FROM project_skills WHERE " + cond,
		// repotest seeds execution_injected_skills with 'exec-' (hyphen)
		// fixtures; production execution IDs are 'exec_' (underscore), so
		// this prefix can't match real rows.
		"DELETE FROM execution_injected_skills WHERE execution_id LIKE 'exec-%'",
		// Round-2: trading children before orders
		"DELETE FROM trading_fills WHERE " + cond,
		"DELETE FROM trading_safety_events WHERE " + cond,
		"DELETE FROM trading_positions_snapshots WHERE " + cond,
		"DELETE FROM trading_orders WHERE " + cond,
		// Round-1: per-task verdict / scratchpad / message children
		"DELETE FROM task_judge_verdicts WHERE " + cond,
		"DELETE FROM task_post_mortems WHERE " + cond,
		"DELETE FROM intent_verdicts WHERE " + cond,
		"DELETE FROM autonomy_evaluations WHERE " + cond,
		"DELETE FROM telegram_task_threads WHERE task_id IN (SELECT id FROM tasks WHERE " + cond + ")",
		"DELETE FROM task_scratchpad WHERE task_id IN (SELECT id FROM tasks WHERE " + cond + ")",
		"DELETE FROM task_messages WHERE task_id IN (SELECT id FROM tasks WHERE " + cond + ")",
		// Execution children, executions, audit, artifacts, tasks
		"DELETE FROM execution_step_outcomes WHERE " + cond,
		"DELETE FROM executions WHERE " + cond,
		"DELETE FROM task_llm_usage WHERE " + cond,
		"DELETE FROM tool_audit_log WHERE " + cond,
		"DELETE FROM webhook_events WHERE " + cond,
		"DELETE FROM chat_audit_log WHERE " + cond,
		"DELETE FROM api_keys WHERE " + cond,
		"DELETE FROM extracted_documents WHERE " + cond,
		"DELETE FROM project_gists WHERE " + cond,
		"DELETE FROM artifacts WHERE " + cond,
		"DELETE FROM tasks WHERE " + cond,
		// Identity-core (migration 90). These tables have NO
		// project_id column, so the project-id predicate above can't
		// reach them — the RunIdentityRepositorySuite fixtures stamp
		// uniqueID prefixes on the id/name columns instead. FK-safe
		// order: ON DELETE CASCADE from users/groups would handle the
		// children, but we delete leaves-first explicitly so a
		// partial table set (bootstrap-from-001) still cleans cleanly.
		// link_codes is not seeded by the suite today but is listed
		// for completeness (Phase 4).
		"DELETE FROM link_codes WHERE user_id LIKE 'user-%'",
		"DELETE FROM user_identities WHERE id LIKE 'uident-%' OR user_id LIKE 'user-%'",
		"DELETE FROM group_members WHERE group_id LIKE 'grp-%' OR user_id LIKE 'user-%'",
		"DELETE FROM group_projects WHERE group_id LIKE 'grp-%'",
		"DELETE FROM groups WHERE id LIKE 'grp-%' OR name LIKE 'grpname-%'",
		"DELETE FROM users WHERE id LIKE 'user-%'",
	}
	for _, stmt := range statements {
		// Skip DELETEs against tables that don't exist in this DB —
		// the integration DB bootstraps from 001_initial.sql and not
		// every later-migration table is present when purge runs
		// after a partial test selection. to_regclass returns NULL
		// for absent tables; we extract the target name from the
		// DELETE prefix and probe before issuing the statement.
		const prefix = "DELETE FROM "
		if rest := strings.TrimPrefix(stmt, prefix); rest != stmt {
			table := rest
			if sp := strings.IndexByte(table, ' '); sp > 0 {
				table = table[:sp]
			}
			var present sql.NullString
			if err := tx.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, table).Scan(&present); err != nil {
				return fmt.Errorf("probe %s: %w", table, err)
			}
			if !present.Valid {
				continue
			}
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("exec %q: %w", stmt, err)
		}
	}
	return tx.Commit()
}

// resetSuiteTables clears tables whose shared repotest suite seeds FIXED
// literal ids, so the contract test is repeatable against a PERSISTENT
// database.
//
// Why this is needed at all: the repotest suites are shared with SQLite, where
// every test gets a fresh in-memory database and hardcoded fixture ids like
// "pr-1" / "lf-a" can never collide. Postgres runs against a durable DB, so
// the SECOND consecutive run of the lane failed with "duplicate key" — the
// purge in TestMain is scoped to project namespaces (proj-%, other-%) and
// never matched these non-project ids.
//
// Truncating before the suite rather than after is deliberate: an after-only
// sweep leaves residue whenever a run is killed or crashes mid-flight, which
// is exactly the state that then breaks the NEXT run.
//
// SAFETY: this refuses to run unless the connection really is on the
// dedicated integration database. POSTGRES_DB can be pointed at the daemon's
// live database, and truncating control_plane_proposals there would destroy
// real operator data — so the guard asks the server for current_database()
// rather than trusting the env var that got us here.
func resetSuiteTables(t *testing.T, db *DB, tables ...string) {
	t.Helper()
	var current string
	if err := db.DB.QueryRow(`SELECT current_database()`).Scan(&current); err != nil {
		t.Fatalf("resetSuiteTables: cannot determine current database: %v", err)
	}
	if current != integrationDBName {
		t.Fatalf("resetSuiteTables: refusing to truncate %v on database %q — "+
			"only the dedicated %q may be reset (unset POSTGRES_DB or point it at %q)",
			tables, current, integrationDBName, integrationDBName)
	}
	for _, table := range tables {
		if _, err := db.DB.Exec("TRUNCATE TABLE " + table); err != nil {
			t.Fatalf("resetSuiteTables: truncate %s: %v", table, err)
		}
	}
}

func newIntegrationDB(t *testing.T) *DB {
	t.Helper()
	cfg := Config{
		Host:            getEnvOrDefault("POSTGRES_HOST", "localhost"),
		Port:            integrationPort(),
		Database:        getEnvOrDefault("POSTGRES_DB", integrationDBName),
		User:            getEnvOrDefault("POSTGRES_USER", "vornik"),
		Password:        getEnvOrDefault("POSTGRES_PASSWORD", "vornik"),
		SSLMode:         "disable",
		MaxOpenConns:    5,
		MaxIdleConns:    2,
		ConnMaxLifetime: 5 * time.Minute,
		ConnMaxIdleTime: 2 * time.Minute,
		ConnectTimeout:  10 * time.Second,
	}
	ctx := context.Background()
	db, err := Connect(ctx, cfg)
	if err != nil {
		t.Skipf("postgres unavailable for integration tests: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestTaskWatcherRepository_PostgresContract runs the shared
// task_watchers contract suite against the Postgres implementation.
// Rows are created with test-unique task IDs (via repotest's
// uniqueID), so cross-test row leakage is impossible without a
// shared TaskID — no cleanup needed.
func TestTaskWatcherRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunTaskWatcherSuite(t, NewTaskWatcherRepository(db.DB))
}

// TestToolAuditRepository_PostgresContract — same shape on
// tool_audit_log.
func TestToolAuditRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunToolAuditSuite(t, NewToolAuditRepository(db.DB))
}

func TestRecoveryEventRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunRecoveryEventSuite(t, NewRecoveryEventRepository(db.DB))
}

// TestSkillRepository_PostgresContract — knowledge-skill store, the
// same suite that runs against SQLite. Must agree on scope isolation,
// the version-bump-on-edit upsert, and the maturity/role list filters.
func TestSkillRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunSkillSuite(t, NewSkillRepository(db.DB))
}

// TestProposalRepository_PostgresContract — the control-plane proposal
// ledger, the same suite that runs against SQLite.
func TestProposalRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	// Fixed fixture ids (pr-1, lf-a, ...) — see resetSuiteTables.
	resetSuiteTables(t, db, "control_plane_proposals")
	repotest.RunProposalSuite(t, NewProposalRepository(db.DB))
}

// TestCostTuningCanaryRepository_PostgresContract — the cost/quality canary
// guard's row store (design 2026-07-24 §4.3), the same suite that runs against
// SQLite.
func TestCostTuningCanaryRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	resetSuiteTables(t, db, "cost_tuning_canaries")
	repotest.RunCostTuningCanarySuite(t, NewCostTuningCanaryRepository(db.DB))
}

// TestCostAutoApplyTrust_PostgresContract — the cost-auto-apply cross-repo trust
// primitives (auto-apply design D1/D8), same suite that runs against SQLite.
func TestCostAutoApplyTrust_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	// Spans both stores, so both have to start clean.
	resetSuiteTables(t, db, "cost_tuning_canaries", "control_plane_proposals")
	repotest.RunCostAutoApplyTrustSuite(t, NewCostTuningCanaryRepository(db.DB), NewProposalRepository(db.DB))
}

// TestCostTuningCanaries_PartialIndex_Postgres asserts the WHERE status='open'
// partial index exists (design I5 — parity with the SQLite side).
func TestCostTuningCanaries_PartialIndex_Postgres(t *testing.T) {
	db := newIntegrationDB(t)
	var indexdef string
	err := db.DB.QueryRowContext(context.Background(), `
		SELECT indexdef FROM pg_indexes
		WHERE tablename = 'cost_tuning_canaries' AND indexname = 'idx_cost_tuning_canaries_open'`).Scan(&indexdef)
	if err != nil {
		t.Fatalf("partial index lookup: %v", err)
	}
	if !strings.Contains(indexdef, "status") || !strings.Contains(indexdef, "'open'") {
		t.Fatalf("idx_cost_tuning_canaries_open is not the expected partial index: %q", indexdef)
	}
}

// TestExecutionInjectedSkillRepository_PostgresContract — the
// execution→skill association, same suite the SQLite side runs.
func TestExecutionInjectedSkillRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunExecutionInjectedSkillSuite(t, NewExecutionInjectedSkillRepository(db.DB))
}

// TestArtifactRepository_PostgresContract — same shape on
// artifacts. The shared suite intentionally omits the UpdateTaskID
// happy path (needs a real tasks row to satisfy the FK on
// artifacts.task_id; deferred until TaskRepository lands in the
// shared layer + a seed-tasks helper exists). The validation
// branches for UpdateTaskID are covered directly in
// artifact_repository_test.go.
func TestArtifactRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunArtifactSuite(t, NewArtifactRepository(db.DB))
}

// TestTaskRepository_PostgresContract runs the shared TaskRepository
// suite against Postgres — exactly the same 12 sub-tests as the
// SQLite side. Postgres provides parallel lease pickup via
// FOR UPDATE SKIP LOCKED; the shared concurrency case asserts on
// correctness only (unique IDs across N goroutines), not timing.
func TestTaskRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunTaskRepositorySuite(t, NewTaskRepository(db.DB))
}

// TestAPIKeyRepository_PostgresContract — security-critical lookup
// contract. Same suite that runs against SQLite.
func TestAPIKeyRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunAPIKeyRepositorySuite(t, NewAPIKeyRepository(db.DB))
}

// TestIdentityRepository_PostgresContract — identity-core resolver
// contract (migration 90). Security-critical: the suite pins the
// revoked_at filter, the admin/user mixed row shape, and literal '*'
// project passthrough that internal/authz depends on.
func TestIdentityRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunIdentityRepositorySuite(t, NewIdentityRepository(db.DB))
}

// TestTaskLLMUsageRepository_PostgresContract — financial accounting.
func TestTaskLLMUsageRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunTaskLLMUsageSuite(t, NewTaskLLMUsageRepository(db.DB))
}

// TestAutonomyEvaluationRepository_PostgresContract — per-tick
// autonomy audit aggregator parity.
func TestAutonomyEvaluationRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunAutonomyEvaluationSuite(t, NewAutonomyEvaluationRepository(db.DB))
}

// TestInstinctRepository_PostgresContract — continuous-learning
// instinct layer (migrations 85/86). Same suite that pins the
// SQLite-side equivalent, so a behaviour divergence (e.g. the
// COUNT FILTER vs SUM(CASE) count derivation, or the GREATEST vs
// max last_seen_at upsert) surfaces as a test failure.
func TestInstinctRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunInstinctSuite(t, NewInstinctRepository(db.DB))
}

// TestInstinctLiftRepository_PostgresContract — lift-measurement snapshot
// store (migration 128). Same suite that runs against SQLite.
func TestInstinctLiftRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunInstinctLiftSuite(t, NewInstinctLiftRepository(db.DB), NewInstinctRepository(db.DB),
		NewExecutionStepOutcomeRepository(db.DB), NewTaskRepository(db.DB), NewWorkflowProposalRepository(db.DB))
}

// TestTradingOrderRepository_PostgresContract — broker audit
// channel + identity-mismatch safeguard. Same suite that pinned
// the SQLite-side equivalent.
func TestTradingOrderRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunTradingOrderSuite(t, NewTradingOrderRepository(db.DB))
}

// Round-1 promotions.
func TestWebhookEventRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunWebhookEventSuite(t, NewWebhookEventRepository(db.DB))
}
func TestTaskScratchpadRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunTaskScratchpadSuite(t,
		NewTaskScratchpadRepository(db.DB),
		NewTaskRepository(db.DB))
}
func TestTelegramThreadRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunTelegramThreadSuite(t,
		NewTelegramThreadRepository(db.DB),
		NewTaskRepository(db.DB))
}
func TestIntentVerdictRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunIntentVerdictSuite(t, NewIntentVerdictRepository(db.DB))
}
func TestTaskJudgeVerdictRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunTaskJudgeVerdictSuite(t,
		NewTaskJudgeVerdictRepository(db.DB),
		NewTaskRepository(db.DB))
}
func TestTaskPostMortemRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunTaskPostMortemSuite(t,
		NewTaskPostMortemRepository(db.DB),
		NewTaskRepository(db.DB))
}
func TestMemoryRetrievalAuditRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunMemoryRetrievalAuditSuite(t, NewMemoryRetrievalAuditRepository(db.DB))
}

// Round-2 trading-event contracts.
func TestTradingFillRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunTradingFillSuite(t,
		NewTradingFillRepository(db.DB),
		NewTradingOrderRepository(db.DB))
}
func TestTradingSafetyEventRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunTradingSafetyEventSuite(t, NewTradingSafetyEventRepository(db.DB))
}
func TestTradingSnapshotRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunTradingSnapshotSuite(t, NewTradingSnapshotRepository(db.DB))
}

// Round-3 memory + KG contracts.
func TestExecutionStepOutcomeRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunExecutionStepOutcomeSuite(t, NewExecutionStepOutcomeRepository(db.DB))
}
func TestKnowledgeEntityRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunKnowledgeEntitySuite(t, NewKnowledgeEntityRepository(db.DB))
}
func TestKnowledgeEdgeRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunKnowledgeEdgeSuite(t,
		NewKnowledgeEdgeRepository(db.DB),
		NewKnowledgeEntityRepository(db.DB))
}
func TestMemoryQuarantineRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunMemoryQuarantineSuite(t,
		NewMemoryQuarantineRepository(db.DB),
		NewArtifactRepository(db.DB))
}
func TestCorpusEpochRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunCorpusEpochSuite(t, NewCorpusEpochRepository(db.DB))
}
func TestIngestQueueRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunIngestQueueSuite(t,
		NewIngestQueueRepository(db.DB),
		NewArtifactRepository(db.DB))
}

// Round-4: pre-refactor coverage gap closure (2026-05-28).
func TestExecutionRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunExecutionRepositorySuite(t,
		NewExecutionRepository(db.DB),
		NewTaskRepository(db.DB))
}
func TestExtractedDocumentRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunExtractedDocumentSuite(t, NewExtractedDocumentRepository(db.DB))
}
func TestMemoryIngestAuditRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunMemoryIngestAuditSuite(t, NewMemoryIngestAuditRepository(db.DB))
}
func TestHealingCandidateRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunHealingCandidateSuite(t,
		NewWorkflowHealingCandidateRepository(db.DB),
		NewWorkflowHealingTriggerRepository(db.DB))
}
func TestHealingTrialRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunHealingTrialSuite(t,
		NewWorkflowHealingTrialRepository(db.DB),
		NewWorkflowHealingCandidateRepository(db.DB),
		NewWorkflowHealingTriggerRepository(db.DB))
}

// Coverage-gap sweep (2026-06-18): eight repositories that previously
// had no shared contract suite. The first three (A2A push config,
// budget reservation, project-wizard session) also run on SQLite; the
// remaining five are Postgres-only — their SQLite stubs are asserted
// separately in internal/persistence/sqlite/stub_contract_test.go.
func TestA2APushConfigRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunA2APushConfigSuite(t, NewA2APushConfigRepository(db.DB))
}
func TestBudgetReservationRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunBudgetReservationSuite(t, NewBudgetReservationRepository(db.DB))
}
func TestProjectWizardSessionRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunProjectWizardSessionSuite(t, NewProjectWizardSessionRepository(db.DB))
}
func TestInstallationOnboardingSessionRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunInstallationOnboardingSessionSuite(t, NewInstallationOnboardingSessionRepository(db.DB))
}
func TestFixItSessionRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunFixItSessionSuite(t, NewFixItSessionRepository(db.DB))
}
func TestCrossProjectCallRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunCrossProjectCallSuite(t, NewCrossProjectCallRepository(db.DB))
}
func TestReminderRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunReminderSuite(t, NewReminderRepository(db.DB))
}
func TestChannelSessionRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunChannelSessionSuite(t, NewChannelSessionRepository(db.DB))
}
func TestTelegramPollerStateRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunTelegramPollerStateSuite(t, NewTelegramPollerStateRepository(db.DB))
}
func TestProfileUseAuditRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunProfileUseAuditSuite(t, NewProfileUseAuditRepository(db.DB))
}
func TestExecutionNarrationRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunExecutionNarrationSuite(t, NewExecutionNarrationRepository(db.DB),
		NewExecutionRepository(db.DB), NewTaskRepository(db.DB))
}

func TestStepLatency_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunStepLatencySuite(t,
		NewExecutionStepOutcomeRepository(db.DB),
		NewExecutionRepository(db.DB),
		NewTaskRepository(db.DB))
}

// TestChannelDisclosureRepository_PostgresContract — the EU AI Act Art 50
// disclosure record (migration 139), the same suite that runs against SQLite.
// The dialects diverge here (TIMESTAMPTZ vs RFC3339 TEXT, NOW() vs a bound
// parameter), which is exactly why this must run on the integration lane:
// `go test ./...` is sqlite-only and would not catch a Postgres-side break.
func TestChannelDisclosureRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	// Fixed fixture ids, same class as the proposal/canary suites — see
	// resetSuiteTables. Safe despite this being the Art 50 evidence trail in
	// production: the helper refuses to truncate anything unless the
	// connection is genuinely on the dedicated integration database.
	resetSuiteTables(t, db, "channel_disclosure_log")
	repotest.RunChannelDisclosureSuite(t, NewChannelDisclosureRepository(db.DB))
}

// TestChatMemoryWriteConfirmationRepository_PostgresContract — the shared-scope memory-write
// confirmation two-step (chat memory-write design §5.3, migration 146), the same suite that
// runs against SQLite. The dialects diverge (TIMESTAMPTZ vs RFC3339 TEXT, NULL acknowledged_at,
// ON CONFLICT DO UPDATE), which is exactly why this must run on the integration lane.
func TestChatMemoryWriteConfirmationRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	// Fixed fixture ids (rt-1, ack-1, ...) — see resetSuiteTables.
	resetSuiteTables(t, db, "chat_memory_write_confirmations", "chat_memory_write_audit")
	repotest.RunChatMemoryWriteConfirmationSuite(t,
		NewChatMemoryWriteConfirmationRepository(db.DB),
		NewChatMemoryWriteAuditRepository(db.DB))
}

// TestMCPOAuthTokenRepository_PostgresContract — the Postgres half of the MCP OAuth token store
// contract. This backend carries the parts SQLite cannot exercise: the conditional UPDATE's row
// count semantics under a real dialect, and the transaction-scoped pg_advisory_xact_lock that
// makes a cross-daemon refresh non-wasteful rather than merely safe.
func TestMCPOAuthTokenRepository_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	resetSuiteTables(t, db, "mcp_oauth_tokens")
	repotest.RunMCPOAuthTokenSuite(t, NewMCPOAuthTokenRepository(db.DB))
}
