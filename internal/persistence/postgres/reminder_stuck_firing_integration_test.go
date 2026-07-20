//go:build integration

package postgres

// Task 14: stuck-`firing` reconciliation sweep (crash recovery, design
// §9). ReclaimStuckFiring is the read side: a plain SELECT ... FOR
// UPDATE SKIP LOCKED over status='firing' AND fired_at < olderThan.
// These tests pin the WHERE clause against a real Postgres instance:
// old-enough firing rows are returned, recent firing rows are not, and
// awaiting_task rows (the long-running-task state, not stuck) are never
// returned regardless of how stale fired_at is.

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// backdateFiredAt directly rewrites fired_at, bypassing the repo API —
// the only way to get an old fired_at onto a row still in 'firing',
// since LeaseDue/ClaimDelivery always stamp fired_at=NOW().
func backdateFiredAt(t *testing.T, ctx context.Context, db *DB, id string, firedAt time.Time) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `UPDATE dispatcher_reminders SET fired_at = $2 WHERE id = $1`, id, firedAt.UTC()); err != nil {
		t.Fatalf("backdate fired_at: %v", err)
	}
}

// TestReclaimStuckFiring_ReturnsOldFiringRow: a row left in 'firing'
// with a fired_at well past the grace window is exactly what the
// crash-recovery sweep must reclaim.
func TestReclaimStuckFiring_ReturnsOldFiringRow(t *testing.T) {
	db := newIntegrationDB(t)
	repo := NewReminderRepository(db)
	ctx := context.Background()
	rem := firingTaskReminder(t, ctx, repo) // inserts + LeaseDue's it to 'firing'
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM dispatcher_reminders WHERE id = $1`, rem.ID)
	})

	old := time.Now().Add(-1 * time.Hour).UTC()
	backdateFiredAt(t, ctx, db, rem.ID, old)

	cutoff := time.Now().Add(-15 * time.Minute)
	stuck, err := repo.ReclaimStuckFiring(ctx, cutoff, 10)
	if err != nil {
		t.Fatalf("ReclaimStuckFiring: %v", err)
	}
	found := false
	for _, s := range stuck {
		if s.ID == rem.ID {
			found = true
			if s.Status != persistence.ReminderStatusFiring {
				t.Errorf("returned row status = %s, want firing", s.Status)
			}
		}
	}
	if !found {
		t.Fatalf("stuck-firing row %s not returned; got %d rows", rem.ID, len(stuck))
	}
}

// TestReclaimStuckFiring_ExcludesRecentFiring: a row that just entered
// 'firing' (fired_at within the grace window) must NOT be reclaimed —
// it's mid-delivery, not stuck.
func TestReclaimStuckFiring_ExcludesRecentFiring(t *testing.T) {
	db := newIntegrationDB(t)
	repo := NewReminderRepository(db)
	ctx := context.Background()
	rem := firingTaskReminder(t, ctx, repo) // fired_at ~= NOW()
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM dispatcher_reminders WHERE id = $1`, rem.ID)
	})

	cutoff := time.Now().Add(-15 * time.Minute)
	stuck, err := repo.ReclaimStuckFiring(ctx, cutoff, 10)
	if err != nil {
		t.Fatalf("ReclaimStuckFiring: %v", err)
	}
	for _, s := range stuck {
		if s.ID == rem.ID {
			t.Fatalf("recently-fired row %s must not be reclaimed within the grace window", rem.ID)
		}
	}
}

