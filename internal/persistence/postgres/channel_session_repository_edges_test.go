package postgres

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"vornik.io/vornik/internal/persistence"
)

func nowForTest() time.Time { return time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC) }

// TestChannelSession_Load_DriverError surfaces non-ErrNoRows
// driver failures so callers can distinguish "DB blip" from
// "fresh session". Without this branch the channel store
// could misclassify a transient connection error as "no row,
// fresh session" + clobber existing in-memory state.
func TestChannelSession_Load_DriverError(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewChannelSessionRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT kind, session_id")).
		WithArgs("webchat", "abc").
		WillReturnError(sql.ErrConnDone)

	_, err := repo.Load(context.Background(), "webchat", "abc")
	if err == nil {
		t.Fatalf("expected driver error")
	}
	if errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("driver error misclassified as ErrNotFound: %v", err)
	}
}

// TestChannelSession_Save_RequiresArgs defends the boundary
// guard — empty kind or sessionID would otherwise INSERT an
// unkeyable row collision-prone with the legacy "" sentinel.
func TestChannelSession_Save_RequiresArgs(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewChannelSessionRepository(db)

	if err := repo.Save(context.Background(), "", "s", "p", []byte(`[]`)); err == nil {
		t.Errorf("empty kind should error")
	}
	if err := repo.Save(context.Background(), "k", "", "p", []byte(`[]`)); err == nil {
		t.Errorf("empty sessionID should error")
	}
}

// TestChannelSession_Save_DriverError surfaces the wrapped
// failure so the channel store logs + serves from cache.
func TestChannelSession_Save_DriverError(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewChannelSessionRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO channel_sessions")).
		WillReturnError(sql.ErrConnDone)

	if err := repo.Save(context.Background(), "webchat", "abc", "proj", []byte(`[]`)); err == nil {
		t.Errorf("driver error should propagate")
	}
}

// TestChannelSession_Delete_DriverError surfaces the wrapped
// failure on the "clear chat" path.
func TestChannelSession_Delete_DriverError(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewChannelSessionRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM channel_sessions")).
		WithArgs("webchat", "abc").
		WillReturnError(sql.ErrConnDone)

	if err := repo.Delete(context.Background(), "webchat", "abc"); err == nil {
		t.Errorf("driver error should propagate")
	}
}

// TestOperatorProfile_Delete_DriverError mirrors the
// channel_session delete failure path. Same shape: surfaces
// the wrapped error so the CLI's "vornikctl operator forget"
// can report the failure instead of silently no-opping.
func TestOperatorProfile_Delete_DriverError(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewOperatorProfileRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM operator_profile")).
		WithArgs("telegram:42").
		WillReturnError(sql.ErrConnDone)

	if err := repo.Delete(context.Background(), "telegram:42"); err == nil {
		t.Errorf("driver error should propagate")
	}
}

// TestExecutionLiveEvent_DeleteOlderThan_RowsAffectedError —
// the prune path tolerates drivers that don't report row counts
// (returns 0 + nil error). Without this guard the future
// stale-event sweeper would log a phantom failure on every
// tick.
func TestExecutionLiveEvent_DeleteOlderThan_RowsAffectedError(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewExecutionLiveEventRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM execution_live_events")).
		WillReturnResult(sqlmock.NewErrorResult(errors.New("driver no rows-affected support")))

	n, err := repo.DeleteOlderThan(context.Background(), nowForTest())
	if err != nil {
		t.Errorf("rows-affected error should NOT propagate: %v", err)
	}
	if n != 0 {
		t.Errorf("rows = %d, want 0 when affected-count is unsupported", n)
	}
}

// TestExecutionLiveEvent_ListSince_RequiresArgs defends the
// empty-execution-id guard.
func TestExecutionLiveEvent_ListSince_RequiresArgs(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewExecutionLiveEventRepository(db)

	if _, err := repo.ListSince(context.Background(), "", 0, 100); err == nil {
		t.Errorf("empty execution_id should error")
	}
}

// TestExecutionLiveEvent_LatestSeq_RequiresArgs defends the
// boundary guard.
func TestExecutionLiveEvent_LatestSeq_RequiresArgs(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewExecutionLiveEventRepository(db)

	if _, err := repo.LatestSeq(context.Background(), ""); err == nil {
		t.Errorf("empty execution_id should error")
	}
}
