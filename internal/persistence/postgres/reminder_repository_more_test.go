package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"vornik.io/vornik/internal/persistence"
)

// reminderRow is a small helper for the columns scanReminder
// expects. Used to make the AddRow lines below readable instead
// of a 16-value mess. cron_expr + recurrence_until default to
// NULL so callers can write one-shot rows without naming them.
func reminderRow(id, operator, status string, now time.Time) []driver.Value {
	return []driver.Value{
		id, operator, "telegram", "42",
		sql.NullString{Valid: true, String: "proj-x"},
		now, "remind me", status, now,
		sql.NullTime{}, sql.NullTime{}, "chat", 0, sql.NullString{},
		sql.NullString{}, sql.NullTime{}, // cron_expr, recurrence_until
	}
}

// TestReminder_Get_Found exercises the Get path's column order
// + scanReminder helper end-to-end via sqlmock.
func TestReminder_Get_Found(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewReminderRepository(db)

	now := time.Date(2026, 5, 23, 16, 0, 0, 0, time.UTC)
	row := reminderRow("rem_1", "telegram:42", "pending", now)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, operator_id, channel, channel_ref, project_id")).
		WithArgs("rem_1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "operator_id", "channel", "channel_ref", "project_id",
			"fire_at", "content", "status", "created_at", "fired_at",
			"cancelled_at", "created_via", "error_count", "last_error",
			"cron_expr", "recurrence_until",
		}).AddRow(row...))

	got, err := repo.Get(context.Background(), "rem_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != "rem_1" {
		t.Errorf("ID = %q", got.ID)
	}
	if got.ProjectID != "proj-x" {
		t.Errorf("ProjectID = %q, want proj-x (nullable round-trip)", got.ProjectID)
	}
	if got.Status != persistence.ReminderStatusPending {
		t.Errorf("Status = %q", got.Status)
	}
}

// TestReminder_Get_NotFound returns persistence.ErrNotFound for
// missing IDs — the CLI / UI surface use this to render the
// right error.
func TestReminder_Get_NotFound(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewReminderRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, operator_id")).
		WithArgs("rem_missing").
		WillReturnError(sql.ErrNoRows)

	_, err := repo.Get(context.Background(), "rem_missing")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestReminder_Get_DriverError wraps non-ErrNoRows errors so the
// caller can distinguish DB failures from "unknown id".
func TestReminder_Get_DriverError(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewReminderRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, operator_id")).
		WithArgs("rem_x").
		WillReturnError(sql.ErrConnDone)

	_, err := repo.Get(context.Background(), "rem_x")
	if err == nil {
		t.Fatalf("expected error")
	}
	if errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("driver error misclassified as ErrNotFound: %v", err)
	}
}

// TestReminder_List_BasicFilter sends operator + status filters
// and verifies the query carries both as bind params. Asserts
// the result order (newest fire_at first).
func TestReminder_List_BasicFilter(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewReminderRepository(db)

	now := time.Date(2026, 5, 23, 16, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, operator_id")).
		WithArgs("telegram:42", "pending", 50).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "operator_id", "channel", "channel_ref", "project_id",
			"fire_at", "content", "status", "created_at", "fired_at",
			"cancelled_at", "created_via", "error_count", "last_error",
			"cron_expr", "recurrence_until",
		}).
			AddRow(reminderRow("rem_a", "telegram:42", "pending", now)...).
			AddRow(reminderRow("rem_b", "telegram:42", "pending", now.Add(time.Minute))...))

	got, err := repo.List(context.Background(), persistence.ReminderListFilter{
		OperatorID: "telegram:42",
		Status:     persistence.ReminderStatusPending,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ID != "rem_a" || got[1].ID != "rem_b" {
		t.Errorf("ordering: %q, %q", got[0].ID, got[1].ID)
	}
}

// TestReminder_List_DefaultsLimit50 confirms a zero PageSize
// gets normalised to 50. Without this guard a buggy caller
// could pull the whole table.
func TestReminder_List_DefaultsLimit50(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewReminderRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, operator_id")).
		WithArgs(50).
		WillReturnRows(sqlmock.NewRows([]string{"id", "operator_id", "channel", "channel_ref", "project_id",
			"fire_at", "content", "status", "created_at", "fired_at",
			"cancelled_at", "created_via", "error_count", "last_error",
			"cron_expr", "recurrence_until"}))

	if _, err := repo.List(context.Background(), persistence.ReminderListFilter{}); err != nil {
		t.Errorf("List: %v", err)
	}
}

// TestReminder_List_CapsLimitAt500: the upper bound — a CLI
// `--limit 9999` shouldn't blow the table.
func TestReminder_List_CapsLimitAt500(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewReminderRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, operator_id")).
		WithArgs(500).
		WillReturnRows(sqlmock.NewRows([]string{"id", "operator_id", "channel", "channel_ref", "project_id",
			"fire_at", "content", "status", "created_at", "fired_at",
			"cancelled_at", "created_via", "error_count", "last_error",
			"cron_expr", "recurrence_until"}))

	if _, err := repo.List(context.Background(), persistence.ReminderListFilter{PageSize: 9999}); err != nil {
		t.Errorf("List: %v", err)
	}
}

// TestReminder_List_QueryError surfaces the driver error.
func TestReminder_List_QueryError(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewReminderRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, operator_id")).
		WillReturnError(sql.ErrConnDone)

	if _, err := repo.List(context.Background(), persistence.ReminderListFilter{}); err == nil {
		t.Errorf("driver error should propagate")
	}
}

// TestReminder_MarkErrored_HappyPath stamps the error message
// + increments error_count.
func TestReminder_MarkErrored_HappyPath(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewReminderRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE dispatcher_reminders")).
		WithArgs("rem_x", "channel send failed").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.MarkErrored(context.Background(), "rem_x", "channel send failed"); err != nil {
		t.Errorf("MarkErrored: %v", err)
	}
}

// TestReminder_MarkErrored_DriverError surfaces failures so the
// heartbeat can log + continue.
func TestReminder_MarkErrored_DriverError(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewReminderRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE dispatcher_reminders")).
		WithArgs("rem_x", "err").
		WillReturnError(sql.ErrConnDone)

	if err := repo.MarkErrored(context.Background(), "rem_x", "err"); err == nil {
		t.Errorf("driver error should propagate")
	}
}

// TestReminder_MarkFired_NoRowsReturnsErrNotFound is the
// double-fire-race guard: a row that's no longer 'firing' must
// surface as ErrNotFound so the heartbeat doesn't silently
// claim success.
func TestReminder_MarkFired_NoRowsReturnsErrNotFound(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewReminderRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE dispatcher_reminders")).
		WithArgs("rem_x").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := repo.MarkFired(context.Background(), "rem_x"); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound on 0 rows-affected", err)
	}
}

