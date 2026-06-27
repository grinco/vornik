package memory

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestHardEvict_HappyPath_TxSnapshotInsertDelete — the canonical
// shape. SELECT FOR UPDATE pulls the snapshot, audit rows insert,
// DELETE fires, COMMIT closes. Asserting on this sequence pins the
// audit-before-delete ordering (so a panic between the two leaves
// the chunk intact, not an orphan audit row).
func TestHardEvict_HappyPath_TxSnapshotInsertDelete(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)

	mock.ExpectBegin()
	// Snapshot fetch with FOR UPDATE — the SELECT must include the
	// project_id filter (IDOR guard) AND the FOR UPDATE clause
	// (prevents concurrent edits between snapshot and delete).
	mock.ExpectQuery(regexp.QuoteMeta("FROM project_memory_chunks")).
		WithArgs("janka", "chunk_1", "chunk_2").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "content_hash", "source_name", "content_class", "producer_role",
		}).
			AddRow("chunk_1", "hash1", "src1", "decision", "researcher").
			AddRow("chunk_2", "hash2", "src2", "research", "scout"))
	// One audit insert per evicted chunk — both must land before
	// the DELETE.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO memory_eviction_audit")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO memory_eviction_audit")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// DELETE the chunks. FK CASCADE handles memory_embed_queue +
	// memory_embed_dlq + entity_mentions automatically — those
	// rows aren't issued by HardEvict.
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM project_memory_chunks")).
		WithArgs("janka", "chunk_1", "chunk_2").
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	audit, err := repo.HardEvict(context.Background(), "janka",
		[]string{"chunk_1", "chunk_2"}, "GDPR DSAR 12", "operator-jane")
	if err != nil {
		t.Fatalf("HardEvict: %v", err)
	}
	if len(audit) != 2 {
		t.Fatalf("audit rows = %d, want 2", len(audit))
	}
	if audit[0].ChunkID != "chunk_1" || audit[1].ChunkID != "chunk_2" {
		t.Errorf("audit chunk_ids = %v", audit)
	}
	if audit[0].ContentClass != "decision" || audit[1].ProducerRole != "scout" {
		t.Errorf("audit denormalised snapshot mismatch: %+v", audit)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestHardEvict_NoMatchingChunks_CommitsEmptyTx — passing IDs that
// don't exist (or live under a different project) must NOT issue
// a DELETE with empty IN-list (postgres rejects that) and must NOT
// leave the lock held. The expected shape: BEGIN → snapshot
// returns zero rows → COMMIT.
func TestHardEvict_NoMatchingChunks_CommitsEmptyTx(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM project_memory_chunks")).
		WithArgs("janka", "ghost_1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "content_hash", "source_name", "content_class", "producer_role",
		}))
	mock.ExpectCommit()

	audit, err := repo.HardEvict(context.Background(), "janka",
		[]string{"ghost_1"}, "stale id check", "tester")
	if err != nil {
		t.Fatalf("HardEvict: %v", err)
	}
	if len(audit) != 0 {
		t.Errorf("expected no audit rows for ghost IDs, got %+v", audit)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestHardEvict_EmptyInput_NoSQL — defensive: passing nil/empty
// chunkIDs must not even open a transaction. Pre-flight noop —
// matches the MarkRefutedByIDs convention.
func TestHardEvict_EmptyInput_NoSQL(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)

	audit, err := repo.HardEvict(context.Background(), "janka", nil, "r", "by")
	if err != nil || audit != nil {
		t.Errorf("nil chunks: audit=%v err=%v, want (nil,nil)", audit, err)
	}
	// ExpectationsWereMet would fail if any SQL ran since we set
	// no expectations.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("nil-input HardEvict issued SQL: %v", err)
	}
}

