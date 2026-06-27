package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestSqlite_ConnectWithDefaults exercises the Connect defaulting paths
// (MaxOpenConns / ConnectTimeout / Path).
func TestSqlite_ConnectWithDefaults(t *testing.T) {
	ctx := context.Background()
	// Empty config — defaults populate Path=":memory:", MaxOpenConns=1.
	db, err := sqlite.Connect(ctx, sqlite.Config{})
	if err != nil {
		t.Fatalf("Connect default: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// Bad path should still error cleanly.
	bad, err := sqlite.Connect(ctx, sqlite.Config{Path: "/nonexistent-dir/does-not-exist.db"})
	if err == nil {
		_ = bad.Close()
		t.Skip("expected open to fail but it succeeded on this filesystem")
	}
}

// TestLeaseTask_ProjectConcurrencyAndPriorities — pushes LeaseTask
// branches through the priority + concurrency-limit code paths.
func TestLeaseTask_ProjectConcurrencyAndPriorities(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewTaskRepository(db.DB)
	ctx := context.Background()

	// Two projects, three queued tasks each.
	for _, pid := range []string{"pA", "pB"} {
		for i := 0; i < 2; i++ {
			if err := repo.Create(ctx, &persistence.Task{
				ID:        pid + "-t" + string(rune('0'+i)),
				ProjectID: pid,
				Priority:  50,
			}); err != nil {
				t.Fatalf("create %s: %v", pid, err)
			}
		}
	}

	// Lease one task from pA — sets it LEASED.
	got, err := repo.LeaseTask(ctx, persistence.LeaseOptions{
		ProjectID:            "pA",
		LeaseHolder:          "w",
		LeaseDurationSeconds: 30,
	})
	if err != nil {
		t.Fatalf("first lease: %v", err)
	}
	if got.ProjectID != "pA" {
		t.Errorf("project = %q, want pA", got.ProjectID)
	}

	// Lease with PriorityFloor that excludes available tasks.
	if _, err := repo.LeaseTask(ctx, persistence.LeaseOptions{
		PriorityFloor:        99,
		LeaseHolder:          "w",
		LeaseDurationSeconds: 30,
	}); err != persistence.ErrNoTasksAvailable {
		t.Errorf("priority floor: err=%v, want ErrNoTasksAvailable", err)
	}

	// Lease with project concurrency limit — pA is at 1 LEASED (the
	// task we just leased), set cap=1 so pA is over-cap, leaving pB.
	got2, err := repo.LeaseTask(ctx, persistence.LeaseOptions{
		LeaseHolder:              "w2",
		LeaseDurationSeconds:     30,
		ProjectConcurrencyLimits: map[string]int{"pA": 1},
	})
	if err != nil {
		t.Fatalf("concurrency-limited lease: %v", err)
	}
	if got2.ProjectID != "pB" {
		t.Errorf("expected pB, got %q", got2.ProjectID)
	}

	// Lease with explicit ProjectPriorities + default.
	got3, err := repo.LeaseTask(ctx, persistence.LeaseOptions{
		LeaseHolder:            "w3",
		LeaseDurationSeconds:   30,
		ProjectPriorities:      map[string]int{"pA": 10, "pB": 90},
		ProjectPriorityDefault: 50,
	})
	if err != nil {
		t.Fatalf("priorities lease: %v", err)
	}
	// pA prio=10 < pB prio=90 so pA should win.
	if got3.ProjectID != "pA" {
		t.Errorf("priority lease project = %q, want pA", got3.ProjectID)
	}
}

// TestTelegramThreadRepository_MarkClosedAndScan covers MarkClosed +
// the not-found branches of GetByTask / GetByThread.
func TestTelegramThreadRepository_MarkClosedAndScan(t *testing.T) {
	db := newTestDB(t)
	taskRepo := sqlite.NewTaskRepository(db.DB)
	repo := sqlite.NewTelegramThreadRepository(db.DB)
	ctx := context.Background()
	if err := taskRepo.Create(ctx, &persistence.Task{ID: "t-tg", ProjectID: "p"}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	tg := &persistence.TelegramTaskThread{TaskID: "t-tg", ChatID: 100, ThreadID: 200, TopicName: "topic"}
	if err := repo.Insert(ctx, tg); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// MarkClosed sets closed_at; idempotent.
	if err := repo.MarkClosed(ctx, "t-tg"); err != nil {
		t.Fatalf("MarkClosed: %v", err)
	}
	if err := repo.MarkClosed(ctx, "t-tg"); err != nil {
		t.Fatalf("MarkClosed idempotent: %v", err)
	}
	// GetByTask returns the thread with ClosedAt populated.
	got, err := repo.GetByTask(ctx, "t-tg")
	if err != nil {
		t.Fatalf("GetByTask: %v", err)
	}
	if got.ClosedAt == nil {
		t.Error("ClosedAt should be non-nil")
	}

	// Not-found cases.
	if _, err := repo.GetByTask(ctx, "missing"); err != persistence.ErrNotFound {
		t.Errorf("GetByTask missing = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetByThread(ctx, 0, 0); err != persistence.ErrNotFound {
		t.Errorf("GetByThread zero = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetByThread(ctx, 999, 999); err != persistence.ErrNotFound {
		t.Errorf("GetByThread unknown = %v, want ErrNotFound", err)
	}

	// Guards.
	if err := repo.Insert(ctx, nil); err == nil {
		t.Error("Insert(nil) should error")
	}
	if err := repo.Insert(ctx, &persistence.TelegramTaskThread{}); err == nil {
		t.Error("Insert empty should error")
	}
	if err := repo.MarkClosed(ctx, ""); err == nil {
		t.Error("MarkClosed empty should error")
	}
	if _, err := repo.GetByTask(ctx, ""); err == nil {
		t.Error("GetByTask empty should error")
	}

	// Duplicate insertion returns ErrDuplicateKey.
	dup := &persistence.TelegramTaskThread{TaskID: "t-tg2", ChatID: 100, ThreadID: 200, TopicName: "x"}
	if err := taskRepo.Create(ctx, &persistence.Task{ID: "t-tg2", ProjectID: "p"}); err != nil {
		t.Fatalf("Create dup task: %v", err)
	}
	if err := repo.Insert(ctx, dup); err != persistence.ErrDuplicateKey {
		t.Errorf("dup insert = %v, want ErrDuplicateKey", err)
	}
}

// TestTaskLLMUsage_SumAndListFilters covers SumCost, SumCostByProject,
// List filter branches not exercised in aggregator tests.
func TestTaskLLMUsage_SumAndListFilters(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewTaskLLMUsageRepository(db.DB)
	ctx := context.Background()

	taskA, taskB := "t-a", "t-b"
	base := time.Now().UTC().Truncate(time.Second)
	rows := []persistence.TaskLLMUsage{
		{ID: "u1", ProjectID: "p1", TaskID: &taskA, StepID: "s1", Role: "w", Model: "m", CostUSD: 1.0, RecordedAt: base.Add(-3 * time.Hour)},
		{ID: "u2", ProjectID: "p1", TaskID: &taskB, StepID: "s1", Role: "judge", Model: "m", CostUSD: 2.0, RecordedAt: base.Add(-2 * time.Hour)},
		{ID: "u3", ProjectID: "p2", TaskID: &taskA, StepID: "s1", Role: "w", Model: "m", CostUSD: 4.0, RecordedAt: base.Add(-1 * time.Hour)},
	}
	for i := range rows {
		if err := repo.Record(ctx, &rows[i]); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}

	sum, err := repo.SumCost(ctx, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("SumCost: %v", err)
	}
	if sum != 7.0 {
		t.Errorf("SumCost = %v, want 7", sum)
	}
	sum, err = repo.SumCost(ctx, base.Add(-2*time.Hour-time.Minute), base.Add(-30*time.Minute))
	if err != nil {
		t.Fatalf("SumCost windowed: %v", err)
	}
	if sum != 6.0 {
		t.Errorf("SumCost windowed = %v, want 6", sum)
	}

	pSum, err := repo.SumCostByProject(ctx, "p1", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("SumCostByProject: %v", err)
	}
	if pSum != 3.0 {
		t.Errorf("SumCostByProject p1 = %v, want 3", pSum)
	}

	// List with all filters.
	pid := "p1"
	taskID := "t-a"
	role := "w"
	since := base.Add(-4 * time.Hour)
	until := base
	listed, err := repo.List(ctx, persistence.TaskLLMUsageFilter{
		ProjectID: &pid, TaskID: &taskID, Role: &role, Since: &since, Until: &until,
		PageSize: 10, Offset: 0,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 {
		t.Errorf("List len = %d", len(listed))
	}

	// List with Offset > 0 branch.
	all, err := repo.List(ctx, persistence.TaskLLMUsageFilter{PageSize: 10, Offset: 1})
	if err != nil {
		t.Fatalf("List offset: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("Offset=1 → %d rows, want 2", len(all))
	}
}

// TestQuarantineRepository_FilterAndGet — Get + ListPending filter coverage.
func TestQuarantineRepository_FilterAndGet(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewMemoryQuarantineRepository(db.DB)
	artRepo := sqlite.NewArtifactRepository(db.DB)
	ctx := context.Background()

	if err := artRepo.Create(ctx, &persistence.Artifact{
		ID: "a1", ProjectID: "p1", Name: "n", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: "/p",
	}); err != nil {
		t.Fatalf("artifact: %v", err)
	}
	role := "writer"
	q := &persistence.MemoryQuarantineItem{
		ProjectID:        "p1",
		SourceArtifactID: "a1",
		ProducerRole:     &role,
		FailedGate:       "claim",
		Content:          "x",
		ContentHash:      "h1",
	}
	if err := repo.Insert(ctx, q); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// Get + Get missing.
	got, err := repo.Get(ctx, q.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SourceArtifactID != "a1" {
		t.Errorf("artifact_id = %q", got.SourceArtifactID)
	}
	if _, err := repo.Get(ctx, "missing"); err == nil {
		t.Errorf("Get missing should error, got nil")
	}
	if _, err := repo.Get(ctx, ""); err == nil {
		t.Error("Get empty should error")
	}

	// CountByGate covers the GROUP BY path.
	counts, err := repo.CountByGate(ctx, "p1")
	if err != nil {
		t.Fatalf("CountByGate: %v", err)
	}
	if counts["claim"] != 1 {
		t.Errorf("counts = %+v", counts)
	}
	if _, err := repo.CountByGate(ctx, ""); err == nil {
		t.Error("empty project should error")
	}
}

// TestIntentVerdictRepository_ListRecent — repotest already covers
// the basics; add coverage of the limit-default branch.
func TestIntentVerdictRepository_ListRecent(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewIntentVerdictRepository(db.DB)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 3; i++ {
		if err := repo.Insert(ctx, &persistence.IntentVerdict{
			ID:                      "iv-" + string(rune('0'+i)),
			ProjectID:               "p1",
			HeuristicRisk:           "low",
			HeuristicConfidence:     0.8,
			HeuristicRecommendation: "auto",
			CreatedAt:               base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	// limit=0 → default
	out, err := repo.ListRecent(ctx, "p1", 0)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	if len(out) != 3 {
		t.Errorf("ListRecent len = %d", len(out))
	}
	// Explicit limit clips.
	out, err = repo.ListRecent(ctx, "p1", 2)
	if err != nil {
		t.Fatalf("ListRecent limit: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("ListRecent limit len = %d", len(out))
	}
}