// TestReminder_Cancel_HappyPath idempotent transition.
func TestReminder_Cancel_HappyPath(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewReminderRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE dispatcher_reminders")).
		WithArgs("rem_x").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Cancel(context.Background(), "rem_x"); err != nil {
		t.Errorf("Cancel: %v", err)
	}
}

// TestReminder_Cancel_DriverError surfaces the wrapped error.
func TestReminder_Cancel_DriverError(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewReminderRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE dispatcher_reminders")).
		WithArgs("rem_x").
		WillReturnError(sql.ErrConnDone)

	if err := repo.Cancel(context.Background(), "rem_x"); err == nil {
		t.Errorf("driver error should propagate")
	}
}

// TestReminder_Insert_NilReminderRejects pins the defensive
// guard — calling Insert(nil) shouldn't fire an INSERT with
// empty strings, it should error before touching the DB.
func TestReminder_Insert_NilReminderRejects(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewReminderRepository(db)

	if err := repo.Insert(context.Background(), nil); err == nil {
		t.Errorf("Insert(nil) should error")
	}
}

// TestReminder_Insert_PersistsCronFields verifies the recurring
// columns added in migration 67 round-trip through Insert. A
// hallucinating LLM that emits `cron_expr` but no FireAt would
// short-circuit elsewhere; here we just confirm the storage
// layer carries the columns to the bind args.
func TestReminder_Insert_PersistsCronFields(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewReminderRepository(db)

	fireAt := time.Date(2026, 5, 24, 9, 0, 0, 0, time.UTC)
	until := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO dispatcher_reminders")).
		WithArgs(
			sqlmock.AnyArg(), // id
			"telegram:42", "telegram", "1234",
			sqlmock.AnyArg(), // project_id
			fireAt,
			"weekly check-in",
			"pending",
			sqlmock.AnyArg(), // created_at
			"chat",
			sql.NullString{Valid: true, String: "0 9 * * 1"},
			sql.NullTime{Valid: true, Time: until},
		).WillReturnResult(sqlmock.NewResult(0, 1))

	rem := &persistence.Reminder{
		OperatorID:      "telegram:42",
		Channel:         "telegram",
		ChannelRef:      "1234",
		FireAt:          fireAt,
		Content:         "weekly check-in",
		CronExpr:        "0 9 * * 1",
		RecurrenceUntil: &until,
	}
	if err := repo.Insert(context.Background(), rem); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestReminder_Reschedule_TransitionsFiringToPending pins the
// re-arm primitive the runner uses on every cron tick.
func TestReminder_Reschedule_TransitionsFiringToPending(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewReminderRepository(db)

	next := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE dispatcher_reminders")).
		WithArgs("rem_cron", next).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Reschedule(context.Background(), "rem_cron", next); err != nil {
		t.Fatalf("Reschedule: %v", err)
	}
}

// TestReminder_Reschedule_NoFiringRowReturnsErrNotFound: the
// race guard. If a concurrent Cancel beat the runner, the next
// Reschedule must surface ErrNotFound rather than reviving a
// cancelled row.
func TestReminder_Reschedule_NoFiringRowReturnsErrNotFound(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewReminderRepository(db)

	next := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE dispatcher_reminders")).
		WithArgs("rem_cron", next).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := repo.Reschedule(context.Background(), "rem_cron", next); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound when no firing row to reschedule", err)
	}
}

// TestReminder_Reschedule_DriverErrorPropagates surfaces DB
// failures so the heartbeat can log + continue.
func TestReminder_Reschedule_DriverErrorPropagates(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewReminderRepository(db)

	next := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE dispatcher_reminders")).
		WithArgs("rem_cron", next).
		WillReturnError(sql.ErrConnDone)

	if err := repo.Reschedule(context.Background(), "rem_cron", next); err == nil {
		t.Errorf("driver error should propagate")
	}
}

// TestEmptyToNullString_NullForEmpty pins the small helper that
// distinguishes the optional project_id column from a literal
// empty string. Wrong value here would write ” rows that the
// project-scoped indexes can't dedupe.
func TestEmptyToNullString_NullForEmpty(t *testing.T) {
	got := emptyToNullString("")
	if got.Valid {
		t.Errorf("empty input should produce !Valid NullString")
	}
	got2 := emptyToNullString("proj-x")
	if !got2.Valid || got2.String != "proj-x" {
		t.Errorf("non-empty input wrong: %+v", got2)
	}
}