// TestHardEvict_AuditInsertFailure_RollsBack — the audit-row write
// is the GDPR compliance hook. If the audit insert fails the
// DELETE must NOT fire — the chunk survives so the operator can
// retry.
func TestHardEvict_AuditInsertFailure_RollsBack(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM project_memory_chunks")).
		WithArgs("janka", "chunk_1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "content_hash", "source_name", "content_class", "producer_role",
		}).AddRow("chunk_1", "h1", "s1", "c1", "r1"))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO memory_eviction_audit")).
		WillReturnError(errors.New("disk-full or constraint"))
	mock.ExpectRollback()

	_, err := repo.HardEvict(context.Background(), "janka",
		[]string{"chunk_1"}, "test", "tester")
	if err == nil {
		t.Fatalf("expected error from audit insert failure, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestHardEvict_DeleteFailure_RollsBackAudit — symmetric: if the
// DELETE fails the audit rows must roll back too. No "we evicted X"
// ghost row for a chunk that's still there.
func TestHardEvict_DeleteFailure_RollsBackAudit(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM project_memory_chunks")).
		WithArgs("janka", "chunk_1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "content_hash", "source_name", "content_class", "producer_role",
		}).AddRow("chunk_1", "h", "s", "c", "r"))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO memory_eviction_audit")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM project_memory_chunks")).
		WithArgs("janka", "chunk_1").
		WillReturnError(errors.New("connection reset"))
	mock.ExpectRollback()

	_, err := repo.HardEvict(context.Background(), "janka",
		[]string{"chunk_1"}, "test", "tester")
	if err == nil {
		t.Fatalf("expected error from DELETE failure, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestHardEvict_NotConfigured — nil repo / no DB must error rather
// than panic. Mirrors the MarkRefutedByIDs nil-safety pattern.
func TestHardEvict_NotConfigured(t *testing.T) {
	var nilRepo *Repository
	_, err := nilRepo.HardEvict(context.Background(), "p", []string{"c"}, "r", "by")
	if err == nil {
		t.Error("nil repo: expected error, got nil")
	}

	repo := &Repository{} // db unset
	_, err = repo.HardEvict(context.Background(), "p", []string{"c"}, "r", "by")
	if err == nil {
		t.Error("nil db: expected error, got nil")
	}
}

// TestHardEvict_EmptyProjectID — project filter is the IDOR guard.
// Calling without a project_id MUST error rather than silently
// evicting across every project.
func TestHardEvict_EmptyProjectID(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)
	_, err := repo.HardEvict(context.Background(), "", []string{"chunk_1"}, "r", "by")
	if err == nil {
		t.Error("empty project: expected error, got nil")
	}
}

// TestCorrector_HardEvict_NilSafe — the Corrector wrapper is the
// dispatcher / CLI / API call site. Nil-safe checks must surface
// errors rather than panic.
func TestCorrector_HardEvict_NilSafe(t *testing.T) {
	var nilCorr *Corrector
	_, err := nilCorr.HardEvict(context.Background(), "p", []string{"c"}, "r", "by")
	if err == nil {
		t.Error("nil corrector: expected error, got nil")
	}

	corrWithNilRepo := &Corrector{Repo: nil}
	_, err = corrWithNilRepo.HardEvict(context.Background(), "p", []string{"c"}, "r", "by")
	if err == nil {
		t.Error("corrector with nil repo: expected error, got nil")
	}
}

// TestCorrector_HardEvict_EmptyInputsAreNoop — empty project ID
// errors; empty chunkIDs is a silent noop (matches Repo).
func TestCorrector_HardEvict_EmptyInputsAreNoop(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	corr := NewCorrector(NewRepository(db), nil)

	_, err := corr.HardEvict(context.Background(), "", []string{"c"}, "r", "by")
	if err == nil {
		t.Error("empty project: expected error, got nil")
	}

	audit, err := corr.HardEvict(context.Background(), "p", nil, "r", "by")
	if err != nil || audit != nil {
		t.Errorf("nil chunks: audit=%v err=%v, want (nil,nil)", audit, err)
	}
}

// TestCorrector_HardEvict_Delegates — the wrapper must actually
// hit the repo (not just validate). Use a happy-path sqlmock to
// verify the transaction shape flows through the wrapper.
func TestCorrector_HardEvict_Delegates(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	corr := NewCorrector(NewRepository(db), nil)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM project_memory_chunks")).
		WithArgs("p", "c1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "content_hash", "source_name", "content_class", "producer_role",
		}).AddRow("c1", "h", "s", "c", "r"))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO memory_eviction_audit")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM project_memory_chunks")).
		WithArgs("p", "c1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	audit, err := corr.HardEvict(context.Background(), "p",
		[]string{"c1"}, "reason text", "by-id")
	if err != nil {
		t.Fatalf("Corrector.HardEvict: %v", err)
	}
	if len(audit) != 1 || audit[0].ChunkID != "c1" {
		t.Errorf("audit = %+v", audit)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
