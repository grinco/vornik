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

// TestLeaderLock_Get_Found pins the column order and Scan
// alignment for the single-row diagnostic query (epoch included).
func TestLeaderLock_Get_Found(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewLeaderLockRepository(db)

	now := time.Date(2026, 5, 23, 16, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT worker_id, holder_id")).
		WithArgs("archive_sweeper").
		WillReturnRows(sqlmock.NewRows([]string{"worker_id", "holder_id", "acquired_at", "renewed_at", "expires_at", "epoch"}).
			AddRow("archive_sweeper", "host-a:1:abc", now, now, now.Add(60*time.Second), int64(3)))

	got, err := repo.Get(context.Background(), "archive_sweeper")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.WorkerID != "archive_sweeper" {
		t.Errorf("worker_id = %q", got.WorkerID)
	}
	if got.HolderID != "host-a:1:abc" {
		t.Errorf("holder_id = %q", got.HolderID)
	}
	if !got.ExpiresAt.After(now) {
		t.Errorf("expires_at should be in the future relative to now")
	}
	if got.Epoch != 3 {
		t.Errorf("epoch = %d, want 3", got.Epoch)
	}
}

// TestLeaderLock_Get_NotFound returns persistence.ErrNotFound so
// the doctor check + admin UI branch on "fresh deployment, no
// rows yet".
func TestLeaderLock_Get_NotFound(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewLeaderLockRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT worker_id, holder_id")).
		WithArgs("never_acquired").
		WillReturnRows(sqlmock.NewRows([]string{"worker_id", "holder_id", "acquired_at", "renewed_at", "expires_at", "epoch"}))

	_, err := repo.Get(context.Background(), "never_acquired")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestLeaderLock_Get_ScanError wraps a non-ErrNoRows driver
// error so the doctor check can surface a clear "DB blip" cause
// instead of misclassifying as "fresh deployment".
func TestLeaderLock_Get_ScanError(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewLeaderLockRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT worker_id, holder_id")).
		WithArgs("any").
		WillReturnError(sql.ErrConnDone)

	_, err := repo.Get(context.Background(), "any")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("driver error should NOT be ErrNotFound: %v", err)
	}
}

// TestLeaderLock_Acquire_RequiresArgs is the defensive guard:
// empty workerID / holderID would otherwise INSERT an
// unkeyable row. The error contract is the boundary.
func TestLeaderLock_Acquire_RequiresArgs(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewLeaderLockRepository(db)

	if _, _, err := repo.Acquire(context.Background(), "", "h", time.Now(), time.Second); err == nil {
		t.Errorf("empty workerID should error")
	}
	if _, _, err := repo.Acquire(context.Background(), "w", "", time.Now(), time.Second); err == nil {
		t.Errorf("empty holderID should error")
	}
	if _, _, err := repo.Acquire(context.Background(), "w", "h", time.Now(), 0); err == nil {
		t.Errorf("zero ttl should error")
	}
}

// TestLeaderLock_Renew_RequiresArgs mirrors Acquire's guard.
func TestLeaderLock_Renew_RequiresArgs(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewLeaderLockRepository(db)

	if _, err := repo.Renew(context.Background(), "", "h", time.Now(), time.Second); err == nil {
		t.Errorf("empty workerID should error")
	}
	if _, err := repo.Renew(context.Background(), "w", "", time.Now(), time.Second); err == nil {
		t.Errorf("empty holderID should error")
	}
	if _, err := repo.Renew(context.Background(), "w", "h", time.Now(), 0); err == nil {
		t.Errorf("zero ttl should error")
	}
}

// TestLeaderLock_Renew_NotHolder: UPDATE matches zero rows when
// holder_id has been taken by another daemon. Renew reports
// false + nil error so the worker's renew loop knows to step
// down on the next tick.
func TestLeaderLock_Renew_NotHolder(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewLeaderLockRepository(db)

	now := time.Now()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE daemon_leader_locks")).
		WithArgs("w", "us", now.UTC(), now.Add(60*time.Second).UTC()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	ok, err := repo.Renew(context.Background(), "w", "us", now, 60*time.Second)
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if ok {
		t.Errorf("Renew with 0 rows-affected should return false")
	}
}

// TestLeaderLock_Release_HappyPath sends the holder-bound
// UPDATE; happens at executor.Pause shutdown + on the
// Elector's gracefulRelease path. No return-value check beyond
// nil-err — Release is best-effort.
func TestLeaderLock_Release_HappyPath(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewLeaderLockRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE daemon_leader_locks")).
		WithArgs("w", "us").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Release(context.Background(), "w", "us"); err != nil {
		t.Errorf("Release: %v", err)
	}
}

// TestLeaderLock_Release_DriverError surfaces the wrapped
// error so the elector can log + continue rather than panic.
func TestLeaderLock_Release_DriverError(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewLeaderLockRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE daemon_leader_locks")).
		WithArgs("w", "us").
		WillReturnError(sql.ErrConnDone)

	if err := repo.Release(context.Background(), "w", "us"); err == nil {
		t.Errorf("driver error should propagate")
	}
}
