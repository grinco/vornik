package graphsweep

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// This package exists because three separate paths delete memory chunks and
// each has to sweep what those chunks derived. Two of them were written
// independently and the keep rule was got WRONG in one — see StillEvidenced.
// These tests pin the properties that must not diverge again.

// newMock returns a mock plus an open transaction. Sweep requires a *sql.Tx by
// signature — that requirement is the point, so the tests honour it rather than
// reaching for a looser handle.
func newMock(t *testing.T) (*sqlmock.Sqlmock, *sql.Tx) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectBegin()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	return &mock, tx
}

// An empty input must never widen into a project-wide predicate. That is the
// difference between "remove what this deletion derived" and an orphan hunt.
func TestSweep_emptyInputIsANoOp(t *testing.T) {
	_, tx := newMock(t) // no expectations: any SQL at all fails the test

	counts, err := Sweep(context.Background(), tx, nil, nil)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if counts.Total() != 0 {
		t.Errorf("nothing may be swept for an empty input: %+v", counts)
	}
}

func TestCaptureEntities_emptyInputIssuesNoQuery(t *testing.T) {
	_, tx := newMock(t)

	got, err := CaptureEntities(context.Background(), tx, nil)
	if err != nil || got != nil {
		t.Errorf("CaptureEntities(nil) = %v, %v; want nil, nil", got, err)
	}
}

func TestDeleteQuarantinedForChunks_emptyInputIssuesNoQuery(t *testing.T) {
	_, tx := newMock(t)

	n, err := DeleteQuarantinedForChunks(context.Background(), tx, nil)
	if err != nil || n != 0 {
		t.Errorf("DeleteQuarantinedForChunks(nil) = %d, %v; want 0, nil", n, err)
	}
}

// The lock must come FIRST. Under READ COMMITTED the NOT EXISTS re-check alone
// lets a concurrently-committed mention be missed, and the entity is then
// deleted with that mention cascaded away — measured against Postgres
// 2026-08-21. Any statement that reads mentions before the lock reopens it.
func TestSweep_locksBeforeReadingAnything(t *testing.T) {
	mp, tx := newMock(t)
	mock := *mp
	mock.ExpectExec("SELECT id FROM knowledge_entities WHERE id = ANY").
		WillReturnError(errors.New("lock not taken"))

	if _, err := Sweep(context.Background(), tx, []string{"c1"}, []string{"e1"}); err == nil {
		t.Fatal("the sweep must take the row lock first and fail if it cannot")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the lock must be the first statement: %v", err)
	}
}

// Chunks but no entities: the edges that cited them still have to lose those
// ids, or they keep pointing at chunks that no longer exist. Nothing else runs,
// because there is no candidate set to decide about.
func TestSweep_prunesSourceChunksEvenWithNoEntities(t *testing.T) {
	mp, tx := newMock(t)
	mock := *mp
	mock.ExpectExec("UPDATE knowledge_edges").WillReturnResult(sqlmock.NewResult(0, 2))

	counts, err := Sweep(context.Background(), tx, []string{"c1"}, nil)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if counts.Total() != 0 {
		t.Errorf("no captured entities means no deletions: %+v", counts)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the source_chunks prune must still run: %v", err)
	}
}

// Edges removed directly and edges taken by the FK cascade are equally gone,
// and the caller's report is the only place either will be recorded.
func TestSweep_reportsDirectAndCascadedEdgesTogether(t *testing.T) {
	mp, tx := newMock(t)
	mock := *mp
	mock.ExpectExec("FOR UPDATE").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE knowledge_edges").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM knowledge_edges").WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectQuery("SELECT count").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
	mock.ExpectExec("DELETE FROM knowledge_entities").WillReturnResult(sqlmock.NewResult(0, 4))

	counts, err := Sweep(context.Background(), tx, []string{"c1"}, []string{"e1"})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if counts.Edges != 5 {
		t.Errorf("Edges = %d, want 2 deleted directly + 3 cascaded", counts.Edges)
	}
	if counts.Entities != 4 {
		t.Errorf("Entities = %d, want 4", counts.Entities)
	}
	if counts.Total() != 9 {
		t.Errorf("Total() = %d, want 9", counts.Total())
	}
}

