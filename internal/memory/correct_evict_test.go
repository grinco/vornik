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
	// The run header is written FIRST — the tombstones foreign-key to it.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO memory_eviction_runs")).
		WillReturnResult(sqlmock.NewResult(0, 1))
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
	// Capture what the chunks derived BEFORE deleting them: entity_mentions
	// cascades with the chunk, so afterwards the link is gone.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT DISTINCT em.entity_id")).
		WillReturnRows(sqlmock.NewRows([]string{"entity_id"}).AddRow("ent_1"))
	// The cached embedding of the evicted text is derived data too, and its key
	// is unreadable once the chunk row is gone — so it is collected first.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT embed_input_hash")).
		WillReturnRows(sqlmock.NewRows([]string{
			"embed_input_hash", "source_name", "content", "content_hash",
		}).AddRow("eh1", "src1", "body", "hash1"))
	// The quarantined pre-ingest copy goes BEFORE the chunk delete, which would
	// otherwise SET NULL the only pointer to it.
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM project_memory_quarantine")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// DELETE the chunks. FK CASCADE handles memory_embed_queue +
	// memory_embed_dlq + entity_mentions automatically — those
	// rows aren't issued by HardEvict.
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM project_memory_chunks")).
		WithArgs("janka", "chunk_1", "chunk_2").
		WillReturnResult(sqlmock.NewResult(0, 2))
	// One cache delete per distinct key the helper derived.
	for i := 0; i < 3; i++ {
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM embedding_cache")).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	// Then the derived sweep, in the SAME transaction: lock, prune
	// source_chunks, drop evidence-less edges, count the cascade, delete
	// entities no surviving chunk reaches.
	mock.ExpectExec(regexp.QuoteMeta("FOR UPDATE")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE knowledge_edges")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM knowledge_edges")).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT count(*) FROM knowledge_edges")).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM knowledge_entities")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// The outcome counts land on the run header, in the SAME transaction as
	// the deletes.
	mock.ExpectExec(regexp.QuoteMeta("UPDATE memory_eviction_runs")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	res, err := repo.HardEvict(context.Background(), "janka",
		[]string{"chunk_1", "chunk_2"}, "GDPR DSAR 12", "operator-jane")
	if err != nil {
		t.Fatalf("HardEvict: %v", err)
	}
	audit := res.Audit
	if len(audit) != 2 {
		t.Fatalf("audit rows = %d, want 2", len(audit))
	}
	// The derived rows are the part an eviction used to leave behind.
	if res.Derived.Entities != 1 || res.Derived.Edges != 3 {
		t.Errorf("derived counts = %+v, want 1 entity and 3 edges (2 emptied + 1 cascaded)",
			res.Derived)
	}
	if res.EmbeddingCacheKeysDeleted != 3 {
		t.Errorf("EmbeddingCacheKeysDeleted = %d, want 3 — the recorded key, the "+
			"recomputed one for pre-migration rows, and the content hash",
			res.EmbeddingCacheKeysDeleted)
	}
	if res.QuarantinedCopiesDeleted != 1 {
		t.Errorf("QuarantinedCopiesDeleted = %d, want 1 — the FK is SET NULL, so an "+
			"eviction that stops at the chunk keeps the rejected text",
			res.QuarantinedCopiesDeleted)
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
	// The run header is written even when nothing matches, and KEPT: it records
	// that an operator asked to erase these ids and that none of them existed.
	// An attempt that removed nothing is still an operator action on personal
	// data, and the row shows chunks_requested > 0 with every outcome count
	// zero — which is the truthful shape.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO memory_eviction_runs")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("FROM project_memory_chunks")).
		WithArgs("janka", "ghost_1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "content_hash", "source_name", "content_class", "producer_role",
		}))
	mock.ExpectCommit()

	res, err := repo.HardEvict(context.Background(), "janka",
		[]string{"ghost_1"}, "stale id check", "tester")
	if err != nil {
		t.Fatalf("HardEvict: %v", err)
	}
	if res.Count() != 0 {
		t.Errorf("expected no audit rows for ghost IDs, got %+v", res.Audit)
	}
	// No chunks evicted means nothing derived from them: the sweep must not run
	// on an empty set, or a keyed delete becomes a project-wide orphan hunt.
	if res.Derived.Total() != 0 || res.QuarantinedCopiesDeleted != 0 {
		t.Errorf("nothing may be swept when nothing was evicted: %+v", res)
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

	res, err := repo.HardEvict(context.Background(), "janka", nil, "r", "by")
	if err != nil || res != nil {
		t.Errorf("nil chunks: res=%v err=%v, want (nil,nil)", res, err)
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
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO memory_eviction_runs")).
		WillReturnResult(sqlmock.NewResult(0, 1))
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
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO memory_eviction_runs")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("FROM project_memory_chunks")).
		WithArgs("janka", "chunk_1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "content_hash", "source_name", "content_class", "producer_role",
		}).AddRow("chunk_1", "h", "s", "c", "r"))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO memory_eviction_audit")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT DISTINCT em.entity_id")).
		WillReturnRows(sqlmock.NewRows([]string{"entity_id"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT embed_input_hash")).
		WillReturnRows(sqlmock.NewRows([]string{
			"embed_input_hash", "source_name", "content", "content_hash",
		}))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM project_memory_quarantine")).
		WillReturnResult(sqlmock.NewResult(0, 0))
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

	res, err := corr.HardEvict(context.Background(), "p", nil, "r", "by")
	if err != nil {
		t.Errorf("nil chunks: err=%v, want nil", err)
	}
	if res.Count() != 0 || res.Derived.Total() != 0 || res.QuarantinedCopiesDeleted != 0 {
		t.Errorf("nil chunks must evict and sweep nothing, got %+v", res)
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
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO memory_eviction_runs")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("FROM project_memory_chunks")).
		WithArgs("p", "c1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "content_hash", "source_name", "content_class", "producer_role",
		}).AddRow("c1", "h", "s", "c", "r"))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO memory_eviction_audit")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT DISTINCT em.entity_id")).
		WillReturnRows(sqlmock.NewRows([]string{"entity_id"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT embed_input_hash")).
		WillReturnRows(sqlmock.NewRows([]string{
			"embed_input_hash", "source_name", "content", "content_hash",
		}))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM project_memory_quarantine")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM project_memory_chunks")).
		WithArgs("p", "c1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// The chunk mentioned no entity, so the sweep prunes source_chunks and
	// stops — there is no candidate set to lock or evaluate.
	mock.ExpectExec(regexp.QuoteMeta("UPDATE knowledge_edges")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE memory_eviction_runs")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	res, err := corr.HardEvict(context.Background(), "p",
		[]string{"c1"}, "reason text", "by-id")
	if err != nil {
		t.Fatalf("Corrector.HardEvict: %v", err)
	}
	if res.Count() != 1 || res.Audit[0].ChunkID != "c1" {
		t.Errorf("audit = %+v", res.Audit)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