// TestClaimDeliveryRefreshesFiredAt_ExcludesFromStuckSweep is the
// regression test for the final-review IMPORTANT finding: ClaimDelivery
// flipped a row to 'firing' without refreshing fired_at, so a task
// delivery that outlives FiringGrace (routine for a slow digest —
// FiringGrace defaults to 15m) re-entered 'firing' carrying a stale
// fired_at from its original spawn. A concurrent sweepStuckFiring tick
// would then falsely reclaim the row mid-delivery (spurious
// Reschedule/MarkErrored + a false firing_reclaimed_total). Simulates
// a task that has already been "running" an hour by backdating
// fired_at while the row is awaiting_task, then asserts ClaimDelivery
// stamps a fresh fired_at (~now) and the freshly-claimed row is NOT
// returned by ReclaimStuckFiring even against the same grace cutoff
// that would have caught the stale timestamp.
func TestClaimDeliveryRefreshesFiredAt_ExcludesFromStuckSweep(t *testing.T) {
	db := newIntegrationDB(t)
	repo := NewReminderRepository(db)
	ctx := context.Background()
	rem := firingTaskReminder(t, ctx, repo) // inserts + LeaseDue's it to 'firing'
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM dispatcher_reminders WHERE id = $1`, rem.ID)
	})

	next := time.Now().Add(24 * time.Hour).UTC()
	if err := repo.MarkTaskSpawned(ctx, rem.ID, "task_1", &next); err != nil {
		t.Fatalf("MarkTaskSpawned: %v", err)
	}
	// Simulate a task that's been running well past FiringGrace: the
	// row's fired_at (stamped at the original LeaseDue) is now an hour
	// stale by the time the task finally completes and calls back.
	old := time.Now().Add(-1 * time.Hour).UTC()
	backdateFiredAt(t, ctx, db, rem.ID, old)

	claimed, ok, err := repo.ClaimDelivery(ctx, "task_1")
	if err != nil || !ok || claimed == nil {
		t.Fatalf("ClaimDelivery: claimed=%v ok=%v err=%v", claimed, ok, err)
	}
	if claimed.FiredAt == nil || time.Since(*claimed.FiredAt) > 10*time.Second {
		t.Fatalf("ClaimDelivery should refresh fired_at to ~now; got %v", claimed.FiredAt)
	}

	// The same grace cutoff that would have caught the stale (1-hour-old)
	// fired_at must NOT catch the row now that ClaimDelivery refreshed it.
	cutoff := time.Now().Add(-15 * time.Minute)
	stuck, err := repo.ReclaimStuckFiring(ctx, cutoff, 10)
	if err != nil {
		t.Fatalf("ReclaimStuckFiring: %v", err)
	}
	for _, s := range stuck {
		if s.ID == rem.ID {
			t.Fatalf("actively-delivering row %s was falsely reclaimed by the stuck-firing sweep (stale fired_at bug)", rem.ID)
		}
	}
}

// TestReclaimStuckFiring_ExcludesAwaitingTask: an awaiting_task row is
// the long-running-task state, NOT stuck — even with a very stale
// fired_at, ReclaimStuckFiring must never return it. This is the
// invariant the design brief calls out explicitly: the sweep only
// touches 'firing', never 'awaiting_task'.
func TestReclaimStuckFiring_ExcludesAwaitingTask(t *testing.T) {
	db := newIntegrationDB(t)
	repo := NewReminderRepository(db)
	ctx := context.Background()
	rem := firingTaskReminder(t, ctx, repo)
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM dispatcher_reminders WHERE id = $1`, rem.ID)
	})

	if err := repo.MarkTaskSpawned(ctx, rem.ID, "task_stuck_awaiting", nil); err != nil {
		t.Fatalf("MarkTaskSpawned: %v", err)
	}
	got, err := repo.Get(ctx, rem.ID)
	if err != nil || got.Status != persistence.ReminderStatusAwaitingTask {
		t.Fatalf("precondition: row should be awaiting_task, got %v err=%v", got, err)
	}
	backdateFiredAt(t, ctx, db, rem.ID, time.Now().Add(-24*time.Hour).UTC())

	cutoff := time.Now().Add(-15 * time.Minute)
	stuck, err := repo.ReclaimStuckFiring(ctx, cutoff, 10)
	if err != nil {
		t.Fatalf("ReclaimStuckFiring: %v", err)
	}
	for _, s := range stuck {
		if s.ID == rem.ID {
			t.Fatalf("awaiting_task row %s must never be returned by ReclaimStuckFiring", rem.ID)
		}
	}
}
