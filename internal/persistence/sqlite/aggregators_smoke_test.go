package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// Smoke tests for the previously-unimplemented dashboard aggregators
// after the backfill. Each test seeds a handful of rows + asserts
// the SQL returns the expected groupings — same semantics as the
// Postgres-side per-method tests.

func TestTaskLLMUsage_AggregateByProject(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewTaskLLMUsageRepository(db.DB)
	taskA, taskB := "t-a", "t-b"
	rows := []persistence.TaskLLMUsage{
		{ID: "u1", ProjectID: "p-1", TaskID: &taskA, StepID: "s1", Role: "w", Model: "m", PromptTokens: 100, CompletionTokens: 50, CostUSD: 0.01},
		{ID: "u2", ProjectID: "p-1", TaskID: &taskA, StepID: "s2", Role: "w", Model: "m", PromptTokens: 200, CompletionTokens: 80, CostUSD: 0.02},
		{ID: "u3", ProjectID: "p-1", TaskID: &taskB, StepID: "s1", Role: "w", Model: "m", PromptTokens: 100, CompletionTokens: 40, CostUSD: 0.015},
		{ID: "u4", ProjectID: "p-2", TaskID: &taskA, StepID: "s1", Role: "w", Model: "m", PromptTokens: 50, CompletionTokens: 20, CostUSD: 0.005},
	}
	for i := range rows {
		if err := repo.Record(ctx, &rows[i]); err != nil {
			t.Fatalf("Record %s: %v", rows[i].ID, err)
		}
	}
	out, err := repo.AggregateByProject(ctx, time.Time{}, time.Time{}, 10)
	if err != nil {
		t.Fatalf("AggregateByProject: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 projects, got %d", len(out))
	}
	// p-1 has higher cost — comes first.
	if out[0].ProjectID != "p-1" {
		t.Errorf("rows not cost-sorted: %v", out)
	}
	if out[0].TaskCount != 2 {
		t.Errorf("p-1 TaskCount = %d, want 2 (distinct task_ids)", out[0].TaskCount)
	}
}

// TestTaskLLMUsage_AggregatesPropagateCacheTokens — aggregation queries
// MUST SUM cache_creation_tokens + cache_read_tokens so the spend
// dashboard's cache hit ratio + savings tile have non-zero inputs. The
// schema columns exist; this pins that the SQL actually reads them.
func TestTaskLLMUsage_AggregatesPropagateCacheTokens(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewTaskLLMUsageRepository(db.DB)

	taskA := "t-cache"
	rows := []persistence.TaskLLMUsage{
		{ID: "ca-1", ProjectID: "p-cache", TaskID: &taskA, StepID: "s1", Role: "coder", Model: "sonnet-4-6",
			PromptTokens: 1000, CompletionTokens: 500, CostUSD: 0.10,
			CacheCreationTokens: 200, CacheReadTokens: 800, Source: "workflow_step"},
		{ID: "ca-2", ProjectID: "p-cache", TaskID: &taskA, StepID: "s2", Role: "coder", Model: "sonnet-4-6",
			PromptTokens: 500, CompletionTokens: 300, CostUSD: 0.05,
			CacheCreationTokens: 100, CacheReadTokens: 1200, Source: "workflow_step"},
		{ID: "ca-3", ProjectID: "p-cache", TaskID: &taskA, StepID: "s3", Role: "reviewer", Model: "haiku-4-5",
			PromptTokens: 100, CompletionTokens: 50, CostUSD: 0.001,
			Source: "dispatcher"},
	}
	for i := range rows {
		if err := repo.Record(ctx, &rows[i]); err != nil {
			t.Fatalf("Record %s: %v", rows[i].ID, err)
		}
	}

	roleModel, err := repo.AggregateByRoleModel(ctx, time.Time{}, time.Time{}, 10, "p-cache")
	if err != nil {
		t.Fatalf("AggregateByRoleModel: %v", err)
	}
	var coderRow *persistence.RoleModelSpend
	for i := range roleModel {
		if roleModel[i].Role == "coder" {
			coderRow = &roleModel[i]
		}
	}
	if coderRow == nil {
		t.Fatalf("AggregateByRoleModel missing coder row: %+v", roleModel)
	}
	if coderRow.CacheCreationTokens != 300 {
		t.Errorf("coder CacheCreationTokens = %d, want 300", coderRow.CacheCreationTokens)
	}
	if coderRow.CacheReadTokens != 2000 {
		t.Errorf("coder CacheReadTokens = %d, want 2000", coderRow.CacheReadTokens)
	}

	bySource, err := repo.AggregateBySource(ctx, time.Time{}, time.Time{}, "p-cache")
	if err != nil {
		t.Fatalf("AggregateBySource: %v", err)
	}
	var workflowRow *persistence.SourceSpend
	for i := range bySource {
		if bySource[i].Source == "workflow_step" {
			workflowRow = &bySource[i]
		}
	}
	if workflowRow == nil {
		t.Fatalf("AggregateBySource missing workflow_step: %+v", bySource)
	}
	if workflowRow.CacheCreationTokens != 300 {
		t.Errorf("workflow_step CacheCreationTokens = %d, want 300", workflowRow.CacheCreationTokens)
	}
	if workflowRow.CacheReadTokens != 2000 {
		t.Errorf("workflow_step CacheReadTokens = %d, want 2000", workflowRow.CacheReadTokens)
	}

	byProject, err := repo.AggregateByProject(ctx, time.Time{}, time.Time{}, 10)
	if err != nil {
		t.Fatalf("AggregateByProject: %v", err)
	}
	var projRow *persistence.ProjectSpend
	for i := range byProject {
		if byProject[i].ProjectID == "p-cache" {
			projRow = &byProject[i]
		}
	}
	if projRow == nil {
		t.Fatalf("AggregateByProject missing p-cache: %+v", byProject)
	}
	if projRow.CacheCreationTokens != 300 {
		t.Errorf("p-cache CacheCreationTokens = %d, want 300", projRow.CacheCreationTokens)
	}
	if projRow.CacheReadTokens != 2000 {
		t.Errorf("p-cache CacheReadTokens = %d, want 2000", projRow.CacheReadTokens)
	}
}

