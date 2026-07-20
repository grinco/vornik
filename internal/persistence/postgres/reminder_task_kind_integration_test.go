//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TestInsertGetRoundTripsTaskKind pins migration 130's three new
// columns (kind, last_task_id, last_delivered_task_id) round-tripping
// through Insert -> Get against a real Postgres instance. Task-kind
// reminders require a non-empty ProjectID (chk_task_reminder_has_project),
// so this also exercises that constraint's happy path.
func TestInsertGetRoundTripsTaskKind(t *testing.T) {
	db := newIntegrationDB(t)
	repo := NewReminderRepository(db)
	ctx := context.Background()

	rem := &persistence.Reminder{
		OperatorID: "telegram:42", Channel: "telegram", ChannelRef: "42",
		ProjectID: "news", FireAt: time.Now().Add(time.Hour).UTC(),
		Content: "Daily news digest", Kind: persistence.ReminderKindTask,
		CronExpr: "0 7 * * *",
	}
	if err := repo.Insert(ctx, rem); err != nil {
		t.Fatalf("insert: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM dispatcher_reminders WHERE id = $1`, rem.ID)
	})

	got, err := repo.Get(ctx, rem.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Kind != persistence.ReminderKindTask {
		t.Fatalf("kind = %q, want task", got.Kind)
	}
	if got.LastTaskID != "" {
		t.Errorf("LastTaskID should be empty on a freshly-inserted reminder, got %q", got.LastTaskID)
	}
	if got.LastDeliveredTaskID != "" {
		t.Errorf("LastDeliveredTaskID should be empty on a freshly-inserted reminder, got %q", got.LastDeliveredTaskID)
	}
}

// TestInsertGetRoundTripsTextKindDefault confirms the default kind
// ("text") round-trips for reminders that don't set Kind at all —
// the shipped behavior prior to migration 130 must be unaffected.
func TestInsertGetRoundTripsTextKindDefault(t *testing.T) {
	db := newIntegrationDB(t)
	repo := NewReminderRepository(db)
	ctx := context.Background()

	rem := &persistence.Reminder{
		OperatorID: "telegram:43", Channel: "telegram", ChannelRef: "43",
		FireAt: time.Now().Add(time.Hour).UTC(), Content: "plain reminder",
	}
	if err := repo.Insert(ctx, rem); err != nil {
		t.Fatalf("insert: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM dispatcher_reminders WHERE id = $1`, rem.ID)
	})

	got, err := repo.Get(ctx, rem.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Kind != persistence.ReminderKindText {
		t.Fatalf("kind = %q, want text (default)", got.Kind)
	}
}

// TestCountTaskByOperator pins the per-operator task-cap query
// (idx_dispatcher_reminders_operator_kind_status). Only task-kind,
// non-terminal rows for the given operator should count; a
// different operator's rows must not leak into the total.
func TestCountTaskByOperator(t *testing.T) {
	db := newIntegrationDB(t)
	repo := NewReminderRepository(db)
	ctx := context.Background()
	op := "telegram:99"

	var ids []string
	for i := 0; i < 3; i++ {
		rem := &persistence.Reminder{
			OperatorID: op, Channel: "telegram", ChannelRef: "99",
			ProjectID: "p", FireAt: time.Now().Add(time.Hour), Content: "c",
			Kind: persistence.ReminderKindTask, CronExpr: "0 7 * * *",
		}
		if err := repo.Insert(ctx, rem); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		ids = append(ids, rem.ID)
	}
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = db.ExecContext(ctx, `DELETE FROM dispatcher_reminders WHERE id = $1`, id)
		}
	})

	// A text-kind reminder for the same operator must not be counted.
	textRem := &persistence.Reminder{
		OperatorID: op, Channel: "telegram", ChannelRef: "99",
		FireAt: time.Now().Add(time.Hour), Content: "not a task",
	}
	if err := repo.Insert(ctx, textRem); err != nil {
		t.Fatalf("insert text reminder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM dispatcher_reminders WHERE id = $1`, textRem.ID)
	})

	n, err := repo.CountTaskByOperator(ctx, op)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Fatalf("count = %d, want 3", n)
	}
}
