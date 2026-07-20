//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// firingTaskReminder inserts a due task-kind reminder and leases it,
// mirroring the real spawn path: LeaseDue transitions pending->firing,
// which is the precondition MarkTaskSpawned/ClaimDelivery/FinalizeDelivery
// all guard on. Mirrors the brief's fictional helper against the real
// integration harness (newIntegrationDB / NewReminderRepository), per
// the Task 4 correction — the brief's newTestDB doesn't exist in this repo.
func firingTaskReminder(t *testing.T, ctx context.Context, repo *ReminderRepository) *persistence.Reminder {
	t.Helper()
	rem := &persistence.Reminder{
		OperatorID: "telegram:1", Channel: "telegram", ChannelRef: "1",
		ProjectID: "news", FireAt: time.Now().Add(-time.Minute).UTC(),
		Content: "digest", Kind: persistence.ReminderKindTask, CronExpr: "0 7 * * *",
	}
	if err := repo.Insert(ctx, rem); err != nil {
		t.Fatalf("insert: %v", err)
	}
	due, err := repo.LeaseDue(ctx, time.Now(), 10)
	if err != nil || len(due) == 0 {
		t.Fatalf("lease: %v n=%d", err, len(due))
	}
	for _, d := range due {
		if d.ID == rem.ID {
			return d
		}
	}
	t.Fatalf("leased batch did not include the inserted reminder (id=%s)", rem.ID)
	return nil
}

// TestMarkTaskSpawnedThenClaimAndFinalizeRecurring pins the recurring
// happy path end-to-end: firing -> awaiting_task (spawn, fire_at
// re-armed) -> firing (claim) -> pending (finalize, non-terminal).
func TestMarkTaskSpawnedThenClaimAndFinalizeRecurring(t *testing.T) {
	db := newIntegrationDB(t)
	repo := NewReminderRepository(db)
	ctx := context.Background()
	rem := firingTaskReminder(t, ctx, repo)
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM dispatcher_reminders WHERE id = $1`, rem.ID)
	})

	next := time.Now().Add(24 * time.Hour).UTC()
	if err := repo.MarkTaskSpawned(ctx, rem.ID, "task_1", &next); err != nil {
		t.Fatalf("MarkTaskSpawned: %v", err)
	}
	got, err := repo.Get(ctx, rem.ID)
	if err != nil {
		t.Fatalf("Get after spawn: %v", err)
	}
	if got.Status != persistence.ReminderStatusAwaitingTask || got.LastTaskID != "task_1" {
		t.Fatalf("after spawn: status=%s last_task=%s", got.Status, got.LastTaskID)
	}

	// First completion wins the claim.
	claimed, ok, err := repo.ClaimDelivery(ctx, "task_1")
	if err != nil || !ok || claimed == nil || claimed.ID != rem.ID {
		t.Fatalf("claim1: claimed=%v ok=%v err=%v", claimed, ok, err)
	}
	if claimed.Status != persistence.ReminderStatusFiring {
		t.Fatalf("claimed row should be firing, got %s", claimed.Status)
	}

	// Recurring finalize -> pending.
	if err := repo.FinalizeDelivery(ctx, rem.ID, "task_1", false); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	got, err = repo.Get(ctx, rem.ID)
	if err != nil {
		t.Fatalf("Get after finalize: %v", err)
	}
	if got.Status != persistence.ReminderStatusPending || got.LastDeliveredTaskID != "task_1" {
		t.Fatalf("after finalize: status=%s delivered=%s", got.Status, got.LastDeliveredTaskID)
	}
}

// TestClaimDeliveryIsAtMostOnce is the correctness-critical
// regression: a duplicate completion callback for the same task
// (e.g. a retried webhook, or two HA instances racing the same
// callback) must NOT re-claim delivery a second time. The conditional
// UPDATE...WHERE last_task_id=$1 AND status='awaiting_task' AND
// last_delivered_task_id IS DISTINCT FROM $1 is the serialization
// point under test.
func TestClaimDeliveryIsAtMostOnce(t *testing.T) {
	db := newIntegrationDB(t)
	repo := NewReminderRepository(db)
	ctx := context.Background()
	rem := firingTaskReminder(t, ctx, repo)
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM dispatcher_reminders WHERE id = $1`, rem.ID)
	})
	next := time.Now().Add(24 * time.Hour).UTC()
	if err := repo.MarkTaskSpawned(ctx, rem.ID, "task_1", &next); err != nil {
		t.Fatalf("MarkTaskSpawned: %v", err)
	}

	_, ok1, err1 := repo.ClaimDelivery(ctx, "task_1")
	if err1 != nil {
		t.Fatalf("claim1 err: %v", err1)
	}
	if err := repo.FinalizeDelivery(ctx, rem.ID, "task_1", false); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	// A duplicate completion callback for the same task must NOT re-claim.
	claimed2, ok2, err2 := repo.ClaimDelivery(ctx, "task_1")
	if err2 != nil {
		t.Fatalf("claim2 err: %v", err2)
	}
	if !ok1 || ok2 {
		t.Fatalf("expected first claim to win and second to lose: ok1=%v ok2=%v", ok1, ok2)
	}
	if claimed2 != nil {
		t.Fatalf("second claim should return a nil row, got %+v", claimed2)
	}
}

