package retention

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

func TestResolve_PerProjectWinsOverDefaults(t *testing.T) {
	per := Policy{
		TaskLLMUsageDays: 30,
		ToolAuditDays:    7,
	}
	defaults := Policy{
		TaskLLMUsageDays: 90,
		ToolAuditDays:    30,
		TasksDays:        60,
		ExecutionsDays:   60,
		ArtifactsDays:    60,
	}
	got := Resolve("p1", per, defaults)

	// Per-project overrides win.
	assert.Equal(t, 30, got.TaskLLMUsageDays)
	assert.Equal(t, 7, got.ToolAuditDays)
	// Per-project zeros inherit from defaults.
	assert.Equal(t, 60, got.TasksDays)
	assert.Equal(t, 60, got.ExecutionsDays)
	assert.Equal(t, 60, got.ArtifactsDays)
}

func TestResolve_BothZeroFallsToCompiledDefault(t *testing.T) {
	got := Resolve("p", Policy{}, Policy{})
	assert.Equal(t, DefaultTaskLLMUsageDays, got.TaskLLMUsageDays)
	assert.Equal(t, DefaultToolAuditDays, got.ToolAuditDays)
	assert.Equal(t, DefaultTasksDays, got.TasksDays)
	assert.Equal(t, DefaultExecutionsDays, got.ExecutionsDays)
	assert.Equal(t, DefaultArtifactsDays, got.ArtifactsDays)
}

func TestResolve_AppliesMinimumFloor(t *testing.T) {
	// Negative and sub-minimum values get clamped to the floor.
	per := Policy{
		TaskLLMUsageDays: -5,
		ToolAuditDays:    0, // will inherit default
		TasksDays:        1, // at floor, unchanged
	}
	got := Resolve("p", per, Policy{TasksDays: 10}) // defaults.TasksDays ignored for this field
	assert.GreaterOrEqual(t, got.TaskLLMUsageDays, MinimumFloorDays)
	assert.GreaterOrEqual(t, got.ToolAuditDays, MinimumFloorDays)
	assert.Equal(t, 1, got.TasksDays)
}

// TestResolve_MemoryIngestAuditAlwaysOn confirms the new
// memory_ingest_audit window is always-on (default 90), honours
// per-project overrides, and clamps to the floor. Mitigation plan §7.3.
func TestResolve_MemoryIngestAuditAlwaysOn(t *testing.T) {
	// Both zero → compiled default.
	got := Resolve("p", Policy{}, Policy{})
	assert.Equal(t, DefaultMemoryIngestAuditDays, got.MemoryIngestAuditDays)
	assert.Equal(t, 90, got.MemoryIngestAuditDays)

	// Per-project wins over daemon default.
	got = Resolve("p", Policy{MemoryIngestAuditDays: 30}, Policy{MemoryIngestAuditDays: 120})
	assert.Equal(t, 30, got.MemoryIngestAuditDays)

	// Daemon default wins when per-project is zero.
	got = Resolve("p", Policy{}, Policy{MemoryIngestAuditDays: 120})
	assert.Equal(t, 120, got.MemoryIngestAuditDays)

	// Sub-floor clamps up.
	got = Resolve("p", Policy{MemoryIngestAuditDays: -5}, Policy{})
	assert.GreaterOrEqual(t, got.MemoryIngestAuditDays, MinimumFloorDays)
}

