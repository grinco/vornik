package postgres

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestLeaderLock_List pins the SELECT shape so a future column
// reorder doesn't silently misalign the Scan targets (epoch included).
func TestLeaderLock_List(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewLeaderLockRepository(db)

	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT worker_id, holder_id, acquired_at, renewed_at, expires_at, epoch\nFROM daemon_leader_locks")).
		WillReturnRows(sqlmock.NewRows([]string{
			"worker_id", "holder_id", "acquired_at", "renewed_at", "expires_at", "epoch",
		}).
			AddRow("archive_sweeper", "host-a:1:abc", now.Add(-1*time.Hour), now.Add(-5*time.Second), now.Add(55*time.Second), int64(1)).
			AddRow("autonomy_manager", "host-a:1:abc", now.Add(-2*time.Hour), now.Add(-1*time.Second), now.Add(59*time.Second), int64(2)))

	rows, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows len = %d, want 2", len(rows))
	}
	if rows[0].WorkerID != "archive_sweeper" {
		t.Errorf("first row worker_id = %q, want archive_sweeper", rows[0].WorkerID)
	}
	if rows[1].WorkerID != "autonomy_manager" {
		t.Errorf("second row worker_id = %q, want autonomy_manager", rows[1].WorkerID)
	}
}

// TestLeaderLock_List_Empty: empty rowset → nil slice + nil err.
// Fresh-deployment shape.
func TestLeaderLock_List_Empty(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewLeaderLockRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("FROM daemon_leader_locks")).
		WillReturnRows(sqlmock.NewRows([]string{"worker_id", "holder_id", "acquired_at", "renewed_at", "expires_at", "epoch"}))

	rows, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List on empty table: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows len = %d, want 0", len(rows))
	}
}
