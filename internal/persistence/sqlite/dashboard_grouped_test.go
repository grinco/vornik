package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestCountRecentFailuresByProject — grouped recent-failure counts (E2, audit
// 2026-07-03) return one entry per project with FAILED tasks in the window.
func TestCountRecentFailuresByProject(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewTaskRepository(db.DB)
	now := time.Now().UTC()

	// pa: 2 failed; pb: 1 failed; pc: 1 queued (should not count).
	seed := []struct {
		id, project string
		status      persistence.TaskStatus
	}{
		{"t1", "pa", persistence.TaskStatusFailed},
		{"t2", "pa", persistence.TaskStatusFailed},
		{"t3", "pb", persistence.TaskStatusFailed},
		{"t4", "pc", persistence.TaskStatusQueued},
	}
	for _, s := range seed {
		if err := repo.Create(ctx, &persistence.Task{ID: s.id, ProjectID: s.project, Status: s.status, CreatedAt: now}); err != nil {
			t.Fatalf("Create %s: %v", s.id, err)
		}
		// Create may not set updated_at into the window for a FAILED row;
		// force the terminal status so updated_at lands now.
		if s.status == persistence.TaskStatusFailed {
			if err := repo.UpdateStatus(ctx, s.id, persistence.TaskStatusFailed); err != nil {
				t.Fatalf("UpdateStatus %s: %v", s.id, err)
			}
		}
	}

	got, err := repo.CountRecentFailuresByProject(ctx, nil, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("CountRecentFailuresByProject: %v", err)
	}
	if got["pa"] != 2 {
		t.Errorf("pa: want 2, got %d", got["pa"])
	}
	if got["pb"] != 1 {
		t.Errorf("pb: want 1, got %d", got["pb"])
	}
	if _, ok := got["pc"]; ok {
		t.Errorf("pc has no failures; should be absent, got %d", got["pc"])
	}
}

// TestAutonomyLatestByProject — returns the newest eval per project in one
// query (E2, audit 2026-07-03).
func TestAutonomyLatestByProject(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewAutonomyEvaluationRepository(db.DB)
	base := time.Now().UTC()

	rows := []*persistence.AutonomyEvaluation{
		{ID: "a1", ProjectID: "pa", Outcome: "CREATED", CreatedAt: base.Add(-2 * time.Minute)},
		{ID: "a2", ProjectID: "pa", Outcome: "NO_ACTION", CreatedAt: base.Add(-1 * time.Minute)}, // newest for pa
		{ID: "b1", ProjectID: "pb", Outcome: "REJECTED", CreatedAt: base.Add(-3 * time.Minute)},
	}
	for _, e := range rows {
		if err := repo.Record(ctx, e); err != nil {
			t.Fatalf("Record %s: %v", e.ID, err)
		}
	}

	latest, err := repo.LatestByProject(ctx)
	if err != nil {
		t.Fatalf("LatestByProject: %v", err)
	}
	if latest["pa"] == nil || latest["pa"].ID != "a2" {
		t.Errorf("pa latest: want a2, got %+v", latest["pa"])
	}
	if latest["pb"] == nil || latest["pb"].ID != "b1" {
		t.Errorf("pb latest: want b1, got %+v", latest["pb"])
	}
}