// TestPruneOlderThanMemoryIngestAudit confirms the sweep accepts the
// memory_ingest_audit table + ingested_at column (allowlist) and emits
// the expected project-scoped DELETE. Without the allowlist entries the
// sweep would error "forbidden table/column" and the audit table would
// grow unbounded (mitigation plan §8.3).
func TestPruneOlderThanMemoryIngestAudit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	s := New(db, zerolog.Nop())
	threshold := time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC)

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM memory_ingest_audit WHERE ingested_at < $1 AND project_id = $2")).
		WithArgs(threshold, "proj-a").
		WillReturnResult(sqlmock.NewResult(0, 4))

	n, err := s.pruneOlderThan(context.Background(), "memory_ingest_audit", "ingested_at", "project_id = $2", "proj-a", threshold, false)
	if err != nil {
		t.Fatalf("pruneOlderThan(memory_ingest_audit) error = %v", err)
	}
	if n != 4 {
		t.Fatalf("pruneOlderThan() = %d, want 4", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestPruneExpiredChunks_DeletesLinksThenChunks — chat memory-write
// design §5 / parent §6.4. The always-on expires_at sweep must delete
// the paired data_subject_links FIRST (no FK cascade backs them, review
// C2), then the expired chunks, both in one transaction.
func TestPruneExpiredChunks_DeletesLinksThenChunks(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	s := New(db, zerolog.Nop())
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM data_subject_links")).
		WithArgs("proj-a", now).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM project_memory_chunks")).
		WithArgs("proj-a", now).
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectCommit()

	n, err := s.pruneExpiredChunks(context.Background(), "proj-a", now, false)
	if err != nil {
		t.Fatalf("pruneExpiredChunks error = %v", err)
	}
	if n != 3 {
		t.Fatalf("pruneExpiredChunks() = %d, want 3 (chunk rows)", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestPruneExpiredChunks_PreviewCountsOnly — preview must not open a
// transaction or delete anything; it COUNTs the expired set.
func TestPruneExpiredChunks_PreviewCountsOnly(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	s := New(db, zerolog.Nop())
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM project_memory_chunks")).
		WithArgs("proj-a", now).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(5))

	n, err := s.pruneExpiredChunks(context.Background(), "proj-a", now, true)
	if err != nil {
		t.Fatalf("pruneExpiredChunks(preview) error = %v", err)
	}
	if n != 5 {
		t.Fatalf("pruneExpiredChunks(preview) = %d, want 5", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestResolve_MemoryPolicyEvalAlwaysOn confirms the firewall audit
// retention windows are always-on with the LLD-specified split
// defaults (allow 30, block 365), honour per-project + default
// overrides, and clamp to the floor. Without these the
// memory_policy_evaluations table grows forever (§8.3).
func TestResolve_MemoryPolicyEvalAlwaysOn(t *testing.T) {
	got := Resolve("p", Policy{}, Policy{})
	assert.Equal(t, DefaultMemoryPolicyEvalAllowDays, got.MemoryPolicyEvalAllowDays)
	assert.Equal(t, 30, got.MemoryPolicyEvalAllowDays)
	assert.Equal(t, DefaultMemoryPolicyEvalBlockDays, got.MemoryPolicyEvalBlockDays)
	assert.Equal(t, 365, got.MemoryPolicyEvalBlockDays)

	// Per-project wins.
	got = Resolve("p", Policy{MemoryPolicyEvalAllowDays: 7, MemoryPolicyEvalBlockDays: 90},
		Policy{MemoryPolicyEvalAllowDays: 14, MemoryPolicyEvalBlockDays: 180})
	assert.Equal(t, 7, got.MemoryPolicyEvalAllowDays)
	assert.Equal(t, 90, got.MemoryPolicyEvalBlockDays)

	// Default fills when per-project zero.
	got = Resolve("p", Policy{}, Policy{MemoryPolicyEvalAllowDays: 14, MemoryPolicyEvalBlockDays: 180})
	assert.Equal(t, 14, got.MemoryPolicyEvalAllowDays)
	assert.Equal(t, 180, got.MemoryPolicyEvalBlockDays)

	// Negative clamps to floor.
	got = Resolve("p", Policy{MemoryPolicyEvalAllowDays: -3, MemoryPolicyEvalBlockDays: -9}, Policy{})
	assert.GreaterOrEqual(t, got.MemoryPolicyEvalAllowDays, MinimumFloorDays)
	assert.GreaterOrEqual(t, got.MemoryPolicyEvalBlockDays, MinimumFloorDays)
}

// TestPruneOlderThanMemoryPolicyEvaluations confirms the allow + block
// split sweeps emit the expected decision-filtered DELETEs against the
// allowlisted memory_policy_evaluations table / evaluated_at column.
func TestPruneOlderThanMemoryPolicyEvaluations(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	s := New(db, zerolog.Nop())
	threshold := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	// Allow rows.
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM memory_policy_evaluations WHERE evaluated_at < $1 AND project_id = $2 AND decision = 'allow'")).
		WithArgs(threshold, "proj-a").
		WillReturnResult(sqlmock.NewResult(0, 12))
	nAllow, err := s.pruneOlderThan(context.Background(), "memory_policy_evaluations", "evaluated_at",
		"project_id = $2 AND decision = 'allow'", "proj-a", threshold, false)
	if err != nil {
		t.Fatalf("prune allow: %v", err)
	}
	if nAllow != 12 {
		t.Fatalf("allow prune = %d, want 12", nAllow)
	}

	// Block rows.
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM memory_policy_evaluations WHERE evaluated_at < $1 AND project_id = $2 AND decision <> 'allow'")).
		WithArgs(threshold, "proj-a").
		WillReturnResult(sqlmock.NewResult(0, 3))
	nBlock, err := s.pruneOlderThan(context.Background(), "memory_policy_evaluations", "evaluated_at",
		"project_id = $2 AND decision <> 'allow'", "proj-a", threshold, false)
	if err != nil {
		t.Fatalf("prune block: %v", err)
	}
	if nBlock != 3 {
		t.Fatalf("block prune = %d, want 3", nBlock)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestPathWithinRoot(t *testing.T) {
	cases := []struct {
		name string
		path string
		root string
		want bool
	}{
		{"simple within", "/var/artifacts/project-a/file.md", "/var/artifacts", true},
		{"nested within", "/var/artifacts/p/exec_xyz/a.md", "/var/artifacts", true},
		{"outside root", "/etc/passwd", "/var/artifacts", false},
		{"sibling path", "/var/other/file.md", "/var/artifacts", false},
		{"empty root disables check", "/var/artifacts/x", "", false},
		{"equal to root (no file)", "/var/artifacts", "/var/artifacts", false},
		{"traversal neutralised", "/var/artifacts/../etc/passwd", "/var/artifacts", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, pathWithinRoot(tc.path, tc.root))
		})
	}
}

func TestNilSweeperSafe(t *testing.T) {
	var s *Sweeper
	_, err := s.Sweep(context.TODO(), Policy{ProjectID: "p"})
	assert.NoError(t, err)
	_, err = s.Preview(context.TODO(), Policy{ProjectID: "p"})
	assert.NoError(t, err)
}

func TestPruneOlderThanPreviewAndDelete(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	s := New(db, zerolog.Nop())
	threshold := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM task_llm_usage WHERE recorded_at < $1 AND project_id = $2")).
		WithArgs(threshold, "proj-a").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))

	n, err := s.pruneOlderThan(context.Background(), "task_llm_usage", "recorded_at", "project_id = $2", "proj-a", threshold, true)
	if err != nil {
		t.Fatalf("preview pruneOlderThan() error = %v", err)
	}
	if n != 3 {
		t.Fatalf("preview pruneOlderThan() = %d, want 3", n)
	}

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM tasks WHERE updated_at < $1 AND project_id = $2 AND status IN ('COMPLETED','FAILED','CANCELLED')")).
		WithArgs(threshold, "proj-a").
		WillReturnResult(sqlmock.NewResult(0, 2))

	n, err = s.pruneOlderThan(context.Background(), "tasks", "updated_at", "project_id = $2 AND status IN ('COMPLETED','FAILED','CANCELLED')", "proj-a", threshold, false)
	if err != nil {
		t.Fatalf("delete pruneOlderThan() error = %v", err)
	}
	if n != 2 {
		t.Fatalf("delete pruneOlderThan() = %d, want 2", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestPruneOlderThanRejectsUnexpectedIdentifiers(t *testing.T) {
	s := &Sweeper{}
	threshold := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	if _, err := s.pruneOlderThan(context.Background(), "tasks; DROP TABLE tasks", "updated_at", "project_id = $2", "p", threshold, true); err == nil {
		t.Fatal("expected forbidden table error")
	}
	if _, err := s.pruneOlderThan(context.Background(), "tasks", "updated_at; DROP", "project_id = $2", "p", threshold, true); err == nil {
		t.Fatal("expected forbidden column error")
	}
}

func TestPruneArtifactsPreviewCountsRowsAndExistingFiles(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	root := t.TempDir()
	existing := filepath.Join(root, "exec-1", "output.txt")
	if err := os.MkdirAll(filepath.Dir(existing), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(existing, []byte("artifact"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatalf("WriteFile outside: %v", err)
	}

	threshold := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, storage_path FROM artifacts WHERE project_id = $1 AND created_at < $2")).
		WithArgs("proj-a", threshold).
		WillReturnRows(sqlmock.NewRows([]string{"id", "storage_path"}).
			AddRow("art-1", existing).
			AddRow("art-2", filepath.Join(root, "missing.txt")).
			AddRow("art-3", outside).
			AddRow("art-4", ""))

	s := New(db, zerolog.Nop())
	files, rows, err := s.pruneArtifacts(context.Background(), "proj-a", threshold, root, true)
	if err != nil {
		t.Fatalf("pruneArtifacts preview error = %v", err)
	}
	if files != 1 || rows != 4 {
		t.Fatalf("pruneArtifacts preview = files %d rows %d, want files 1 rows 4", files, rows)
	}
	if _, err := os.Stat(existing); err != nil {
		t.Fatalf("preview should not remove existing artifact: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestPruneArtifactsDeleteRemovesFilesAndRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	root := t.TempDir()
	existing := filepath.Join(root, "exec-1", "output.txt")
	if err := os.MkdirAll(filepath.Dir(existing), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(existing, []byte("artifact"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	threshold := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, storage_path FROM artifacts WHERE project_id = $1 AND created_at < $2")).
		WithArgs("proj-a", threshold).
		WillReturnRows(sqlmock.NewRows([]string{"id", "storage_path"}).AddRow("art-1", existing))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM artifacts WHERE id = ANY($1)")).
		WillReturnResult(sqlmock.NewResult(0, 1))

	s := New(db, zerolog.Nop())
	files, rows, err := s.pruneArtifacts(context.Background(), "proj-a", threshold, root, false)
	if err != nil {
		t.Fatalf("pruneArtifacts delete error = %v", err)
	}
	if files != 1 || rows != 1 {
		t.Fatalf("pruneArtifacts delete = files %d rows %d, want files 1 rows 1", files, rows)
	}
	if _, err := os.Stat(existing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected artifact file removed, stat err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// ---- slice-3 opt-in fields (task_messages + project_memory_chunks) ----

func TestResolve_OptInFieldsDefaultZero(t *testing.T) {
	// Both perProject and defaults zero → opt-in fields stay at 0.
	got := Resolve("p", Policy{}, Policy{})
	assert.Equal(t, 0, got.TaskMessagesDays, "task_messages_days defaults disabled")
	assert.Equal(t, 0, got.MemoryChunksDays, "memory_chunks_days defaults disabled")
}

func TestResolve_OptInFieldsRespectPerProject(t *testing.T) {
	per := Policy{TaskMessagesDays: 14, MemoryChunksDays: 365}
	got := Resolve("p", per, Policy{})
	assert.Equal(t, 14, got.TaskMessagesDays)
	assert.Equal(t, 365, got.MemoryChunksDays)
}

func TestResolve_OptInFieldsInheritDefaults(t *testing.T) {
	defaults := Policy{TaskMessagesDays: 30, MemoryChunksDays: 180}
	got := Resolve("p", Policy{}, defaults)
	assert.Equal(t, 30, got.TaskMessagesDays)
	assert.Equal(t, 180, got.MemoryChunksDays)
}

func TestResolve_OptInFieldsAtFloorUnchanged(t *testing.T) {
	got := Resolve("p", Policy{TaskMessagesDays: 1, MemoryChunksDays: 1}, Policy{})
	assert.Equal(t, 1, got.TaskMessagesDays)
	assert.Equal(t, 1, got.MemoryChunksDays)
}

func TestPruneTaskMessagesPreviewCounts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	threshold := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery("SELECT COUNT").
		WithArgs("proj-a", threshold).
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(7))

	s := New(db, zerolog.Nop())
	n, err := s.pruneTaskMessages(context.Background(), "proj-a", threshold, true)
	if err != nil {
		t.Fatalf("pruneTaskMessages preview error = %v", err)
	}
	if n != 7 {
		t.Errorf("preview count = %d, want 7", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestPruneTaskMessagesDeletesRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	threshold := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectExec("DELETE FROM task_messages").
		WithArgs("proj-a", threshold).
		WillReturnResult(sqlmock.NewResult(0, 12))

	s := New(db, zerolog.Nop())
	n, err := s.pruneTaskMessages(context.Background(), "proj-a", threshold, false)
	if err != nil {
		t.Fatalf("pruneTaskMessages delete error = %v", err)
	}
	if n != 12 {
		t.Errorf("delete affected = %d, want 12", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestPruneOlderThan_AllowsProjectMemoryChunks(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	threshold := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectExec("DELETE FROM project_memory_chunks").
		WillReturnResult(sqlmock.NewResult(0, 4))

	s := New(db, zerolog.Nop())
	n, err := s.pruneOlderThan(context.Background(),
		"project_memory_chunks", "created_at",
		"project_id = $2", "proj-a", threshold, false,
	)
	if err != nil {
		t.Fatalf("pruneOlderThan project_memory_chunks: %v", err)
	}
	if n != 4 {
		t.Errorf("affected = %d, want 4", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestPruneOlderThan_RejectsForbiddenTable(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	s := New(db, zerolog.Nop())
	_, err = s.pruneOlderThan(context.Background(),
		"pg_catalog.pg_class", "created_at",
		"project_id = $2", "p", time.Now(), false,
	)
	if err == nil {
		t.Fatal("forbidden table must error")
	}
}

func TestPruneOlderThan_RejectsForbiddenColumn(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	s := New(db, zerolog.Nop())
	_, err = s.pruneOlderThan(context.Background(),
		"tasks", "expires_at_secret",
		"project_id = $2", "p", time.Now(), false,
	)
	if err == nil {
		t.Fatal("forbidden timestamp column must error")
	}
}

func TestSweep_OptInFieldsOffSkipsExtraQueries(t *testing.T) {
	// Default (TaskMessagesDays == 0, MemoryChunksDays == 0) must
	// NOT issue queries against task_messages or
	// project_memory_chunks. We let sqlmock fail loudly if any
	// such query slips through.
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery("SELECT COUNT.*FROM task_llm_usage").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	mock.ExpectQuery("SELECT COUNT.*FROM tool_audit_log").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	mock.ExpectQuery("SELECT id, storage_path FROM artifacts").
		WillReturnRows(sqlmock.NewRows([]string{"id", "storage_path"}))
	mock.ExpectQuery("SELECT COUNT.*FROM tasks").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	mock.ExpectQuery("SELECT COUNT.*FROM executions").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	// The expires_at sweep is ALWAYS-ON (TTL-verified-to-DELETE, §6.4), so even
	// with MemoryChunksDays==0 project_memory_chunks IS queried — by expires_at,
	// not by the created_at operator cap (which stays off).
	mock.ExpectQuery("SELECT COUNT.*FROM project_memory_chunks.*expires_at").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	// memory_ingest_audit is always-on (default 90d) → always queried.
	mock.ExpectQuery("SELECT COUNT.*FROM memory_ingest_audit").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	// memory_policy_evaluations is always-on (allow + block split) → two queries.
	mock.ExpectQuery("SELECT COUNT.*FROM memory_policy_evaluations.*decision = 'allow'").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	mock.ExpectQuery("SELECT COUNT.*FROM memory_policy_evaluations.*decision <> 'allow'").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))

	s := New(db, zerolog.Nop())
	p := Resolve("p", Policy{}, Policy{
		TaskLLMUsageDays: 90, ToolAuditDays: 30,
		TasksDays: 60, ExecutionsDays: 60, ArtifactsDays: 60,
	})
	if _, err := s.Preview(context.Background(), p); err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet (or extra) sql expectations: %v", err)
	}
}

func TestSweep_OptInFieldsOnIssuesExtraQueries(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery("SELECT COUNT.*FROM task_llm_usage").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	mock.ExpectQuery("SELECT COUNT.*FROM tool_audit_log").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	mock.ExpectQuery("SELECT id, storage_path FROM artifacts").
		WillReturnRows(sqlmock.NewRows([]string{"id", "storage_path"}))
	mock.ExpectQuery("SELECT COUNT.*FROM tasks").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	mock.ExpectQuery("SELECT COUNT.*FROM executions").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))

	// Opt-in prune queries, gated on TaskMessagesDays > 0 and
	// MemoryChunksDays > 0.
	mock.ExpectQuery("SELECT COUNT.*FROM task_messages").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(3))
	// The created_at operator cap (step 7, MemoryChunksDays>0) …
	mock.ExpectQuery("SELECT COUNT.*FROM project_memory_chunks.*created_at").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(99))
	// … then the always-on expires_at sweep (step 7b, §6.4).
	mock.ExpectQuery("SELECT COUNT.*FROM project_memory_chunks.*expires_at").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	// memory_ingest_audit is always-on (default 90d) → always queried.
	mock.ExpectQuery("SELECT COUNT.*FROM memory_ingest_audit").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	// memory_policy_evaluations is always-on (allow + block split) → two queries.
	mock.ExpectQuery("SELECT COUNT.*FROM memory_policy_evaluations.*decision = 'allow'").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	mock.ExpectQuery("SELECT COUNT.*FROM memory_policy_evaluations.*decision <> 'allow'").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))

	s := New(db, zerolog.Nop())
	p := Resolve("p",
		Policy{TaskMessagesDays: 30, MemoryChunksDays: 365},
		Policy{
			TaskLLMUsageDays: 90, ToolAuditDays: 30,
			TasksDays: 60, ExecutionsDays: 60, ArtifactsDays: 60,
		})
	counts, err := s.Preview(context.Background(), p)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if counts.TaskMessages != 3 {
		t.Errorf("TaskMessages count = %d, want 3", counts.TaskMessages)
	}
	if counts.MemoryChunks != 99 {
		t.Errorf("MemoryChunks count = %d, want 99", counts.MemoryChunks)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestSweepGlobal_DisabledShortCircuits(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	s := New(db, zerolog.Nop())
	// ui_sessions cleanup is unconditional (no knob); the table is
	// absent here so it's a single probe + no-op.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.ui_sessions')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))
	// api_keys cleanup is also unconditional; absent table → no-op.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.api_keys')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))
	// link_codes cleanup is also unconditional; absent table → no-op.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.link_codes')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))
	counts, err := s.SweepGlobal(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("SweepGlobal(0): %v", err)
	}
	if counts.ResponseCache != 0 {
		t.Errorf("expected 0 response-cache prunes when disabled, got %d", counts.ResponseCache)
	}
	if counts.UISessions != 0 {
		t.Errorf("expected 0 ui_sessions prunes when table absent, got %d", counts.UISessions)
	}
	if counts.APIKeys != 0 {
		t.Errorf("expected 0 api_keys prunes when table absent, got %d", counts.APIKeys)
	}
	if counts.EmbeddingCache != 0 {
		t.Errorf("expected 0 embedding-cache prunes when disabled, got %d", counts.EmbeddingCache)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected SQL: %v", err)
	}
}

func TestSweepGlobal_TableAbsentNoOp(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	s := New(db, zerolog.Nop())
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.llm_response_cache')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.ui_sessions')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.api_keys')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.link_codes')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))

	counts, err := s.SweepGlobal(context.Background(), 30, 0)
	if err != nil {
		t.Fatalf("SweepGlobal: %v", err)
	}
	if counts.ResponseCache != 0 {
		t.Errorf("expected 0 when table absent, got %d", counts.ResponseCache)
	}
}

func TestPreviewGlobal_CountsWithoutDelete(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	s := New(db, zerolog.Nop())
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.llm_response_cache')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM llm_response_cache WHERE last_hit_at <")).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(17))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.ui_sessions')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM ui_sessions WHERE")).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(4))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.api_keys')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM api_keys WHERE (expires_at IS NOT NULL AND expires_at < $1) OR (revoked_at IS NOT NULL AND revoked_at < $1)")).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(6))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.link_codes')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM link_codes WHERE expires_at < $1 OR (used_at IS NOT NULL AND used_at < $1)")).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))

	counts, err := s.PreviewGlobal(context.Background(), 30, 0)
	if err != nil {
		t.Fatalf("PreviewGlobal: %v", err)
	}
	if counts.ResponseCache != 17 {
		t.Errorf("expected 17 preview rows, got %d", counts.ResponseCache)
	}
	if counts.UISessions != 4 {
		t.Errorf("expected 4 ui_sessions preview rows, got %d", counts.UISessions)
	}
	if counts.APIKeys != 6 {
		t.Errorf("expected 6 api_keys preview rows, got %d", counts.APIKeys)
	}
	if counts.LinkCodes != 2 {
		t.Errorf("expected 2 link_codes preview rows, got %d", counts.LinkCodes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sql: %v", err)
	}
}

func TestSweepGlobal_DeleteRemovesRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	s := New(db, zerolog.Nop())
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.llm_response_cache')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM llm_response_cache WHERE last_hit_at <")).
		WillReturnResult(sqlmock.NewResult(0, 5))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.ui_sessions')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM ui_sessions WHERE")).
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.api_keys')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM api_keys WHERE")).
		WillReturnResult(sqlmock.NewResult(0, 7))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.link_codes')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM link_codes WHERE")).
		WillReturnResult(sqlmock.NewResult(0, 2))

	counts, err := s.SweepGlobal(context.Background(), 30, 0)
	if err != nil {
		t.Fatalf("SweepGlobal: %v", err)
	}
	if counts.ResponseCache != 5 {
		t.Errorf("expected 5 deleted rows, got %d", counts.ResponseCache)
	}
	if counts.UISessions != 3 {
		t.Errorf("expected 3 ui_sessions deleted, got %d", counts.UISessions)
	}
	if counts.APIKeys != 7 {
		t.Errorf("expected 7 api_keys deleted, got %d", counts.APIKeys)
	}
	if counts.LinkCodes != 2 {
		t.Errorf("expected 2 link_codes deleted, got %d", counts.LinkCodes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sql: %v", err)
	}
}

// TestSweepGlobal_UISessionsDeletedWithGrace pins the ui_sessions
// cleanup independently of the response cache: with the cache
// disabled (days=0) the sweeper still probes + deletes expired/revoked
// sessions past the 7-day grace.
func TestSweepGlobal_UISessionsDeletedWithGrace(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	s := New(db, zerolog.Nop())
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.ui_sessions')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM ui_sessions WHERE expires_at < $1 OR (revoked_at IS NOT NULL AND revoked_at < $1)")).
		WillReturnResult(sqlmock.NewResult(0, 9))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.api_keys')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.link_codes')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))

	counts, err := s.SweepGlobal(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("SweepGlobal: %v", err)
	}
	if counts.UISessions != 9 {
		t.Errorf("expected 9 ui_sessions deleted, got %d", counts.UISessions)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sql: %v", err)
	}
}

// TestSweepGlobal_LinkCodesDeletedWithGrace pins the link_codes cleanup:
// expired-or-used codes past the 7-day grace are hard-deleted. The table has
// no writers yet, but the sweep must run so codes can't accumulate once
// Phase 4 starts minting them.
func TestSweepGlobal_LinkCodesDeletedWithGrace(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	s := New(db, zerolog.Nop())
	// ui_sessions + api_keys absent here so the test isolates link_codes.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.ui_sessions')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.api_keys')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.link_codes')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM link_codes WHERE expires_at < $1 OR (used_at IS NOT NULL AND used_at < $1)")).
		WillReturnResult(sqlmock.NewResult(0, 4))

	counts, err := s.SweepGlobal(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("SweepGlobal: %v", err)
	}
	if counts.LinkCodes != 4 {
		t.Errorf("expected 4 link_codes deleted, got %d", counts.LinkCodes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sql: %v", err)
	}
}

// TestSweepGlobal_APIKeysDeletedWithGrace pins the api_keys cleanup:
// expired and revoked rows past the 7-day grace are hard-deleted;
// active rows are never touched.
func TestSweepGlobal_APIKeysDeletedWithGrace(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	s := New(db, zerolog.Nop())
	// ui_sessions absent, api_keys present.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.ui_sessions')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.api_keys')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM api_keys WHERE (expires_at IS NOT NULL AND expires_at < $1) OR (revoked_at IS NOT NULL AND revoked_at < $1)")).
		WillReturnResult(sqlmock.NewResult(0, 12))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.link_codes')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))

	counts, err := s.SweepGlobal(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("SweepGlobal: %v", err)
	}
	if counts.APIKeys != 12 {
		t.Errorf("expected 12 api_keys deleted, got %d", counts.APIKeys)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sql: %v", err)
	}
}

func TestSweepGlobal_NilDBSafe(t *testing.T) {
	var s *Sweeper
	if _, err := s.SweepGlobal(context.Background(), 30, 0); err != nil {
		t.Errorf("nil sweeper: %v", err)
	}
}

// TestSweepGlobal_EmbeddingCacheDeleteRemovesRows pins the embedding_cache
// eviction: with the response cache disabled (days=0) and embedding
// days=30, the sweeper probes + deletes cold rows by last_hit_at. Closes
// the historical gap where embedding_cache had a last_hit_at index but no
// sweep, so the table grew unbounded (migration 41 staged the index for
// exactly this eviction).
func TestSweepGlobal_EmbeddingCacheDeleteRemovesRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	s := New(db, zerolog.Nop())
	// response cache disabled (days=0) → no llm_response_cache probe.
	// embedding_cache runs next; other unconditional tables absent so
	// this test isolates the embedding cache.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.embedding_cache')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM embedding_cache WHERE last_hit_at < $1")).
		WillReturnResult(sqlmock.NewResult(0, 8))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.ui_sessions')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.api_keys')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.link_codes')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))

	counts, err := s.SweepGlobal(context.Background(), 0, 30)
	if err != nil {
		t.Fatalf("SweepGlobal: %v", err)
	}
	if counts.EmbeddingCache != 8 {
		t.Errorf("expected 8 embedding_cache deleted, got %d", counts.EmbeddingCache)
	}
	if counts.ResponseCache != 0 {
		t.Errorf("expected response cache untouched when disabled, got %d", counts.ResponseCache)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sql: %v", err)
	}
}

// TestSweepGlobal_EmbeddingCacheTableAbsentNoOp: enabled but table missing
// (deployment predating migration 41) is a single probe + no-op, not a 500.
func TestSweepGlobal_EmbeddingCacheTableAbsentNoOp(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	s := New(db, zerolog.Nop())
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.embedding_cache')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.ui_sessions')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.api_keys')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.link_codes')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))

	counts, err := s.SweepGlobal(context.Background(), 0, 30)
	if err != nil {
		t.Fatalf("SweepGlobal: %v", err)
	}
	if counts.EmbeddingCache != 0 {
		t.Errorf("expected 0 embedding_cache prunes when table absent, got %d", counts.EmbeddingCache)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sql: %v", err)
	}
}

// TestPreviewGlobal_EmbeddingCache pins that preview counts embedding_cache
// rows without deleting them.
func TestPreviewGlobal_EmbeddingCache(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	s := New(db, zerolog.Nop())
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.embedding_cache')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM embedding_cache WHERE last_hit_at < $1")).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(23))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.ui_sessions')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.api_keys')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.link_codes')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))

	counts, err := s.PreviewGlobal(context.Background(), 0, 30)
	if err != nil {
		t.Fatalf("PreviewGlobal: %v", err)
	}
	if counts.EmbeddingCache != 23 {
		t.Errorf("expected 23 embedding_cache preview rows, got %d", counts.EmbeddingCache)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sql: %v", err)
	}
}

