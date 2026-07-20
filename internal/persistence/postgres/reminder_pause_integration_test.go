//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TestPauseFromPending pins the happy path: a pending row transitions
// to 'paused' on Pause. Design §5.4.
func TestPauseFromPending(t *testing.T) {
	db := newIntegrationDB(t)
	repo := NewReminderRepository(db)
	ctx := context.Background()

	rem := &persistence.Reminder{
		OperatorID: "telegram:1", Channel: "telegram", ChannelRef: "1",
		ProjectID: "news", FireAt: time.Now().Add(time.Hour).UTC(),
		Content: "digest",
	}
	if err := repo.Insert(ctx, rem); err != nil {
		t.Fatalf("insert: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM dispatcher_reminders WHERE id = $1`, rem.ID)
	})

	if err := repo.Pause(ctx, rem.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	got, err := repo.Get(ctx, rem.ID)
	if err != nil {
		t.Fatalf("Get after pause: %v", err)
	}
	if got.Status != persistence.ReminderStatusPaused {
		t.Fatalf("status = %s, want paused", got.Status)
	}
}

// TestPause_RefusesNonPending is the correctness-critical regression:
// Pause must NOT mute an in-flight reminder. A row reached via
// LeaseDue -> MarkTaskSpawned sits in 'awaiting_task' — Pause on it
// must return ErrNotFound and leave status untouched, per design §5.4
// ("pause only from pending"). Mirrors the firingTaskReminder helper
// from reminder_task_transitions_integration_test.go (Task 4).
func TestPause_RefusesNonPending(t *testing.T) {
	db := newIntegrationDB(t)
	repo := NewReminderRepository(db)
	ctx := context.Background()
	rem := firingTaskReminder(t, ctx, repo)
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM dispatcher_reminders WHERE id = $1`, rem.ID)
	})

	// Row is 'firing' post-lease; drive it to 'awaiting_task' the way
	// the real spawn path does.
	if err := repo.MarkTaskSpawned(ctx, rem.ID, "task_1", nil); err != nil {
		t.Fatalf("MarkTaskSpawned: %v", err)
	}

	if err := repo.Pause(ctx, rem.ID); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("Pause on awaiting_task row: want ErrNotFound, got %v", err)
	}
	got, err := repo.Get(ctx, rem.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != persistence.ReminderStatusAwaitingTask {
		t.Fatalf("status = %s, want awaiting_task (unchanged)", got.Status)
	}
}

// TestResumeFromPaused pins Resume's happy path: paused -> pending
// with fire_at set to the supplied nextFireAt.
func TestResumeFromPaused(t *testing.T) {
	db := newIntegrationDB(t)
	repo := NewReminderRepository(db)
	ctx := context.Background()

	rem := &persistence.Reminder{
		OperatorID: "telegram:1", Channel: "telegram", ChannelRef: "1",
		ProjectID: "news", FireAt: time.Now().Add(time.Hour).UTC(),
		Content: "digest",
	}
	if err := repo.Insert(ctx, rem); err != nil {
		t.Fatalf("insert: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM dispatcher_reminders WHERE id = $1`, rem.ID)
	})
	if err := repo.Pause(ctx, rem.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	next := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Microsecond)
	if err := repo.Resume(ctx, rem.ID, next); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	got, err := repo.Get(ctx, rem.ID)
	if err != nil {
		t.Fatalf("Get after resume: %v", err)
	}
	if got.Status != persistence.ReminderStatusPending {
		t.Fatalf("status = %s, want pending", got.Status)
	}
	if !got.FireAt.Equal(next) {
		t.Fatalf("fire_at = %v, want %v", got.FireAt, next)
	}
}

// TestResume_RefusesNonPaused: a non-paused row (e.g. still pending)
// must not be re-armed by Resume — ErrNotFound.
func TestResume_RefusesNonPaused(t *testing.T) {
	db := newIntegrationDB(t)
	repo := NewReminderRepository(db)
	ctx := context.Background()

	rem := &persistence.Reminder{
		OperatorID: "telegram:1", Channel: "telegram", ChannelRef: "1",
		ProjectID: "news", FireAt: time.Now().Add(time.Hour).UTC(),
		Content: "digest",
	}
	if err := repo.Insert(ctx, rem); err != nil {
		t.Fatalf("insert: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM dispatcher_reminders WHERE id = $1`, rem.ID)
	})

	next := time.Now().Add(48 * time.Hour).UTC()
	if err := repo.Resume(ctx, rem.ID, next); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("Resume on pending row: want ErrNotFound, got %v", err)
	}
	got, err := repo.Get(ctx, rem.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != persistence.ReminderStatusPending {
		t.Fatalf("status = %s, want pending (unchanged)", got.Status)
	}
}