func TestTaskLLMUsage_AggregateBySource(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewTaskLLMUsageRepository(db.DB)
	for i, src := range []string{"workflow_step", "workflow_step", "dispatcher", "judge"} {
		if err := repo.Record(ctx, &persistence.TaskLLMUsage{
			ID:        "u-" + string(rune('a'+i)),
			ProjectID: "p",
			StepID:    "s",
			Role:      "w",
			Model:     "m",
			CostUSD:   float64(i + 1),
			Source:    src,
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	out, err := repo.AggregateBySource(ctx, time.Time{}, time.Time{}, "p")
	if err != nil {
		t.Fatalf("AggregateBySource: %v", err)
	}
	totalsByName := map[string]float64{}
	for _, r := range out {
		totalsByName[r.Source] = r.CostUSD
	}
	if totalsByName["workflow_step"] != 3 {
		t.Errorf("workflow_step total = %v, want 3 (1+2)", totalsByName["workflow_step"])
	}
	if totalsByName["dispatcher"] != 3 {
		t.Errorf("dispatcher total = %v, want 3", totalsByName["dispatcher"])
	}
	if totalsByName["judge"] != 4 {
		t.Errorf("judge total = %v, want 4", totalsByName["judge"])
	}
}

func TestTaskLLMUsage_TimeSeriesByDay(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewTaskLLMUsageRepository(db.DB)

	// Three rows on 2026-05-14, two on 2026-05-15.
	bucket1 := time.Date(2026, 5, 14, 9, 30, 0, 0, time.UTC)
	bucket2 := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	for i, when := range []time.Time{bucket1, bucket1, bucket1, bucket2, bucket2} {
		if err := repo.Record(ctx, &persistence.TaskLLMUsage{
			ID:         "u-" + string(rune('a'+i)),
			ProjectID:  "p",
			StepID:     "s",
			Role:       "w",
			Model:      "m",
			CostUSD:    1.0,
			RecordedAt: when,
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	out, err := repo.TimeSeriesByDay(ctx, time.Time{}, time.Time{}, "p")
	if err != nil {
		t.Fatalf("TimeSeriesByDay: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 day buckets, got %d", len(out))
	}
	if out[0].Day.UTC() != time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC) {
		t.Errorf("day 0 = %v, want 2026-05-14 UTC", out[0].Day)
	}
	if out[0].CallCount != 3 || out[1].CallCount != 2 {
		t.Errorf("call counts wrong: %d / %d", out[0].CallCount, out[1].CallCount)
	}
}

func TestTaskLLMUsage_TaskCostBreakdown(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewTaskLLMUsageRepository(db.DB)
	taskID := "task-1"
	base := time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if err := repo.Record(ctx, &persistence.TaskLLMUsage{
			ID:         "u-step" + string(rune('0'+i)),
			ProjectID:  "p",
			TaskID:     &taskID,
			StepID:     "step-" + string(rune('0'+i)),
			Role:       "w",
			Model:      "m",
			CostUSD:    float64(i + 1),
			RecordedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	out, err := repo.TaskCostBreakdown(ctx, taskID)
	if err != nil {
		t.Fatalf("TaskCostBreakdown: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(out))
	}
	// Sorted by recorded_at ASC.
	if out[0].StepID != "step-0" || out[2].StepID != "step-2" {
		t.Errorf("rows not in execution order: %v / %v", out[0].StepID, out[2].StepID)
	}
}

// TestTaskLLMUsage_MeanCostByWorkflow pins the §8.2 cost-estimate
// query: the per-(project, workflow) average is total spend divided
// by the DISTINCT-task count (so a multi-step task isn't
// double-counted), and an unseen workflow returns sampleCount 0.
func TestTaskLLMUsage_MeanCostByWorkflow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	taskRepo := sqlite.NewTaskRepository(db.DB)
	usageRepo := sqlite.NewTaskLLMUsageRepository(db.DB)

	wfA, wfB := "wf-a", "wf-b"
	// Two tasks on wf-a (one with two usage rows), one on wf-b.
	tasks := []struct {
		id string
		wf *string
	}{
		{"t-a1", &wfA},
		{"t-a2", &wfA},
		{"t-b1", &wfB},
	}
	for _, tk := range tasks {
		if err := taskRepo.Create(ctx, &persistence.Task{ID: tk.id, ProjectID: "p1", WorkflowID: tk.wf}); err != nil {
			t.Fatalf("create task %s: %v", tk.id, err)
		}
	}
	tA1, tA2, tB1 := "t-a1", "t-a2", "t-b1"
	rows := []persistence.TaskLLMUsage{
		{ID: "u1", ProjectID: "p1", TaskID: &tA1, StepID: "s1", Role: "w", Model: "m", CostUSD: 0.10},
		{ID: "u2", ProjectID: "p1", TaskID: &tA1, StepID: "s2", Role: "w", Model: "m", CostUSD: 0.30}, // same task → 0.40 total
		{ID: "u3", ProjectID: "p1", TaskID: &tA2, StepID: "s1", Role: "w", Model: "m", CostUSD: 0.20},
		{ID: "u4", ProjectID: "p1", TaskID: &tB1, StepID: "s1", Role: "w", Model: "m", CostUSD: 5.00},
	}
	for i := range rows {
		if err := usageRepo.Record(ctx, &rows[i]); err != nil {
			t.Fatalf("record %s: %v", rows[i].ID, err)
		}
	}

	// wf-a: total 0.60 over 2 distinct tasks → mean 0.30.
	mean, n, err := usageRepo.MeanCostByWorkflow(ctx, "p1", wfA, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("MeanCostByWorkflow wf-a: %v", err)
	}
	if n != 2 {
		t.Fatalf("wf-a sample = %d, want 2 distinct tasks", n)
	}
	if mean < 0.2999 || mean > 0.3001 {
		t.Fatalf("wf-a mean = %v, want 0.30", mean)
	}

	// Unseen workflow → (0, 0, nil) so the caller omits the estimate.
	mean, n, err = usageRepo.MeanCostByWorkflow(ctx, "p1", "wf-nope", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("MeanCostByWorkflow wf-nope: %v", err)
	}
	if n != 0 || mean != 0 {
		t.Fatalf("unseen workflow = (%v, %d), want (0, 0)", mean, n)
	}
}

func TestExecutionRepository_GetRoleQuality(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	outcomeRepo := sqlite.NewExecutionStepOutcomeRepository(db.DB)
	now := time.Now().UTC()
	rows := []persistence.ExecutionStepOutcome{
		{ID: "o1", ProjectID: "p", TaskID: "t", ExecutionID: "e1", StepID: "s1", Role: "worker", Model: "m", Outcome: "ok", DurationMS: int64Ptr(1000), RecordedAt: now},
		{ID: "o2", ProjectID: "p", TaskID: "t", ExecutionID: "e1", StepID: "s2", Role: "worker", Model: "m", Outcome: "ok", DurationMS: int64Ptr(2000), RecordedAt: now},
		{ID: "o3", ProjectID: "p", TaskID: "t", ExecutionID: "e1", StepID: "s3", Role: "worker", Model: "m", Outcome: "failed", RecordedAt: now},
		// Cancelled is excluded from the quality denominator.
		{ID: "o4", ProjectID: "p", TaskID: "t", ExecutionID: "e1", StepID: "s4", Role: "worker", Model: "m", Outcome: "cancelled", RecordedAt: now},
		// Empty role is excluded.
		{ID: "o5", ProjectID: "p", TaskID: "t", ExecutionID: "e1", StepID: "s5", Role: "", Model: "m", Outcome: "ok", RecordedAt: now},
		// Different role bucket.
		{ID: "o6", ProjectID: "p", TaskID: "t", ExecutionID: "e1", StepID: "s6", Role: "judge", Model: "m", Outcome: "ok", DurationMS: int64Ptr(500), RecordedAt: now},
	}
	for i := range rows {
		if err := outcomeRepo.Record(ctx, &rows[i]); err != nil {
			t.Fatalf("Record %s: %v", rows[i].ID, err)
		}
	}
	execRepo := sqlite.NewExecutionRepository(db.DB)
	out, err := execRepo.GetRoleQuality(ctx, "p", 24*time.Hour)
	if err != nil {
		t.Fatalf("GetRoleQuality: %v", err)
	}
	w := out["worker"]
	if w == nil {
		t.Fatalf("worker role missing from result: %v", out)
	}
	if w.Executions != 3 {
		t.Errorf("worker Executions = %d, want 3 (ok+ok+failed, cancelled excluded)", w.Executions)
	}
	if w.Completed != 2 || w.Failed != 1 {
		t.Errorf("worker Completed/Failed = %d/%d, want 2/1", w.Completed, w.Failed)
	}
	if w.SuccessRatePct != 66.7 {
		t.Errorf("worker SuccessRatePct = %v, want 66.7", w.SuccessRatePct)
	}
	// Avg duration only counts ok rows with non-nil duration:
	// (1000 + 2000) / 2 = 1500 ms → 1.5 sec.
	if w.AvgDurationSec != 1.5 {
		t.Errorf("worker AvgDurationSec = %v, want 1.5", w.AvgDurationSec)
	}
	if _, has := out[""]; has {
		t.Error("empty role should be filtered from result")
	}
}

// TestTaskLLMUsage_AggregateByAPIKey pins the per-API-key grouping
// (migration 149): rows attributed to a real API key join against
// api_keys for the display name/prefix; rows with no api_key_id collapse
// into one "" (Unattributed) bucket rather than one row per project.
func TestTaskLLMUsage_AggregateByAPIKey(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewTaskLLMUsageRepository(db.DB)

	if _, err := db.ExecContext(ctx, `INSERT INTO api_keys
		(id, project_id, name, key_hash, key_prefix, created_at)
		VALUES ('akey-1', 'p', 'jnovak/laptop', 'h_akey1', 'vk_abcd', datetime('now'))`); err != nil {
		t.Fatalf("insert api_keys fixture: %v", err)
	}

	akey1 := "akey-1"
	rows := []persistence.TaskLLMUsage{
		{ID: "u1", ProjectID: "p", StepID: "s1", Role: "coder", Model: "m", CostUSD: 1.0, APIKeyID: &akey1},
		{ID: "u2", ProjectID: "p", StepID: "s2", Role: "coder", Model: "m", CostUSD: 2.0, APIKeyID: &akey1},
		{ID: "u3", ProjectID: "p", StepID: "s3", Role: "dispatcher", Model: "m", CostUSD: 0.5}, // no api_key_id
	}
	for i := range rows {
		if err := repo.Record(ctx, &rows[i]); err != nil {
			t.Fatalf("Record %s: %v", rows[i].ID, err)
		}
	}

	out, err := repo.AggregateByAPIKey(ctx, time.Time{}, time.Time{}, 10, "p")
	if err != nil {
		t.Fatalf("AggregateByAPIKey: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 buckets (attributed + unattributed), got %d: %+v", len(out), out)
	}
	// Cost-sorted descending — the attributed key (3.0) comes first.
	if out[0].APIKeyID != akey1 || out[0].KeyName != "jnovak/laptop" || out[0].KeyPrefix != "vk_abcd" {
		t.Errorf("row 0 = %+v, want attributed to akey-1/jnovak/laptop/vk_abcd", out[0])
	}
	if out[0].CostUSD != 3.0 || out[0].CallCount != 2 {
		t.Errorf("row 0 cost/calls = %v/%d, want 3.0/2", out[0].CostUSD, out[0].CallCount)
	}
	if out[1].APIKeyID != "" || out[1].KeyName != "" {
		t.Errorf("row 1 should be the unattributed bucket, got %+v", out[1])
	}
	if out[1].CostUSD != 0.5 || out[1].CallCount != 1 {
		t.Errorf("unattributed cost/calls = %v/%d, want 0.5/1", out[1].CostUSD, out[1].CallCount)
	}
}

// TestTaskLLMUsage_SumCostByAPIKey_OneIdentityRule pins that the budget-cap
// gate and the /ui/spend per-key table agree on WHOSE spend a task's usage is.
//
// Before migration 148 the cap resolved a key only through the companion
// payload marker, so a key spending through REST POST /tasks was capped at
// nothing — while the new per-key table counted it. The cap now reads
// tasks.created_by_api_key_id first and keeps the payload marker as the
// pre-migration fallback, so both surfaces attribute identically. Added during
// the CE PR #6 intake.
func TestTaskLLMUsage_SumCostByAPIKey_OneIdentityRule(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	usage := sqlite.NewTaskLLMUsageRepository(db.DB)

	// t-col: attributed by the migration-148 column only (a REST-created task
	// — no companion payload marker anywhere).
	// t-json: attributed the legacy way only (created before the column
	// existed, so it must still count).
	// t-other: a different key, to prove the sum is not just "everything".
	seed := []struct{ id, payload, col string }{
		{"t-col", `{}`, "akey-1"},
		{"t-json", `{"companion":{"api_key_id":"akey-1"}}`, ""},
		{"t-other", `{}`, "akey-2"},
	}
	for _, s := range seed {
		var col any
		if s.col != "" {
			col = s.col
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO tasks
			(id, project_id, status, payload, created_at, updated_at, created_by_api_key_id)
			VALUES (?, 'p', 'COMPLETED', ?, datetime('now'), datetime('now'), ?)`,
			s.id, s.payload, col); err != nil {
			t.Fatalf("insert task %s: %v", s.id, err)
		}
	}
	akey1, akey2 := "akey-1", "akey-2"
	tCol, tJSON, tOther := "t-col", "t-json", "t-other"
	rows := []persistence.TaskLLMUsage{
		{ID: "u-col", TaskID: &tCol, ProjectID: "p", StepID: "s1", Role: "coder", Model: "m", CostUSD: 1.25, APIKeyID: &akey1},
		{ID: "u-json", TaskID: &tJSON, ProjectID: "p", StepID: "s2", Role: "coder", Model: "m", CostUSD: 0.75},
		{ID: "u-other", TaskID: &tOther, ProjectID: "p", StepID: "s3", Role: "coder", Model: "m", CostUSD: 9.0, APIKeyID: &akey2},
	}
	for i := range rows {
		if err := usage.Record(ctx, &rows[i]); err != nil {
			t.Fatalf("Record %s: %v", rows[i].ID, err)
		}
	}

	got, err := usage.SumCostByAPIKey(ctx, akey1, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("SumCostByAPIKey: %v", err)
	}
	if got != 2.0 {
		t.Fatalf("sum = %v, want 2.0 (1.25 via the column + 0.75 via the payload fallback)", got)
	}

	// The other key's spend must not leak in — a cap is per key.
	other, err := usage.SumCostByAPIKey(ctx, akey2, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("SumCostByAPIKey(akey-2): %v", err)
	}
	if other != 9.0 {
		t.Fatalf("akey-2 sum = %v, want 9.0", other)
	}
}

func int64Ptr(v int64) *int64 { return &v }