// The AI Act Art 50 disclosure log is the Art 99 evidence trail: the record
// that the "you are interacting with an AI system" notice was actually served.
// An obligation met but unprovable is worth little under enforcement, so
// pruning this table converts a compliant deployment into an indefensible one.
//
// Before this guard it was protected only by ABSENCE from two allowlists, plus
// a documentation note. Absence protects by accident — anyone adding a line
// while wiring a new cleanup would have started pruning it, and nothing would
// have failed.
//
// see LLD § https://docs.vornik.io
func TestEvidenceTablesAreNeverPrunable(t *testing.T) {
	if !evidenceTables["channel_disclosure_log"] {
		t.Fatal("channel_disclosure_log must be listed as a conformity evidence table")
	}
	// Belt: it must not appear in either allowlist. If someone adds it, this
	// fails even though the deny would still refuse at runtime — the intent is
	// that nobody should be able to *believe* it is prunable.
	for table := range evidenceTables {
		if globalCleanupTables[table] {
			t.Errorf("%s is an evidence table but appears in globalCleanupTables", table)
		}
	}
	// Braces: the deny must be checked BEFORE the allowlist, so adding the
	// table to an allowlist cannot enable pruning. Exercised through the real
	// entry points with a nil DB — the refusal must happen before any query,
	// so a nil handle is safe and proves the ordering.
	s := &Sweeper{}
	if _, err := s.pruneOlderThan(context.Background(), "channel_disclosure_log", "served_at", "TRUE", "", time.Now(), false); err == nil {
		t.Error("pruneOlderThan must refuse the evidence table")
	} else if !strings.Contains(err.Error(), "evidence trail") {
		t.Errorf("refusal should name the reason, got: %v", err)
	}
	if _, err := s.pruneGlobalByThreshold(context.Background(), "channel_disclosure_log", "TRUE", time.Now(), false); err == nil {
		t.Error("pruneGlobalByThreshold must refuse the evidence table")
	} else if !strings.Contains(err.Error(), "evidence trail") {
		t.Errorf("refusal should name the reason, got: %v", err)
	}
	// And a preview must refuse too — a COUNT that succeeds would imply the
	// table is in scope for a future DELETE.
	if _, err := s.pruneOlderThan(context.Background(), "channel_disclosure_log", "served_at", "TRUE", "", time.Now(), true); err == nil {
		t.Error("preview must refuse the evidence table as well")
	}
}
