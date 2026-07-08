package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestFailedRateByProject — the control-plane Tune detector's signal query:
// per-project FAILED vs total (COMPLETED+FAILED) terminal executions inside
// the window; CANCELLED excluded; out-of-window rows excluded.
func TestFailedRateByProject(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewExecutionRepository(db.DB)
	now := time.Now().UTC()

	mk := func(id, project string, status persistence.ExecutionStatus, age time.Duration) {
		if err := repo.Create(ctx, &persistence.Execution{
			ID: id, TaskID: "t-" + id, ProjectID: project, Status: status,
			CreatedAt: now.Add(-age),
		}); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}
	// p1 in-window: 3 FAILED + 2 COMPLETED → failed 3 / total 5.
	mk("f1", "p1", persistence.ExecutionStatusFailed, time.Minute)
	mk("f2", "p1", persistence.ExecutionStatusFailed, 2*time.Minute)
	mk("f3", "p1", persistence.ExecutionStatusFailed, 3*time.Minute)
	mk("c1", "p1", persistence.ExecutionStatusCompleted, time.Minute)
	mk("c2", "p1", persistence.ExecutionStatusCompleted, 2*time.Minute)
	// p1 noise: CANCELLED (excluded) + an old FAILED (outside the window).
	mk("x1", "p1", persistence.ExecutionStatusCancelled, time.Minute)
	mk("old", "p1", persistence.ExecutionStatusFailed, 48*time.Hour)
	// p2 in-window: 1 COMPLETED → failed 0 / total 1.
	mk("c3", "p2", persistence.ExecutionStatusCompleted, time.Minute)

	got, err := repo.FailedRateByProject(ctx, now.Add(-6*time.Hour))
	if err != nil {
		t.Fatalf("FailedRateByProject: %v", err)
	}
	if got["p1"].Failed != 3 || got["p1"].Total != 5 {
		t.Errorf("p1: want failed=3 total=5 (CANCELLED + old FAILED excluded), got %+v", got["p1"])
	}
	if got["p2"].Failed != 0 || got["p2"].Total != 1 {
		t.Errorf("p2: want failed=0 total=1, got %+v", got["p2"])
	}
}