// The keep rule accepts BOTH routes. A predicate that only checked mentions
// would have deleted 522 live rows in production, because the graph pipeline
// writes a mention only when the extracted candidate has a valid span.
func TestStillEvidenced_acceptsMentionsAndLiveEdges(t *testing.T) {
	if !strings.Contains(StillEvidenced, "entity_mentions") {
		t.Error("the mention route is missing")
	}
	if !strings.Contains(StillEvidenced, "knowledge_edges") {
		t.Error("the edge route is missing — a mention-only rule deletes live entities")
	}
	if !strings.Contains(StillEvidenced, "project_memory_chunks") {
		t.Error("an edge only counts as evidence when the chunk it cites still EXISTS; " +
			"without this check a dangling edge would keep a stranded entity alive")
	}
}

// Every failure must surface. A partial sweep reported as success is the defect
// this package exists to fix, one level along.
func TestSweep_everyFailurePointSurfaces(t *testing.T) {
	ctx := context.Background()
	chunks, entities := []string{"c1"}, []string{"e1"}

	cases := []struct {
		name  string
		setup func(sqlmock.Sqlmock)
	}{
		{"prune source chunks", func(m sqlmock.Sqlmock) {
			m.ExpectExec("FOR UPDATE").WillReturnResult(sqlmock.NewResult(0, 1))
			m.ExpectExec("UPDATE knowledge_edges").WillReturnError(errors.New("lock timeout"))
		}},
		{"delete evidence-less edges", func(m sqlmock.Sqlmock) {
			m.ExpectExec("FOR UPDATE").WillReturnResult(sqlmock.NewResult(0, 1))
			m.ExpectExec("UPDATE knowledge_edges").WillReturnResult(sqlmock.NewResult(0, 0))
			m.ExpectExec("DELETE FROM knowledge_edges").WillReturnError(errors.New("deadlock"))
		}},
		{"count cascading edges", func(m sqlmock.Sqlmock) {
			m.ExpectExec("FOR UPDATE").WillReturnResult(sqlmock.NewResult(0, 1))
			m.ExpectExec("UPDATE knowledge_edges").WillReturnResult(sqlmock.NewResult(0, 0))
			m.ExpectExec("DELETE FROM knowledge_edges").WillReturnResult(sqlmock.NewResult(0, 0))
			m.ExpectQuery("SELECT count").WillReturnError(errors.New("statement timeout"))
		}},
		{"delete entities", func(m sqlmock.Sqlmock) {
			m.ExpectExec("FOR UPDATE").WillReturnResult(sqlmock.NewResult(0, 1))
			m.ExpectExec("UPDATE knowledge_edges").WillReturnResult(sqlmock.NewResult(0, 0))
			m.ExpectExec("DELETE FROM knowledge_edges").WillReturnResult(sqlmock.NewResult(0, 0))
			m.ExpectQuery("SELECT count").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
			m.ExpectExec("DELETE FROM knowledge_entities").WillReturnError(errors.New("permission denied"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mp, tx := newMock(t)
			tc.setup(*mp)
			if _, err := Sweep(ctx, tx, chunks, entities); err == nil {
				t.Fatalf("a failure in %q must surface", tc.name)
			}
		})
	}
}

func TestCaptureEntities_failureSurfaces(t *testing.T) {
	mp, tx := newMock(t)
	mock := *mp
	mock.ExpectQuery("SELECT DISTINCT em.entity_id").WillReturnError(errors.New("connection reset"))

	if _, err := CaptureEntities(context.Background(), tx, []string{"c1"}); err == nil {
		t.Fatal("a failed capture must surface: deleting the chunks afterwards would " +
			"destroy the only record of what they derived")
	}
}

func TestCaptureEntities_scanFailureSurfaces(t *testing.T) {
	mp, tx := newMock(t)
	mock := *mp
	mock.ExpectQuery("SELECT DISTINCT em.entity_id").
		WillReturnRows(sqlmock.NewRows([]string{"entity_id"}).AddRow(nil))

	if _, err := CaptureEntities(context.Background(), tx, []string{"c1"}); err == nil {
		t.Fatal("an unreadable capture must not pass as an empty one")
	}
}

func TestDeleteQuarantinedForChunks_failureSurfaces(t *testing.T) {
	mp, tx := newMock(t)
	mock := *mp
	mock.ExpectExec("DELETE FROM project_memory_quarantine").
		WillReturnError(errors.New("permission denied"))

	if _, err := DeleteQuarantinedForChunks(context.Background(), tx, []string{"c1"}); err == nil {
		t.Fatal("a failed purge must surface — that row holds the chunk's full text")
	}
}

// A driver that runs the statement but cannot report rows affected leaves the
// count unknown; reporting a number would be a guess about deleted data.
func TestDeleteQuarantinedForChunks_unknowableCountIsAnError(t *testing.T) {
	mp, tx := newMock(t)
	mock := *mp
	mock.ExpectExec("DELETE FROM project_memory_quarantine").
		WillReturnResult(sqlmock.NewErrorResult(errors.New("no RowsAffected available")))

	if _, err := DeleteQuarantinedForChunks(context.Background(), tx, []string{"c1"}); err == nil {
		t.Fatal("an unknown count must surface rather than be reported as zero purged")
	}
}

// Quarantine is the RETENTION ending of the same cascade Sweep performs. The
// two must not converge: erasure deletes, retention parks.
func TestQuarantine_emptyChunkSetIsANoOp(t *testing.T) {
	_, tx := newMock(t) // no expectations: any SQL at all fails the test

	counts, err := Quarantine(context.Background(), tx, nil, []string{"e1"})
	if err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if counts.Total() != 0 {
		t.Errorf("nothing may be parked for an empty chunk set: %+v", counts)
	}
}

// Chunks but no captured entities: the edges that cited them still have to lose
// those ids, or they keep pointing at chunks that no longer exist. Nothing is
// parked, because there is no candidate set to decide about.
func TestQuarantine_prunesSourceChunksEvenWithNoEntities(t *testing.T) {
	mp, tx := newMock(t)
	mock := *mp
	mock.ExpectExec("UPDATE knowledge_edges").WillReturnResult(sqlmock.NewResult(0, 2))

	counts, err := Quarantine(context.Background(), tx, []string{"c1"}, nil)
	if err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if counts.Total() != 0 {
		t.Errorf("no captured entities means nothing parked: %+v", counts)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the source_chunks prune must still run: %v", err)
	}
}

// It PARKS. If this ever starts issuing DELETEs it has become the erasure
// sweep, and retention would be destroying rows a subject never asked about.
func TestQuarantine_neverDeletes(t *testing.T) {
	mp, tx := newMock(t)
	mock := *mp
	mock.ExpectExec("UPDATE knowledge_edges").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("SET lifecycle_state = 'quarantined'").WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectExec("UPDATE knowledge_entities").WillReturnResult(sqlmock.NewResult(0, 2))

	counts, err := Quarantine(context.Background(), tx, []string{"c1"}, []string{"e1"})
	if err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if counts.Edges != 3 || counts.Entities != 2 {
		t.Errorf("counts = %+v, want 3 edges and 2 entities parked", counts)
	}
	// ExpectationsWereMet fails if a DELETE was issued, since none is expected.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("retention must issue no DELETE against the graph: %v", err)
	}
}

// Every failure surfaces: a half-settled graph reported as settled is the
// condition this fixes, one level along.
func TestQuarantine_failuresSurface(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		setup func(sqlmock.Sqlmock)
	}{
		{"prune source chunks", func(m sqlmock.Sqlmock) {
			m.ExpectExec("UPDATE knowledge_edges").WillReturnError(errors.New("lock timeout"))
		}},
		{"park edges", func(m sqlmock.Sqlmock) {
			m.ExpectExec("UPDATE knowledge_edges").WillReturnResult(sqlmock.NewResult(0, 0))
			m.ExpectExec("SET lifecycle_state").WillReturnError(errors.New("deadlock"))
		}},
		{"park entities", func(m sqlmock.Sqlmock) {
			m.ExpectExec("UPDATE knowledge_edges").WillReturnResult(sqlmock.NewResult(0, 0))
			m.ExpectExec("SET lifecycle_state").WillReturnResult(sqlmock.NewResult(0, 0))
			m.ExpectExec("UPDATE knowledge_entities").WillReturnError(errors.New("permission denied"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mp, tx := newMock(t)
			tc.setup(*mp)
			if _, err := Quarantine(ctx, tx, []string{"c1"}, []string{"e1"}); err == nil {
				t.Fatalf("a failure in %q must surface", tc.name)
			}
		})
	}
}

func TestQuarantineCounts_Total(t *testing.T) {
	if got := (QuarantineCounts{Edges: 3, Entities: 4}).Total(); got != 7 {
		t.Errorf("Total() = %d, want 7", got)
	}
	if (QuarantineCounts{}).Total() != 0 {
		t.Error("an empty result totals zero")
	}
}