// TestFinalizeDeliveryOneShotGoesFired covers the one-shot path:
// MarkTaskSpawned with nextFireAt=nil leaves fire_at untouched
// (COALESCE), and a terminal FinalizeDelivery moves the row to the
// terminal 'fired' status rather than back to 'pending'.
func TestFinalizeDeliveryOneShotGoesFired(t *testing.T) {
	db := newIntegrationDB(t)
	repo := NewReminderRepository(db)
	ctx := context.Background()
	rem := firingTaskReminder(t, ctx, repo)
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM dispatcher_reminders WHERE id = $1`, rem.ID)
	})

	if err := repo.MarkTaskSpawned(ctx, rem.ID, "task_1", nil); err != nil { // one-shot: nil next
		t.Fatalf("MarkTaskSpawned: %v", err)
	}
	if _, ok, err := repo.ClaimDelivery(ctx, "task_1"); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if err := repo.FinalizeDelivery(ctx, rem.ID, "task_1", true); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Get(ctx, rem.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != persistence.ReminderStatusFired {
		t.Fatalf("one-shot after finalize: status=%s want fired", got.Status)
	}
	if got.LastDeliveredTaskID != "task_1" {
		t.Fatalf("last_delivered_task_id = %q, want task_1", got.LastDeliveredTaskID)
	}
}

// TestMarkTaskSpawned_RefusesNonFiring pins the ErrNotFound guard:
// a row not currently 'firing' (e.g. already awaiting_task) must not
// be re-spawned.
func TestMarkTaskSpawned_RefusesNonFiring(t *testing.T) {
	db := newIntegrationDB(t)
	repo := NewReminderRepository(db)
	ctx := context.Background()
	rem := firingTaskReminder(t, ctx, repo)
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM dispatcher_reminders WHERE id = $1`, rem.ID)
	})

	if err := repo.MarkTaskSpawned(ctx, rem.ID, "task_1", nil); err != nil {
		t.Fatalf("first spawn: %v", err)
	}
	// Row is now awaiting_task, not firing — a second spawn attempt
	// must be refused.
	if err := repo.MarkTaskSpawned(ctx, rem.ID, "task_2", nil); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("second spawn: want ErrNotFound, got %v", err)
	}
}

// TestFinalizeDelivery_RefusesNonFiring pins the ErrNotFound guard on
// FinalizeDelivery: a row not in 'firing' (e.g. a retried finalize
// call after the row already moved to pending/fired) must not
// silently re-stamp last_delivered_task_id / fired_at.
func TestFinalizeDelivery_RefusesNonFiring(t *testing.T) {
	db := newIntegrationDB(t)
	repo := NewReminderRepository(db)
	ctx := context.Background()
	rem := firingTaskReminder(t, ctx, repo)
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM dispatcher_reminders WHERE id = $1`, rem.ID)
	})
	next := time.Now().Add(24 * time.Hour).UTC()
	if err := repo.MarkTaskSpawned(ctx, rem.ID, "task_1", &next); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if _, ok, err := repo.ClaimDelivery(ctx, "task_1"); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if err := repo.FinalizeDelivery(ctx, rem.ID, "task_1", false); err != nil {
		t.Fatalf("first finalize: %v", err)
	}
	// Row is now 'pending' — a retried finalize call must be refused.
	if err := repo.FinalizeDelivery(ctx, rem.ID, "task_1", false); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("second finalize: want ErrNotFound, got %v", err)
	}
}
