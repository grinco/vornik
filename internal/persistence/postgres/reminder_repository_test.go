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

// TestReminder_Insert_StampsDefaults pins the row-level defaults
// (id generation, created_at = now, status forced to pending).
func TestReminder_Insert_StampsDefaults(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewReminderRepository(db)

	fireAt := time.Date(2026, 5, 24, 9, 0, 0, 0, time.UTC)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO dispatcher_reminders")).
		WithArgs(
			sqlmock.AnyArg(), // id
			"telegram:42", "telegram", "1234",
			sqlmock.AnyArg(), // project_id NullString
			fireAt,
			"Prague Castle event",
			"pending",
			sqlmock.AnyArg(), // created_at
			"chat",
			sqlmock.AnyArg(), // cron_expr NullString
			sqlmock.AnyArg(), // recurrence_until NullTime
		).WillReturnResult(sqlmock.NewResult(0, 1))

	rem := &persistence.Reminder{
		OperatorID: "telegram:42",
		Channel:    "telegram",
		ChannelRef: "1234",
		FireAt:     fireAt,
		Content:    "Prague Castle event",
	}
	if err := repo.Insert(context.Background(), rem); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if rem.ID == "" {
		t.Errorf("ID should be auto-generated")
	}
	if rem.Status != persistence.ReminderStatusPending {
		t.Errorf("status forced to pending; got %q", rem.Status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestReminder_LeaseDue pins the CTE shape and the
// FOR UPDATE SKIP LOCKED claim. The mock returns one row; the
// repo should hand it back with status flipped to firing.
func TestReminder_LeaseDue(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewReminderRepository(db)

	now := time.Now().UTC()
	rows := sqlmock.NewRows([]string{
		"id", "operator_id", "channel", "channel_ref", "project_id",
		"fire_at", "content", "status", "created_at", "fired_at",
		"cancelled_at", "created_via", "error_count", "last_error",
		"cron_expr", "recurrence_until",
	}).AddRow(
		"rem_001", "telegram:42", "telegram", "1234", nil,
		now.Add(-1*time.Minute), "go check the deploy", "firing", now.Add(-10*time.Minute), nil,
		nil, "chat", 0, nil,
		nil, nil,
	)
	mock.ExpectQuery(regexp.QuoteMeta("FOR UPDATE SKIP LOCKED")).
		WithArgs(sqlmock.AnyArg(), 10).
		WillReturnRows(rows)

	out, err := repo.LeaseDue(context.Background(), now, 10)
	if err != nil {
		t.Fatalf("LeaseDue: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 row, got %d", len(out))
	}
	if out[0].ID != "rem_001" {
		t.Errorf("id mismatch: %q", out[0].ID)
	}
	if out[0].Status != persistence.ReminderStatusFiring {
		t.Errorf("status should be firing; got %q", out[0].Status)
	}
}

// TestReminder_MarkFired_RefusesNonFiring covers the double-fire
// race guard — the WHERE status='firing' clause means a row
// already flipped to 'fired' returns ErrNotFound on a second
// attempt.
func TestReminder_MarkFired_RefusesNonFiring(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewReminderRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE dispatcher_reminders")).
		WithArgs("rem_already_done").
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := repo.MarkFired(context.Background(), "rem_already_done")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestReminder_Cancel_Idempotent confirms cancel doesn't error
// when the row is already terminal (the WHERE clause filters
// them out; the UPDATE affects 0 rows but returns nil).
func TestReminder_Cancel_Idempotent(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewReminderRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE dispatcher_reminders")).
		WithArgs("rem_fired").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := repo.Cancel(context.Background(), "rem_fired"); err != nil {
		t.Errorf("Cancel on already-terminal row should be nil; got %v", err)
	}
}

// TestReminder_CountPendingByOperator covers the cap-enforcement
// query the set_reminder tool consults.
func TestReminder_CountPendingByOperator(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewReminderRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM dispatcher_reminders")).
		WithArgs("telegram:42").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(7))

	n, err := repo.CountPendingByOperator(context.Background(), "telegram:42")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 7 {
		t.Errorf("count=%d, want 7", n)
	}
}

// TestReminderStatus_IsTerminal pins the terminal set the
// heartbeat and cancel surfaces share.
func TestReminderStatus_IsTerminal(t *testing.T) {
	cases := []struct {
		s        persistence.ReminderStatus
		terminal bool
	}{
		{persistence.ReminderStatusPending, false},
		{persistence.ReminderStatusFiring, false},
		{persistence.ReminderStatusFired, true},
		{persistence.ReminderStatusCancelled, true},
		{persistence.ReminderStatusExpired, true},
	}
	for _, tc := range cases {
		if got := tc.s.IsTerminal(); got != tc.terminal {
			t.Errorf("IsTerminal(%q) = %v, want %v", tc.s, got, tc.terminal)
		}
	}
}
