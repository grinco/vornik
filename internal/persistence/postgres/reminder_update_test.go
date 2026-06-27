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

// TestReminder_UpdateFields_AppliesBothFireAtAndContent: the
// update-reminder dispatcher tool needs to change fire_at +
// content in one operation. Without the combined update the
// tool would have to do two round-trips and race against the
// heartbeat picking up the row mid-edit.
func TestReminder_UpdateFields_AppliesBothFireAtAndContent(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewReminderRepository(db)

	newFire := time.Date(2026, 5, 24, 9, 0, 0, 0, time.UTC)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE dispatcher_reminders")).
		WithArgs("rem_x", newFire.UTC(), "new content").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.UpdateFields(context.Background(), "rem_x", newFire, "new content"); err != nil {
		t.Errorf("UpdateFields: %v", err)
	}
}

// TestReminder_UpdateFields_RefusesIfNotPending: a row that's
// already firing / fired / cancelled MUST NOT be modified —
// the heartbeat may already be sending it. Empty rows-affected
// surfaces as ErrNotFound so the dispatcher tool can tell the
// operator the reminder was already in flight.
func TestReminder_UpdateFields_RefusesIfNotPending(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewReminderRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE dispatcher_reminders")).
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := repo.UpdateFields(context.Background(), "rem_x", time.Now(), "x")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound on non-pending row", err)
	}
}

// TestReminder_UpdateFields_DriverErrorPropagates: a DB blip
// surfaces so the tool can tell the operator instead of
// silently claiming success.
func TestReminder_UpdateFields_DriverErrorPropagates(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewReminderRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE dispatcher_reminders")).
		WillReturnError(sql.ErrConnDone)

	if err := repo.UpdateFields(context.Background(), "rem_x", time.Now(), "x"); err == nil {
		t.Errorf("driver error should propagate")
	}
}
