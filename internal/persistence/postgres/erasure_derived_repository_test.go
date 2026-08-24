package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"vornik.io/vornik/internal/erasure"
)

// The hard deletes in this file are the only legitimate knowledge-graph
// deletions in the system, and the design chose a REQUIRED ARGUMENT over a
// distinctive name to keep them that way (§4.14, review F5). These tests pin
// that: they need no database, because refusing must happen before a
// connection is ever touched.

func TestErasureDerived_refusesHardDeleteWithoutARequestID(t *testing.T) {
	// A nil *sql.DB is deliberate. If the gate ever moves after the query, this
	// panics instead of passing quietly.
	r := NewErasureDerivedRepository(nil)

	_, err := r.DeleteOrphanedDerived(context.Background(), "  ",
		erasure.Derivation{EntityIDs: []string{"ent-1"}, ChunkIDs: []string{"chunk-1"}})
	if err == nil {
		t.Fatal("deleting graph rows without an erasure request must be refused")
	}
	if !strings.Contains(err.Error(), "erasure request id") {
		t.Errorf("the refusal must name what is missing: %v", err)
	}

	if _, err := r.DeleteQuarantinedForArtifact(context.Background(), "", "artifact-1"); err == nil {
		t.Fatal("purging quarantined content without an erasure request must be refused")
	}
}

// An empty derivation must not fall through to a broader predicate. This is the
// difference between an erasure and the global orphan hunt §5.5 deliberately
// makes a separate, audited operator action.
func TestErasureDerived_emptyDerivationIsANoOp(t *testing.T) {
	r := NewErasureDerivedRepository(nil) // nil DB: a query here would panic

	counts, err := r.DeleteOrphanedDerived(context.Background(), "req-1", erasure.Derivation{})
	if err != nil {
		t.Fatalf("an empty derivation is not an error: %v", err)
	}
	if counts.Total() != 0 {
		t.Errorf("nothing may be deleted for an empty derivation: %+v", counts)
	}
}

func TestErasureDerived_captureRequiresAnArtifact(t *testing.T) {
	r := NewErasureDerivedRepository(nil)

	if _, err := r.CaptureDerivation(context.Background(), "   ", nil); err == nil {
		t.Fatal("an empty artifact id must be refused, not turned into a query matching nothing")
	}
}

// A failed erasure must not read as a completed one. These pin that every
// database failure on this path SURFACES — a partially-erased subject reported
// as erased is the defect the whole change exists to fix, one level along.

func newMockDerivedRepo(t *testing.T) (*ErasureDerivedRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewErasureDerivedRepository(db), mock
}

func TestErasureDerived_captureFailureSurfaces(t *testing.T) {
	repo, mock := newMockDerivedRepo(t)
	mock.ExpectQuery("SELECT id FROM project_memory_chunks").WillReturnError(errors.New("connection reset"))

	_, err := repo.CaptureDerivation(context.Background(), "artifact-1", nil)
	if err == nil {
		t.Fatal("a failed capture must surface: erasing chunks after it would " +
			"destroy the only record of what they derived")
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("the cause must survive: %v", err)
	}
}

func TestErasureDerived_sweepRollsBackOnFailure(t *testing.T) {
	repo, mock := newMockDerivedRepo(t)
	mock.ExpectBegin()
	mock.ExpectExec("FOR UPDATE").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE knowledge_edges").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM knowledge_edges").WillReturnError(errors.New("deadlock detected"))
	mock.ExpectRollback()

	_, err := repo.DeleteOrphanedDerived(context.Background(), "req-1",
		erasure.Derivation{EntityIDs: []string{"ent-1"}, ChunkIDs: []string{"chunk-1"}})
	if err == nil {
		t.Fatal("a partial sweep must roll back and surface, not report success")
	}
	if !strings.Contains(err.Error(), "deadlock detected") {
		t.Errorf("the cause must survive: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expected a rollback: %v", err)
	}
}

