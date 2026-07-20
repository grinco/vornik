package reminders

// Task 14: stuck-firing reconciliation sweep (crash recovery, design §9).
// A reminder can get stuck in status='firing' after a crash: (a) between
// LeaseDue and MarkTaskSpawned (task-kind spawn interrupted), (b) between
// Send/ClaimDelivery and FinalizeDelivery (delivery interrupted), or (c)
// the pre-existing text-kind case where a failed Send leaves the row
// firing forever. Before this sweep, nothing reclaimed these — these
// tests pin sweepStuckFiring's behavior against the real stubRepo.

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
)

// TestSweepStuckFiring_RecurringReArms: a recurring row ReclaimStuckFiring
// hands back must be re-armed via Reschedule (not MarkFired, not
// MarkErrored) — the crash was in the spawn/delivery path, not the
// schedule itself, so the cron loop should just pick back up.
func TestSweepStuckFiring_RecurringReArms(t *testing.T) {
	clock := time.Date(2026, 5, 24, 16, 0, 0, 0, time.UTC) // Sunday
	repo := newStubRepo()
	repo.stuckQueue = []*persistence.Reminder{
		{
			ID: "rem_stuck_cron", Channel: "telegram",
			CronExpr: "0 9 * * 1", // every Monday 09:00
			Status:   persistence.ReminderStatusFiring,
		},
	}
	r := New(Config{
		Repo:   repo,
		Logger: zerolog.Nop(),
		Clock:  func() time.Time { return clock },
	})

	before := testutil.ToFloat64(metricFiringReclaimed)
	r.sweepStuckFiring(context.Background())

	if len(repo.rescheduled) != 1 || repo.rescheduled[0].ID != "rem_stuck_cron" {
		t.Fatalf("rescheduled = %v, want exactly [rem_stuck_cron]", repo.rescheduled)
	}
	want := time.Date(2026, 5, 25, 9, 0, 0, 0, time.UTC)
	if !repo.rescheduled[0].NextFireAt.Equal(want) {
		t.Errorf("next fire = %s, want %s", repo.rescheduled[0].NextFireAt, want)
	}
	if len(repo.fired) != 0 {
		t.Errorf("recurring reclaim must not MarkFired; got %v", repo.fired)
	}
	if _, ok := repo.errored["rem_stuck_cron"]; ok {
		t.Errorf("recurring reclaim must not MarkErrored")
	}
	if got := testutil.ToFloat64(metricFiringReclaimed) - before; got != 1 {
		t.Errorf("metricFiringReclaimed delta = %v, want 1", got)
	}
}

// TestSweepStuckFiring_RecurringPastBoundGoesTerminal mirrors finalize's
// terminal-when-bound-hit branch: a recurring row whose next cron slot
// would exceed RecurrenceUntil must terminate via MarkFired, not
// Reschedule — same rule as the happy-path finalize (design §4.2), just
// reached via the crash-recovery sweep instead.
func TestSweepStuckFiring_RecurringPastBoundGoesTerminal(t *testing.T) {
	clock := time.Date(2026, 5, 24, 16, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 24, 17, 0, 0, 0, time.UTC) // bound before next Monday
	repo := newStubRepo()
	repo.stuckQueue = []*persistence.Reminder{
		{
			ID: "rem_stuck_bounded", Channel: "telegram",
			CronExpr:        "0 9 * * 1",
			RecurrenceUntil: &until,
			Status:          persistence.ReminderStatusFiring,
		},
	}
	r := New(Config{Repo: repo, Logger: zerolog.Nop(), Clock: func() time.Time { return clock }})

	r.sweepStuckFiring(context.Background())

	if len(repo.rescheduled) != 0 {
		t.Errorf("bounded row past bound must not reschedule; got %v", repo.rescheduled)
	}
	if len(repo.fired) != 1 || repo.fired[0] != "rem_stuck_bounded" {
		t.Errorf("fired = %v, want [rem_stuck_bounded]", repo.fired)
	}
}

// TestSweepStuckFiring_OneShotMarksErrored: a one-shot stuck row gets
// MarkErrored, mirroring deliver()'s existing failed-Send branch (the
// pre-existing text-kind case design §9 calls "accepted behavior" —
// MarkErrored deliberately doesn't itself change status off 'firing').
func TestSweepStuckFiring_OneShotMarksErrored(t *testing.T) {
	repo := newStubRepo()
	repo.stuckQueue = []*persistence.Reminder{
		{ID: "rem_stuck_oneshot", Channel: "telegram", Status: persistence.ReminderStatusFiring},
	}
	r := New(Config{Repo: repo, Logger: zerolog.Nop()})

	before := testutil.ToFloat64(metricFiringReclaimed)
	r.sweepStuckFiring(context.Background())

	msg, ok := repo.errored["rem_stuck_oneshot"]
	if !ok || msg == "" {
		t.Fatalf("one-shot stuck row should be marked errored with a non-empty message; got %q ok=%v", msg, ok)
	}
	if len(repo.rescheduled) != 0 || len(repo.fired) != 0 {
		t.Errorf("one-shot reclaim must not reschedule or MarkFired; rescheduled=%v fired=%v", repo.rescheduled, repo.fired)
	}
	if got := testutil.ToFloat64(metricFiringReclaimed) - before; got != 1 {
		t.Errorf("metricFiringReclaimed delta = %v, want 1", got)
	}
}

// TestSweepStuckFiring_UsesFiringGraceCutoffAndCadence pins two things at
// once: (1) ReclaimStuckFiring is called with clock() - cfg.FiringGrace as
// the cutoff, not some other value, and (2) the sweep only runs on the
// gated (every sweepEveryNTicks) cadence from tickOnce, not every tick —
// ticks 1..N-1 must leave the stuck-firing table untouched.
func TestSweepStuckFiring_UsesFiringGraceCutoffAndCadence(t *testing.T) {
	clock := time.Date(2026, 5, 24, 16, 0, 0, 0, time.UTC)
	repo := newStubRepo()
	grace := 20 * time.Minute
	r := New(Config{
		Repo:        repo,
		Logger:      zerolog.Nop(),
		Clock:       func() time.Time { return clock },
		FiringGrace: grace,
	})

	// Ticks 1..N-1 (< sweepEveryNTicks) must NOT invoke the sweep.
	for i := 0; i < sweepEveryNTicks-1; i++ {
		r.tickOnce(context.Background())
	}
	if repo.reclaimCalls != 0 {
		t.Fatalf("reclaimCalls = %d before the Nth tick, want 0", repo.reclaimCalls)
	}

	r.tickOnce(context.Background()) // Nth tick: sweep gate fires
	if repo.reclaimCalls != 1 {
		t.Fatalf("reclaimCalls = %d on the Nth tick, want 1", repo.reclaimCalls)
	}
	want := clock.Add(-grace)
	if !repo.reclaimOlderThan.Equal(want) {
		t.Errorf("ReclaimStuckFiring cutoff = %s, want %s (clock - FiringGrace)", repo.reclaimOlderThan, want)
	}
}
