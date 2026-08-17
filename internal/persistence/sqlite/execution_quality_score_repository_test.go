package sqlite_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

func TestExecutionQualityScoreRepository_CompletenessAndProjectScope(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	for _, task := range []struct{ id, project string }{{"t1", "p1"}, {"t2", "p1"}, {"t3", "p2"}, {"t4", "p1"}} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO tasks (id, project_id, created_at, updated_at) VALUES (?, ?, ?, ?)`,
			task.id, task.project, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
			t.Fatalf("seed task %s: %v", task.id, err)
		}
	}
	execRepo := sqlite.NewExecutionRepository(db.DB)
	for _, exec := range []*persistence.Execution{
		{ID: "e1", TaskID: "t1", ProjectID: "p1", WorkflowID: "dev-pipeline", WorkflowRevision: "r1", Status: persistence.ExecutionStatusCompleted, CreatedAt: now},
		{ID: "e2", TaskID: "t2", ProjectID: "p1", WorkflowID: "dev-pipeline", WorkflowRevision: "r1", Status: persistence.ExecutionStatusFailed, CreatedAt: now.Add(time.Second)},
		{ID: "e3", TaskID: "t3", ProjectID: "p2", WorkflowID: "simple", WorkflowRevision: "r2", Status: persistence.ExecutionStatusCancelled, CreatedAt: now.Add(2 * time.Second)},
		{ID: "e4", TaskID: "t4", ProjectID: "p1", WorkflowID: "dev-pipeline", WorkflowRevision: "r1", Status: persistence.ExecutionStatusRunning, CreatedAt: now.Add(3 * time.Second)},
	} {
		if err := execRepo.Create(ctx, exec); err != nil {
			t.Fatalf("seed execution %s: %v", exec.ID, err)
		}
	}

	repo := sqlite.NewExecutionQualityScoreRepository(db.DB)
	pending, err := repo.ListPendingTerminal(ctx, 10)
	if err != nil {
		t.Fatalf("ListPendingTerminal: %v", err)
	}
	if len(pending) != 3 || pending[0].ID != "e1" || pending[1].ID != "e2" || pending[2].ID != "e3" {
		t.Fatalf("pending terminal executions = %#v, want e1/e2/e3 oldest-first", pending)
	}
	stats, err := repo.PendingTerminalStats(ctx, []string{"p1"})
	if err != nil || stats.Count != 2 || stats.OldestAt == nil || !stats.OldestAt.Equal(now) {
		t.Fatalf("p1 pending stats = %+v, %v", stats, err)
	}

	score := &persistence.ExecutionQualityScore{
		ProjectID: "p1", TaskID: "t1", ExecutionID: "e1", WorkflowID: "dev-pipeline",
		WorkflowRevision: "r1", ScorerVersion: "1", ScoringPolicySHA: "policy-a",
		Kind: "pinned_case_validation", Status: "scored", Score: floatPtr(0.5),
		PassedCaseCount: 1, PinnedCaseCount: 2, CaseEvidence: json.RawMessage(`[{"id":"a"}]`),
		RecordedAt: now.Add(time.Minute),
	}
	if err := repo.Upsert(ctx, score); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := repo.GetByExecution(ctx, "e1")
	if err != nil {
		t.Fatalf("GetByExecution: %v", err)
	}
	if got.Score == nil || *got.Score != 0.5 || got.PassedCaseCount != 1 || got.ProjectID != "p1" {
		t.Fatalf("score round-trip = %+v", got)
	}

	// Idempotent fast-path/reconciler collision updates the one row.
	score.Score = floatPtr(1)
	score.PassedCaseCount = 2
	if err := repo.Upsert(ctx, score); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	var rows int
	if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_quality_scores WHERE execution_id = ?`, "e1").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("idempotent upsert left %d rows, want 1", rows)
	}

	pending, err = repo.ListPendingTerminal(ctx, 10)
	if err != nil || len(pending) != 2 || pending[0].ID != "e2" || pending[1].ID != "e3" {
		t.Fatalf("pending after score = %#v, %v", pending, err)
	}
	stats, err = repo.PendingTerminalStats(ctx, []string{"p1"})
	if err != nil || stats.Count != 1 || stats.OldestAt == nil || !stats.OldestAt.Equal(now.Add(time.Second)) {
		t.Fatalf("p1 pending stats after score = %+v, %v", stats, err)
	}

	listed, err := repo.List(ctx, persistence.ExecutionQualityScoreFilter{ProjectIDs: []string{"p1"}, PageSize: 10})
	if err != nil || len(listed) != 1 || listed[0].ExecutionID != "e1" {
		t.Fatalf("project-scoped List = %#v, %v", listed, err)
	}

	// Project/task identity comes from the execution row. A caller cannot
	// attach e1's score to p2 even if it knows the globally unique ID.
	wrong := *score
	wrong.ProjectID = "p2"
	if err := repo.Upsert(ctx, &wrong); err == nil {
		t.Fatal("cross-project score upsert must be rejected")
	}
}

func floatPtr(v float64) *float64 { return &v }
