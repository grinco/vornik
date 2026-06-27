package postgres

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestLiveEvent_Append_AllocatesSeq: the headline contract —
// Append computes the next per-execution seq via the embedded
// SELECT MAX+1 and returns it.
func TestLiveEvent_Append_AllocatesSeq(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewExecutionLiveEventRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO execution_live_events")).
		WithArgs("exec-1", "step_started", []byte(`{"step_id":"s1"}`)).
		WillReturnRows(sqlmock.NewRows([]string{"seq"}).AddRow(int64(0)))

	seq, err := repo.Append(context.Background(), "exec-1", "step_started", []byte(`{"step_id":"s1"}`))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if seq != 0 {
		t.Errorf("seq = %d, want 0", seq)
	}
}

// TestLiveEvent_Append_EmptyPayloadIsNull: nil/empty payload
// becomes JSONB null, matching the column's nullable shape.
// Without this, lib/pq would error on the bytea→jsonb coercion.
func TestLiveEvent_Append_EmptyPayloadIsNull(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewExecutionLiveEventRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO execution_live_events")).
		WithArgs("exec-empty", "step_started", nil).
		WillReturnRows(sqlmock.NewRows([]string{"seq"}).AddRow(int64(0)))

	if _, err := repo.Append(context.Background(), "exec-empty", "step_started", nil); err != nil {
		t.Errorf("Append: %v", err)
	}
}

// TestLiveEvent_Append_RequiresExecIDAndKind: defensive guard.
// A typo at the caller mustn't fire an INSERT with empty
// strings — would pollute the table.
func TestLiveEvent_Append_RequiresExecIDAndKind(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewExecutionLiveEventRepository(db)

	if _, err := repo.Append(context.Background(), "", "step_started", nil); err == nil {
		t.Errorf("expected error on empty execution_id")
	}
	if _, err := repo.Append(context.Background(), "e", "", nil); err == nil {
		t.Errorf("expected error on empty kind")
	}
}

// TestLiveEvent_ListSince_ReturnsOrderedSlice: scans rows in
// ascending seq order so subscribers replay without reordering
// on their side.
func TestLiveEvent_ListSince_ReturnsOrderedSlice(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewExecutionLiveEventRepository(db)

	now := time.Date(2026, 5, 23, 16, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, execution_id, seq, kind")).
		WithArgs("exec-1", int64(0)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "execution_id", "seq", "kind", "payload", "created_at"}).
			AddRow(int64(1), "exec-1", int64(0), "step_started", `{"step_id":"a"}`, now).
			AddRow(int64(2), "exec-1", int64(1), "step_completed", `{"step_id":"a"}`, now.Add(time.Second)))

	got, err := repo.ListSince(context.Background(), "exec-1", 0, 0)
	if err != nil {
		t.Fatalf("ListSince: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[0].Seq != 0 || got[1].Seq != 1 {
		t.Errorf("order wrong: %d, %d", got[0].Seq, got[1].Seq)
	}
}

// TestLiveEvent_ListSince_HonoursLimit: the limit param caps
// the result set. Without it, a Subscribe asking for fromSeq=0
// on a long-running execution would replay every event.
func TestLiveEvent_ListSince_HonoursLimit(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewExecutionLiveEventRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, execution_id, seq, kind")).
		WithArgs("exec-1", int64(0), 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "execution_id", "seq", "kind", "payload", "created_at"}).
			AddRow(int64(1), "exec-1", int64(0), "step_started", `{}`, time.Now()))

	got, err := repo.ListSince(context.Background(), "exec-1", 0, 1)
	if err != nil {
		t.Fatalf("ListSince: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("limit ignored: got %d rows", len(got))
	}
}

// TestLiveEvent_LatestSeq_NoRowsReturnsNegativeOne — the
// "fresh execution" sentinel. Callers branch on -1 to decide
// "no events yet" vs an actual zero seq.
func TestLiveEvent_LatestSeq_NoRowsReturnsNegativeOne(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewExecutionLiveEventRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(seq), -1)")).
		WithArgs("exec-fresh").
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(int64(-1)))

	got, err := repo.LatestSeq(context.Background(), "exec-fresh")
	if err != nil {
		t.Fatalf("LatestSeq: %v", err)
	}
	if got != -1 {
		t.Errorf("LatestSeq = %d, want -1", got)
	}
}

// TestLiveEvent_DeleteOlderThan_FiresPrune: the stale-event
// sweeper path. Just verify the DELETE goes out with the cutoff
// as the bind param.
func TestLiveEvent_DeleteOlderThan_FiresPrune(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewExecutionLiveEventRepository(db)

	cutoff := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM execution_live_events")).
		WithArgs(cutoff).
		WillReturnResult(sqlmock.NewResult(0, 42))

	n, err := repo.DeleteOlderThan(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("DeleteOlderThan: %v", err)
	}
	if n != 42 {
		t.Errorf("rows removed = %d, want 42", n)
	}
}