// A commit failure means NOTHING was deleted. Reporting the counts the
// statements would have produced would be a false claim about personal data.
func TestErasureDerived_commitFailureReportsNoCounts(t *testing.T) {
	repo, mock := newMockDerivedRepo(t)
	mock.ExpectBegin()
	mock.ExpectExec("FOR UPDATE").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE knowledge_edges").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM knowledge_edges").WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectQuery("SELECT count").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectExec("DELETE FROM knowledge_entities").WillReturnResult(sqlmock.NewResult(0, 4))
	mock.ExpectCommit().WillReturnError(errors.New("server closed the connection"))

	counts, err := repo.DeleteOrphanedDerived(context.Background(), "req-1",
		erasure.Derivation{EntityIDs: []string{"ent-1"}, ChunkIDs: []string{"chunk-1"}})
	if err == nil {
		t.Fatal("a failed commit must surface")
	}
	if counts.Total() != 0 {
		t.Errorf("a rolled-back sweep deleted nothing and must report nothing: %+v", counts)
	}
}

func TestErasureDerived_quarantinePurgeFailureSurfaces(t *testing.T) {
	repo, mock := newMockDerivedRepo(t)
	mock.ExpectExec("DELETE FROM project_memory_quarantine").
		WillReturnError(errors.New("permission denied"))

	if _, err := repo.DeleteQuarantinedForArtifact(context.Background(), "req-1", "artifact-1"); err == nil {
		t.Fatal("a failed quarantine purge must surface — that row holds the subject's text")
	}
}

// The backfill's preview and its delete both fail loudly. An operator deciding
// whether to delete personal data must not be shown a count produced by a
// half-failed query.
func TestErasureDerived_backfillFailuresSurface(t *testing.T) {
	repo, mock := newMockDerivedRepo(t)
	mock.ExpectQuery("SELECT ke.type").WillReturnError(errors.New("relation does not exist"))
	if _, err := repo.CountOrphanedEntities(context.Background(), "proj"); err == nil {
		t.Fatal("a failed count must surface, not report zero orphans")
	}

	repo2, mock2 := newMockDerivedRepo(t)
	mock2.ExpectBegin()
	mock2.ExpectQuery("FOR UPDATE SKIP LOCKED").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ent-1"))
	mock2.ExpectQuery("SELECT count").WillReturnError(errors.New("statement timeout"))
	mock2.ExpectRollback()
	if _, _, err := repo2.PruneOrphanedEntities(context.Background(), "proj"); err == nil {
		t.Fatal("a failed prune must surface")
	}
	if err := mock2.ExpectationsWereMet(); err != nil {
		t.Errorf("expected a rollback: %v", err)
	}
}

