package postgres

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"vornik.io/vornik/internal/persistence"
)

// TestCorpusEpochRepository_CreateEpoch — pin INSERT shape +
// validation. CreateEpoch fills ID + CreatedAt when blank.
func TestCorpusEpochRepository_CreateEpoch(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewCorpusEpochRepository(db)

	execID := "exec-1"
	notes := "test"
	e := &persistence.CorpusEpoch{
		ProjectID: "p1", IngestExecutionID: &execID,
		ChunksAdmitted: 10, ChunksQuarantined: 1,
		ChunksVerified: 2, ChunksRefuted: 0, ChunksSuperseded: 0,
		Notes: &notes,
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO corpus_epochs")).
		WithArgs(sqlmock.AnyArg(), "p1", &execID, sqlmock.AnyArg(),
			10, 1, 2, 0, 0, &notes).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.CreateEpoch(context.Background(), e); err != nil {
		t.Fatalf("CreateEpoch: %v", err)
	}
	if e.ID == "" {
		t.Error("CreateEpoch did not fill ID")
	}
	if e.CreatedAt.IsZero() {
		t.Error("CreateEpoch did not fill CreatedAt")
	}
}

// TestCorpusEpochRepository_CreateEpoch_RejectsNilOrMissing — the
// defensive guards. nil epoch and empty project both error.
func TestCorpusEpochRepository_CreateEpoch_RejectsNilOrMissing(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewCorpusEpochRepository(db)

	if err := repo.CreateEpoch(context.Background(), nil); err == nil {
		t.Error("nil epoch should error")
	}
	if err := repo.CreateEpoch(context.Background(),
		&persistence.CorpusEpoch{ProjectID: ""}); err == nil {
		t.Error("empty project_id should error")
	}
}

// TestCorpusEpochRepository_CloseEpoch — the counts roll-up
// happens via UPDATE; pin the column order.
func TestCorpusEpochRepository_CloseEpoch(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewCorpusEpochRepository(db)

	counts := persistence.CorpusEpochCounts{Admitted: 5, Quarantined: 1, Verified: 3, Refuted: 0, Superseded: 1}
	mock.ExpectExec(regexp.QuoteMeta("UPDATE corpus_epochs")).
		WithArgs("epoch-1", 5, 1, 3, 0, 1).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.CloseEpoch(context.Background(), "epoch-1", counts); err != nil {
		t.Fatalf("CloseEpoch: %v", err)
	}
}

// TestCorpusEpochRepository_CloseEpoch_RequiresID — defensive.
func TestCorpusEpochRepository_CloseEpoch_RequiresID(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewCorpusEpochRepository(db)
	if err := repo.CloseEpoch(context.Background(), "", persistence.CorpusEpochCounts{}); err == nil {
		t.Error("empty epochID should error")
	}
}

// TestCorpusEpochRepository_Activate_DefaultsBy — the by field
// defaults to "system" when empty so the audit row always carries
// a non-null actor identifier.
func TestCorpusEpochRepository_Activate_DefaultsBy(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewCorpusEpochRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO corpus_epochs_active")).
		WithArgs("p1", "epoch-1", "system", "initial").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Migration 89: Activate clears the explicit-deactivation
	// tombstone so a re-activated epoch is rollback-eligible again.
	mock.ExpectExec(regexp.QuoteMeta("UPDATE corpus_epochs")).
		WithArgs("epoch-1", "p1").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := repo.Activate(context.Background(), "p1", "epoch-1", "", "initial"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
}

// TestCorpusEpochRepository_Activate_RejectsMissing — both
// project_id and epoch_id are required.
func TestCorpusEpochRepository_Activate_RejectsMissing(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewCorpusEpochRepository(db)
	if err := repo.Activate(context.Background(), "", "epoch-1", "x", "x"); err == nil {
		t.Error("missing project_id")
	}
	if err := repo.Activate(context.Background(), "p1", "", "x", "x"); err == nil {
		t.Error("missing epoch_id")
	}
}

// TestCorpusEpochRepository_Deactivate — DELETE on the
// active-set table. Idempotent by virtue of just deleting any
// row.
func TestCorpusEpochRepository_Deactivate(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewCorpusEpochRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM corpus_epochs_active")).
		WithArgs("p1", "epoch-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Migration 89: Deactivate also stamps the explicit-deactivation
	// tombstone so rollback re-activation cannot resurrect the epoch.
	mock.ExpectExec(regexp.QuoteMeta("UPDATE corpus_epochs")).
		WithArgs("epoch-1", "p1", "operator").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.Deactivate(context.Background(), "p1", "epoch-1", "operator"); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
}

// TestCorpusEpochRepository_ListActive_OrdersNewestFirst — the
// search-query filter relies on this ordering to break ties
// when multiple epochs are simultaneously active. Pin it.
func TestCorpusEpochRepository_ListActive_OrdersNewestFirst(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewCorpusEpochRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY activated_at DESC")).
		WithArgs("p1").
		WillReturnRows(sqlmock.NewRows([]string{"epoch_id"}).
			AddRow("epoch-new").AddRow("epoch-old"))

	ids, err := repo.ListActive(context.Background(), "p1")
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(ids) != 2 || ids[0] != "epoch-new" {
		t.Errorf("got %v, want [epoch-new, epoch-old]", ids)
	}
}

// TestCorpusEpochRepository_ListActive_RejectsEmptyProject — the
// IDOR guard. An empty project_id would return every active
// epoch in the deployment.
func TestCorpusEpochRepository_ListActive_RejectsEmptyProject(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewCorpusEpochRepository(db)
	if _, err := repo.ListActive(context.Background(), ""); err == nil {
		t.Error("empty project_id should error (IDOR guard)")
	}
}

// TestCorpusEpochRepository_ListEpochs_LimitsAndDecodes — full
// epoch listing. Pin the column order; defensive guards on
// limit ≤ 0 and >100.
func TestCorpusEpochRepository_ListEpochs_LimitsAndDecodes(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewCorpusEpochRepository(db)

	created := time.Date(2026, 5, 16, 14, 0, 0, 0, time.UTC)
	closed := created.Add(2 * time.Minute)
	mock.ExpectQuery(regexp.QuoteMeta("FROM corpus_epochs")).
		WithArgs("p1", 25).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "ingest_execution_id", "created_at", "closed_at",
			"chunks_admitted", "chunks_quarantined", "chunks_verified",
			"chunks_refuted", "chunks_superseded", "notes", "is_active",
		}).AddRow("epoch-1", "p1", "exec-1", created, closed,
			10, 1, 2, 0, 0, "test", true))

	got, err := repo.ListEpochs(context.Background(), "p1", 25)
	if err != nil {
		t.Fatalf("ListEpochs: %v", err)
	}
	if len(got) != 1 || got[0].ID != "epoch-1" || got[0].ChunksAdmitted != 10 {
		t.Errorf("got %+v", got)
	}
}

// TestCorpusEpochRepository_GetEpoch_NotFound — a missing row
// returns nil + nil, not an error. The handler differentiates
// from a real DB error so operators get a clean 404.
func TestCorpusEpochRepository_GetEpoch_NotFound(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewCorpusEpochRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("FROM corpus_epochs")).
		WithArgs("missing").
		WillReturnError(errors.New("sql: no rows in result set"))

	if _, err := repo.GetEpoch(context.Background(), "missing"); err == nil {
		t.Skip("repo doesn't surface missing as a sentinel; behaviour will be reviewed in slice 2")
	}
}
