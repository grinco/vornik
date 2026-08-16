package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// Regression, operator-reported 2026-08-16: the dashboard's "failed in the last
// 24h" banner stopped appearing entirely, while four tasks had failed that day
// and were still in FAILED.
//
// 7e7b2cef (2026-08-15) correctly stopped the card counting by updated_at — a
// BEFORE UPDATE trigger stamps that on ANY row change, so a lease sweep made a
// 90-day-old failure look like it had just happened. It added tasks.failed_at,
// stamped it in UpdateStatus and TransitionConditional, and switched the count
// to `failed_at IS NOT NULL AND failed_at >= ?`.
//
// It missed the path the executor actually uses. handleFailure's terminal
// branch calls taskRepo.Update — the full-row write, workflow.go:3394 — which
// never touched failed_at. So every real task failure landed with failed_at
// NULL, the count query correctly matched nothing, and the banner went silent.
// Measured on the production ledger: 34 of 34 FAILED rows had failed_at NULL,
// four of them from that same day.
//
// The commit's own comment predicted it: "FAILED is reached from several paths
// (executor, lease release, sweeps) and an opt-in flag would be missed by one
// of them."
//
// These assert through CountRecentFailuresByProject rather than reading the
// column, so they test what the dashboard actually asks.
func TestUpdate_StampsFailedAtOnTheWayIntoFailure(t *testing.T) {
	repo, ctx := newFailedAtRepo(t)
	task := seedRunningTask(ctx, t, repo, "t-fail-1")

	// The executor's terminal-failure path: mutate the struct, full-row Update.
	task.Status = persistence.TaskStatusFailed
	if err := repo.Update(ctx, task); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if n := failedWithinWindow(ctx, t, repo, time.Now().Add(-time.Hour)); n != 1 {
		t.Fatalf("recent-failure count = %d, want 1 — a full-row Update into FAILED left failed_at NULL, so the dashboard banner shows nothing", n)
	}
}

// A task that fails, is retried and succeeds must not keep a stale failure
// timestamp, or the card counts a failure that no longer exists.
func TestUpdate_ClearsFailedAtWhenLeavingFailure(t *testing.T) {
	repo, ctx := newFailedAtRepo(t)
	task := seedRunningTask(ctx, t, repo, "t-fail-2")

	task.Status = persistence.TaskStatusFailed
	if err := repo.Update(ctx, task); err != nil {
		t.Fatalf("Update to FAILED: %v", err)
	}
	if n := failedWithinWindow(ctx, t, repo, time.Now().Add(-time.Hour)); n != 1 {
		t.Fatalf("precondition: want 1 recent failure, got %d", n)
	}

	task.Status = persistence.TaskStatusCompleted
	if err := repo.Update(ctx, task); err != nil {
		t.Fatalf("Update to COMPLETED: %v", err)
	}
	if n := failedWithinWindow(ctx, t, repo, time.Now().Add(-time.Hour)); n != 0 {
		t.Errorf("recent-failure count = %d after recovery, want 0 — a resolved failure is still being counted", n)
	}
}

// Repeated Updates while still FAILED must not push the timestamp forward: a
// lease sweep touching the row would otherwise make an old failure look fresh,
// which is precisely the bug 7e7b2cef set out to fix.
func TestUpdate_DoesNotRefreshFailedAtWhileStillFailed(t *testing.T) {
	repo, ctx := newFailedAtRepo(t)
	task := seedRunningTask(ctx, t, repo, "t-fail-3")

	task.Status = persistence.TaskStatusFailed
	if err := repo.Update(ctx, task); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Let real time pass, then touch the row again while still FAILED.
	time.Sleep(1100 * time.Millisecond)
	boundary := time.Now().Add(-500 * time.Millisecond)

	msg := "still failed, row touched again"
	task.LastError = &msg
	if err := repo.Update(ctx, task); err != nil {
		t.Fatalf("second Update: %v", err)
	}

	// The original failure predates the boundary. If the second Update had
	// re-stamped failed_at, it would now fall inside this window and count.
	if n := failedWithinWindow(ctx, t, repo, boundary); n != 0 {
		t.Errorf("count within a window that starts AFTER the failure = %d, want 0 — failed_at was re-stamped by a row touch, reintroducing the updated_at bug", n)
	}
	// It must still be counted in a window that genuinely contains it.
	if n := failedWithinWindow(ctx, t, repo, time.Now().Add(-time.Hour)); n != 1 {
		t.Errorf("count over the last hour = %d, want 1 — the failure was lost entirely", n)
	}
}

// --- helpers -------------------------------------------------------------

func newFailedAtRepo(t *testing.T) (*sqlite.TaskRepository, context.Context) {
	t.Helper()
	db := newTestDB(t)
	return sqlite.NewTaskRepository(db.DB), context.Background()
}

func seedRunningTask(ctx context.Context, t *testing.T, repo *sqlite.TaskRepository, id string) *persistence.Task {
	t.Helper()
	task := &persistence.Task{
		ID:        id,
		ProjectID: "p1",
		Status:    persistence.TaskStatusRunning,
		CreatedAt: time.Now().UTC(),
	}
	if err := repo.Create(ctx, task); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return task
}

func failedWithinWindow(ctx context.Context, t *testing.T, repo *sqlite.TaskRepository, since time.Time) int {
	t.Helper()
	got, err := repo.CountRecentFailuresByProject(ctx, nil, since)
	if err != nil {
		t.Fatalf("CountRecentFailuresByProject: %v", err)
	}
	return got["p1"]
}