// The remaining ways this can fail, each of which would otherwise let a caller
// believe derived personal data was removed when it was not.
func TestErasureDerived_everyFailurePointRefusesToClaimSuccess(t *testing.T) {
	ctx := context.Background()
	d := erasure.Derivation{EntityIDs: []string{"ent-1"}, ChunkIDs: []string{"chunk-1"}}

	t.Run("sweep cannot begin", func(t *testing.T) {
		repo, mock := newMockDerivedRepo(t)
		mock.ExpectBegin().WillReturnError(errors.New("too many connections"))
		if _, err := repo.DeleteOrphanedDerived(ctx, "req-1", d); err == nil {
			t.Fatal("an unopened transaction deleted nothing and must say so")
		}
	})

	t.Run("pruning source_chunks fails", func(t *testing.T) {
		repo, mock := newMockDerivedRepo(t)
		mock.ExpectBegin()
		mock.ExpectExec("FOR UPDATE").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec("UPDATE knowledge_edges").WillReturnError(errors.New("lock timeout"))
		mock.ExpectRollback()
		if _, err := repo.DeleteOrphanedDerived(ctx, "req-1", d); err == nil {
			t.Fatal("edges still citing an erased chunk is a failed erasure")
		}
	})

	t.Run("counting cascading edges fails", func(t *testing.T) {
		repo, mock := newMockDerivedRepo(t)
		mock.ExpectBegin()
		mock.ExpectExec("FOR UPDATE").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec("UPDATE knowledge_edges").WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("DELETE FROM knowledge_edges").WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery("SELECT count").WillReturnError(errors.New("statement timeout"))
		mock.ExpectRollback()
		if _, err := repo.DeleteOrphanedDerived(ctx, "req-1", d); err == nil {
			t.Fatal("a count the report depends on cannot fail silently")
		}
	})

	t.Run("deleting entities fails", func(t *testing.T) {
		repo, mock := newMockDerivedRepo(t)
		mock.ExpectBegin()
		mock.ExpectExec("FOR UPDATE").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec("UPDATE knowledge_edges").WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("DELETE FROM knowledge_edges").WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery("SELECT count").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
		mock.ExpectExec("DELETE FROM knowledge_entities").WillReturnError(errors.New("deadlock"))
		mock.ExpectRollback()
		if _, err := repo.DeleteOrphanedDerived(ctx, "req-1", d); err == nil {
			t.Fatal("entities surviving their erasure must be reported, not swallowed")
		}
	})

	t.Run("quarantine purge needs an artifact", func(t *testing.T) {
		repo := NewErasureDerivedRepository(nil) // nil DB: a query here would panic
		if _, err := repo.DeleteQuarantinedForArtifact(ctx, "req-1", "  "); err == nil {
			t.Fatal("an empty artifact id must be refused, not turned into a wider delete")
		}
	})

	t.Run("backfill commit fails", func(t *testing.T) {
		repo, mock := newMockDerivedRepo(t)
		mock.ExpectBegin()
		mock.ExpectQuery("FOR UPDATE SKIP LOCKED").
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ent-1"))
		mock.ExpectQuery("SELECT count").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(4))
		mock.ExpectExec("DELETE FROM knowledge_entities").WillReturnResult(sqlmock.NewResult(0, 9))
		mock.ExpectCommit().WillReturnError(errors.New("server closed the connection"))
		entities, edges, err := repo.PruneOrphanedEntities(ctx, "proj")
		if err == nil {
			t.Fatal("a rolled-back prune must surface")
		}
		if entities != 0 || edges != 0 {
			t.Errorf("a rolled-back prune deleted nothing: %d entities, %d edges", entities, edges)
		}
	})

	t.Run("backfill delete fails", func(t *testing.T) {
		repo, mock := newMockDerivedRepo(t)
		mock.ExpectBegin()
		mock.ExpectQuery("FOR UPDATE SKIP LOCKED").
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ent-1"))
		mock.ExpectQuery("SELECT count").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
		mock.ExpectExec("DELETE FROM knowledge_entities").WillReturnError(errors.New("permission denied"))
		mock.ExpectRollback()
		if _, _, err := repo.PruneOrphanedEntities(ctx, "proj"); err == nil {
			t.Fatal("a failed prune must surface")
		}
	})

	// A driver that accepts the statement and then cannot say how many rows it
	// touched leaves the count unknown. Reporting a number here would be a
	// guess about deleted personal data.
	t.Run("an unknowable row count is an error, not a zero", func(t *testing.T) {
		repo, mock := newMockDerivedRepo(t)
		mock.ExpectExec("DELETE FROM project_memory_quarantine").
			WillReturnResult(sqlmock.NewErrorResult(errors.New("no RowsAffected available")))
		if _, err := repo.DeleteQuarantinedForArtifact(ctx, "req-1", "artifact-1"); err == nil {
			t.Fatal("an unknown count must surface rather than be reported as zero purged")
		}
	})

	t.Run("a malformed count row surfaces", func(t *testing.T) {
		repo, mock := newMockDerivedRepo(t)
		mock.ExpectQuery("SELECT ke.type").WillReturnRows(
			sqlmock.NewRows([]string{"type", "count", "with_embedding"}).
				AddRow("PERSON", "not-a-number", 3))
		if _, err := repo.CountOrphanedEntities(ctx, "proj"); err == nil {
			t.Fatal("an unreadable preview must not be shown to the operator as a count")
		}
	})
}

