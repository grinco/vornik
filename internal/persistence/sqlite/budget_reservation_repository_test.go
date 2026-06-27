package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestBudgetReservationRepository_ReserveSettleSum walks the core ledger
// lifecycle: a reservation under the cap is inserted (and shows in the
// unsettled sum), one over the cap is refused (no row), and settling drops
// the unsettled sum back to zero.
func TestBudgetReservationRepository_ReserveSettleSum(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewBudgetReservationRepository(db.DB)
	ctx := context.Background()
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)

	// Under the $100 daily cap: committed 20 + reserved 0 + estimate 5 = 25.
	res, err := repo.Reserve(ctx, persistence.ReserveRequest{
		ProjectID: "p1", TaskID: "task-1", EstimateUSD: 5,
		DailyCommittedUSD: 20, DailyHardUSD: 100, Now: now,
	})
	if err != nil {
		t.Fatalf("reserve 1: %v", err)
	}
	if !res.Reserved || res.Blocked {
		t.Fatalf("first reserve should succeed: %+v", res)
	}

	sum, err := repo.UnsettledSumByProject(ctx, "p1")
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if sum != 5 {
		t.Fatalf("unsettled sum = %v want 5", sum)
	}

	// Over the cap now: committed 90 + reserved 5 + estimate 10 = 105 > 100.
	res2, err := repo.Reserve(ctx, persistence.ReserveRequest{
		ProjectID: "p1", TaskID: "task-2", EstimateUSD: 10,
		DailyCommittedUSD: 90, DailyHardUSD: 100, Now: now,
	})
	if err != nil {
		t.Fatalf("reserve 2: %v", err)
	}
	if res2.Reserved || !res2.Blocked || res2.Period != "daily" {
		t.Fatalf("second reserve should be blocked on daily: %+v", res2)
	}
	// Blocked reserve must NOT have inserted a row.
	if sum, _ := repo.UnsettledSumByProject(ctx, "p1"); sum != 5 {
		t.Fatalf("blocked reserve must not insert: sum=%v want 5", sum)
	}

	// Settle task-1 → unsettled sum back to 0.
	n, err := repo.SettleByTask(ctx, "task-1", now)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if n != 1 {
		t.Fatalf("settle rows = %d want 1", n)
	}
	if sum, _ := repo.UnsettledSumByProject(ctx, "p1"); sum != 0 {
		t.Fatalf("post-settle sum = %v want 0", sum)
	}
	// Idempotent: a second settle affects nothing.
	if n, _ := repo.SettleByTask(ctx, "task-1", now); n != 0 {
		t.Fatalf("second settle rows = %d want 0", n)
	}
}

// TestBudgetReservationRepository_Sweep covers both sweep paths: a stale
// unsettled reservation (older than the cutoff) and one whose task already
// went terminal are both settled; a fresh reservation for a running task is
// left alone.
func TestBudgetReservationRepository_Sweep(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewBudgetReservationRepository(db.DB)
	ctx := context.Background()
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)

	// Stale reservation — reserved 10h ago, no task row.
	if _, err := repo.Reserve(ctx, persistence.ReserveRequest{
		ProjectID: "p1", TaskID: "stale-task", EstimateUSD: 1,
		DailyHardUSD: 1000, Now: now.Add(-10 * time.Hour),
	}); err != nil {
		t.Fatalf("reserve stale: %v", err)
	}

	// Terminal-task reservation — fresh, but its task is COMPLETED.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO tasks (id, project_id, status, created_at, updated_at) VALUES (?, ?, 'COMPLETED', ?, ?)`,
		"done-task", "p1", sqliteTimeStr(now), sqliteTimeStr(now),
	); err != nil {
		t.Fatalf("insert terminal task: %v", err)
	}
	if _, err := repo.Reserve(ctx, persistence.ReserveRequest{
		ProjectID: "p1", TaskID: "done-task", EstimateUSD: 2,
		DailyHardUSD: 1000, Now: now,
	}); err != nil {
		t.Fatalf("reserve terminal: %v", err)
	}

	// Fresh reservation for a still-running task — must survive the sweep.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO tasks (id, project_id, status, created_at, updated_at) VALUES (?, ?, 'RUNNING', ?, ?)`,
		"live-task", "p1", sqliteTimeStr(now), sqliteTimeStr(now),
	); err != nil {
		t.Fatalf("insert live task: %v", err)
	}
	if _, err := repo.Reserve(ctx, persistence.ReserveRequest{
		ProjectID: "p1", TaskID: "live-task", EstimateUSD: 4,
		DailyHardUSD: 1000, Now: now,
	}); err != nil {
		t.Fatalf("reserve live: %v", err)
	}

	// Sweep: stale cutoff = 6h ago. Stale (10h old) + terminal-task settle;
	// live one (4) survives.
	settled, err := repo.SweepTerminalAndStale(ctx, now.Add(-6*time.Hour), now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if settled != 2 {
		t.Fatalf("swept %d want 2 (stale + terminal)", settled)
	}
	if sum, _ := repo.UnsettledSumByProject(ctx, "p1"); sum != 4 {
		t.Fatalf("post-sweep unsettled sum = %v want 4 (live task only)", sum)
	}
}

// sqliteTimeStr mirrors the repo's timestamp encoding for the raw task
// inserts above (RFC3339Nano UTC).
func sqliteTimeStr(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