// The lock is not optional and not an optimisation. Without it the NOT EXISTS
// re-check is evaluated against a snapshot that can predate a committed ingest,
// and the entity is deleted with the fresh mention cascaded away — reproduced
// against Postgres 2026-08-21. This pins that both sweeps take it BEFORE
// reading any mention.
func TestErasureDerived_locksCandidatesBeforeReadingMentions(t *testing.T) {
	t.Run("erasure sweep", func(t *testing.T) {
		repo, mock := newMockDerivedRepo(t)
		mock.ExpectBegin()
		mock.ExpectExec("SELECT id FROM knowledge_entities WHERE id = ANY").
			WillReturnError(errors.New("lock not taken"))
		mock.ExpectRollback()
		if _, err := repo.DeleteOrphanedDerived(context.Background(), "req-1",
			erasure.Derivation{EntityIDs: []string{"ent-1"}, ChunkIDs: []string{"c1"}}); err == nil {
			t.Fatal("the sweep must take the row lock first and fail if it cannot")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("the lock must be the FIRST statement in the transaction: %v", err)
		}
	})

	t.Run("backfill", func(t *testing.T) {
		repo, mock := newMockDerivedRepo(t)
		mock.ExpectBegin()
		mock.ExpectQuery("FOR UPDATE SKIP LOCKED").WillReturnError(errors.New("lock not taken"))
		mock.ExpectRollback()
		if _, _, err := repo.PruneOrphanedEntities(context.Background(), "proj"); err == nil {
			t.Fatal("the backfill must lock its candidates before deciding their fate")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("the lock must precede the counting query: %v", err)
		}
	})
}

// The backfill deletes in batches, and reports what it actually deleted even
// when a later batch fails.
//
// The lock this takes is the same FOR UPDATE that blocks an ingest from
// recording a mention. The erasure sweep locks one document's entities; this
// locks every stranded row in the project — 3,274 on production — so holding
// them in one transaction would turn a hygiene run into an ingestion stall.
// Partial counts must survive a failure because the admin_audit row has to
// record what happened, not what was attempted.
func TestErasureDerived_backfillBatchesAndReportsPartialProgress(t *testing.T) {
	repo, mock := newMockDerivedRepo(t)

	// Batch one succeeds.
	mock.ExpectBegin()
	mock.ExpectQuery("FOR UPDATE SKIP LOCKED").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ent-1").AddRow("ent-2"))
	mock.ExpectQuery("SELECT count").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
	mock.ExpectExec("DELETE FROM knowledge_entities").WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()
	// Batch two fails.
	mock.ExpectBegin()
	mock.ExpectQuery("FOR UPDATE SKIP LOCKED").WillReturnError(errors.New("connection reset"))
	mock.ExpectRollback()

	entities, edges, err := repo.PruneOrphanedEntities(context.Background(), "proj")
	if err == nil {
		t.Fatal("a failed batch must surface")
	}
	if entities != 2 || edges != 3 {
		t.Errorf("partial progress must be reported for the audit row, got %d entities / %d edges",
			entities, edges)
	}
}

// A batch whose every candidate gained evidence between the lock and the delete
// deletes nothing — and must NOT end the run, because other rows are still
// stranded. The loop keys on candidates FOUND, not on rows removed.
func TestErasureDerived_backfillKeepsGoingWhenABatchDeletesNothing(t *testing.T) {
	repo, mock := newMockDerivedRepo(t)

	// Batch one: two candidates locked, both saved by a concurrent mention.
	mock.ExpectBegin()
	mock.ExpectQuery("FOR UPDATE SKIP LOCKED").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ent-1").AddRow("ent-2"))
	mock.ExpectQuery("SELECT count").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec("DELETE FROM knowledge_entities").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	// Batch two: a genuinely stranded row, which a stop-on-zero loop would miss.
	mock.ExpectBegin()
	mock.ExpectQuery("FOR UPDATE SKIP LOCKED").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ent-3"))
	mock.ExpectQuery("SELECT count").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectExec("DELETE FROM knowledge_entities").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	// Batch three: nothing left.
	mock.ExpectBegin()
	mock.ExpectQuery("FOR UPDATE SKIP LOCKED").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectRollback()

	entities, edges, err := repo.PruneOrphanedEntities(context.Background(), "proj")
	if err != nil {
		t.Fatalf("PruneOrphanedEntities: %v", err)
	}
	if entities != 1 || edges != 1 {
		t.Errorf("got %d entities / %d edges; a batch that deletes nothing must not end the run",
			entities, edges)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the run stopped early: %v", err)
	}
}
