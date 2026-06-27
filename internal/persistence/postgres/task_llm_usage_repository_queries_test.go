package postgres

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"vornik.io/vornik/internal/persistence"
)

// Helpers for usage analytics columns. Kept here (not in the
// happy-path file) so the smoke tests below can stand alone.

func usageRow() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "project_id", "task_id", "execution_id", "step_id",
		"role", "model", "prompt_tokens", "completion_tokens", "iterations",
		"cost_usd", "source", "session_id", "recorded_at",
		"cache_creation_tokens", "cache_read_tokens",
	})
}

// TestUsageList_HappyPath_DefaultFilter exercises the no-filter
// path through List + the row-scan loop, which is the largest
// chunk of statements in the file.
func TestUsageList_HappyPath_DefaultFilter(t *testing.T) {
	repo, mock, cleanup := newUsageRepo(t)
	defer cleanup()

	recorded := time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC)
	rows := usageRow().
		AddRow(
			"u-1", "p-1", "t-1", "e-1", "s-1",
			"worker", "claude", int64(100), int64(50), 1,
			1.0, "workflow_step", "sess-1", recorded,
			int64(0), int64(0),
		).
		AddRow(
			"u-2", "p-1", nil, nil, "s-2",
			"dispatcher", "claude", int64(40), int64(10), 0,
			0.5, "dispatcher", nil, recorded,
			int64(0), int64(0),
		)
	mock.ExpectQuery(regexp.QuoteMeta("FROM task_llm_usage WHERE 1=1")).
		WithArgs(). // no filter args
		WillReturnRows(rows)

	out, err := repo.List(context.Background(), persistence.TaskLLMUsageFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(out))
	}
	if out[0].TaskID == nil || *out[0].TaskID != "t-1" {
		t.Errorf("row 0 task_id roundtrip: %+v", out[0].TaskID)
	}
	if out[1].TaskID != nil {
		t.Errorf("row 1 task_id should be nil, got %+v", out[1].TaskID)
	}
}

// TestUsageList_AllFilters drives every WHERE branch in List so the
// $-placeholder bookkeeping is exercised end-to-end. The actual
// row contents are uninteresting — what matters is the SQL+args
// the repo emits.
func TestUsageList_AllFilters(t *testing.T) {
	repo, mock, cleanup := newUsageRepo(t)
	defer cleanup()

	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)
	f := persistence.TaskLLMUsageFilter{
		ProjectID:   strPtr("p-1"),
		TaskID:      strPtr("t-1"),
		ExecutionID: strPtr("e-1"),
		Role:        strPtr("worker"),
		Model:       strPtr("claude"),
		Source:      strPtr("workflow_step"),
		SessionID:   strPtr("sess-1"),
		Since:       &since,
		Until:       &until,
		PageSize:    50,
		Offset:      10,
	}

	mock.ExpectQuery(regexp.QuoteMeta("FROM task_llm_usage")).
		WithArgs(
			"p-1", "t-1", "e-1", "worker", "claude",
			"workflow_step", "sess-1", since, until,
			50, 10,
		).
		WillReturnRows(usageRow())

	if _, err := repo.List(context.Background(), f); err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestUsageList_QueryError(t *testing.T) {
	repo, mock, cleanup := newUsageRepo(t)
	defer cleanup()

	mock.ExpectQuery("FROM task_llm_usage").
		WillReturnError(errors.New("conn closed"))

	if _, err := repo.List(context.Background(), persistence.TaskLLMUsageFilter{}); err == nil {
		t.Fatalf("expected error, got nil")
	}
}

func TestUsageSumCostByProject_Bounded(t *testing.T) {
	repo, mock, cleanup := newUsageRepo(t)
	defer cleanup()

	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(SUM(cost_usd), 0)")).
		WithArgs("p-1", since, until).
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(12.5))

	total, err := repo.SumCostByProject(context.Background(), "p-1", since, until)
	if err != nil {
		t.Fatalf("SumCostByProject: %v", err)
	}
	if total != 12.5 {
		t.Errorf("expected 12.5, got %v", total)
	}
}

func TestUsageSumCostByProject_Unbounded(t *testing.T) {
	repo, mock, cleanup := newUsageRepo(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(SUM(cost_usd), 0)")).
		WithArgs("p-1").
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(0.0))

	total, err := repo.SumCostByProject(context.Background(), "p-1", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("SumCostByProject: %v", err)
	}
	if total != 0 {
		t.Errorf("expected 0, got %v", total)
	}
}

func TestUsageSumCost_Bounded(t *testing.T) {
	repo, mock, cleanup := newUsageRepo(t)
	defer cleanup()

	since := time.Now().Add(-24 * time.Hour).UTC()
	until := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(SUM(cost_usd), 0)")).
		WithArgs(since, until).
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(7.0))

	total, err := repo.SumCost(context.Background(), since, until)
	if err != nil {
		t.Fatalf("SumCost: %v", err)
	}
	if total != 7.0 {
		t.Errorf("expected 7.0, got %v", total)
	}
}

func TestUsageSumCost_Unbounded(t *testing.T) {
	repo, mock, cleanup := newUsageRepo(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(SUM(cost_usd), 0)")).
		WithArgs().
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(0.0))

	if _, err := repo.SumCost(context.Background(), time.Time{}, time.Time{}); err != nil {
		t.Fatalf("SumCost: %v", err)
	}
}

func TestUsageAggregateByProject(t *testing.T) {
	repo, mock, cleanup := newUsageRepo(t)
	defer cleanup()

	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)

	rows := sqlmock.NewRows([]string{
		"project_id", "cost_usd", "step_count", "prompt_tokens", "completion_tokens", "task_count",
		"cache_creation_tokens", "cache_read_tokens",
	}).AddRow("p-1", 5.0, int64(10), int64(1000), int64(500), int64(3), int64(120), int64(800))

	mock.ExpectQuery(regexp.QuoteMeta("GROUP BY project_id ORDER BY cost_usd DESC")).
		WithArgs(since, until, 25).
		WillReturnRows(rows)

	out, err := repo.AggregateByProject(context.Background(), since, until, 25)
	if err != nil {
		t.Fatalf("AggregateByProject: %v", err)
	}
	if len(out) != 1 || out[0].ProjectID != "p-1" || out[0].CostUSD != 5.0 {
		t.Errorf("row roundtrip: %+v", out)
	}
	if out[0].CacheCreationTokens != 120 || out[0].CacheReadTokens != 800 {
		t.Errorf("cache token columns lost in scan: %+v", out[0])
	}
}

func TestUsageAggregateBySource(t *testing.T) {
	repo, mock, cleanup := newUsageRepo(t)
	defer cleanup()

	rows := sqlmock.NewRows([]string{
		"source", "cost_usd", "call_count", "prompt_tokens", "completion_tokens",
		"cache_creation_tokens", "cache_read_tokens",
	}).AddRow("workflow_step", 3.0, int64(5), int64(500), int64(250), int64(100), int64(700)).
		AddRow("dispatcher", 0.5, int64(2), int64(100), int64(40), int64(0), int64(0))

	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta("GROUP BY source ORDER BY cost_usd DESC")).
		WithArgs(since, until, "p-1").
		WillReturnRows(rows)

	out, err := repo.AggregateBySource(context.Background(), since, until, "p-1")
	if err != nil {
		t.Fatalf("AggregateBySource: %v", err)
	}
	if len(out) != 2 || out[0].Source != "workflow_step" {
		t.Errorf("rows: %+v", out)
	}
	if out[0].CacheCreationTokens != 100 || out[0].CacheReadTokens != 700 {
		t.Errorf("cache token columns lost in scan: %+v", out[0])
	}
}

func TestUsageTimeSeriesByDay(t *testing.T) {
	repo, mock, cleanup := newUsageRepo(t)
	defer cleanup()

	d1 := time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows([]string{"day", "cost_usd", "call_count"}).
		AddRow(d1, 1.0, int64(3)).
		AddRow(d2, 2.5, int64(5))

	mock.ExpectQuery(regexp.QuoteMeta("GROUP BY day ORDER BY day ASC")).
		WithArgs(d1, d2, "p-1").
		WillReturnRows(rows)

	out, err := repo.TimeSeriesByDay(context.Background(), d1, d2, "p-1")
	if err != nil {
		t.Fatalf("TimeSeriesByDay: %v", err)
	}
	if len(out) != 2 || out[1].CostUSD != 2.5 {
		t.Errorf("rows: %+v", out)
	}
}

// TestUsageTopTasks covers the LEFT-JOIN-tolerant path that the
// retention comment in the source documents: a usage row whose
// task row has been pruned should still come back (with empty
// status/creation_source).
func TestUsageTopTasks(t *testing.T) {
	repo, mock, cleanup := newUsageRepo(t)
	defer cleanup()

	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows([]string{
		"task_id", "project_id", "status", "creation_source",
		"cost_usd", "prompt_tokens", "completion_tokens",
		"step_count", "iterations", "first_step_at", "last_step_at",
	}).AddRow(
		"task-1", "p-1", "COMPLETED", "user",
		7.5, int64(1000), int64(500), int64(4), int64(2), since, until,
	).AddRow(
		nil, "p-1", "", "", // pruned task row
		2.0, int64(200), int64(80), int64(1), int64(1), since, until,
	)

	mock.ExpectQuery(regexp.QuoteMeta("LEFT JOIN tasks t ON t.id = u.task_id")).
		WithArgs(since, until, "p-1", 5).
		WillReturnRows(rows)

	out, err := repo.TopTasks(context.Background(), since, until, 5, "p-1")
	if err != nil {
		t.Fatalf("TopTasks: %v", err)
	}
	if len(out) != 2 || out[0].TaskID != "task-1" {
		t.Errorf("rows: %+v", out)
	}
	if out[1].TaskID != "" {
		t.Errorf("pruned-task row should carry empty TaskID, got %q", out[1].TaskID)
	}
}

func TestUsageTaskCostBreakdown(t *testing.T) {
	repo, mock, cleanup := newUsageRepo(t)
	defer cleanup()

	now := time.Now().UTC()
	rows := sqlmock.NewRows([]string{
		"step_id", "role", "model", "prompt_tokens", "completion_tokens",
		"iterations", "cost_usd", "recorded_at", "source",
	}).AddRow("s-1", "worker", "claude", int64(100), int64(50), 1, 1.0, now, "workflow_step")

	mock.ExpectQuery(regexp.QuoteMeta("FROM task_llm_usage WHERE task_id = $1")).
		WithArgs("task-1").
		WillReturnRows(rows)

	out, err := repo.TaskCostBreakdown(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("TaskCostBreakdown: %v", err)
	}
	if len(out) != 1 || out[0].StepID != "s-1" {
		t.Errorf("rows: %+v", out)
	}
}

func TestUsageTaskCostBreakdown_RequiresTaskID(t *testing.T) {
	repo, _, cleanup := newUsageRepo(t)
	defer cleanup()
	if _, err := repo.TaskCostBreakdown(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty taskID")
	}
}

func TestUsageAggregateByRoleModel(t *testing.T) {
	repo, mock, cleanup := newUsageRepo(t)
	defer cleanup()

	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows([]string{
		"role", "model", "cost_usd", "step_count", "prompt_tokens", "completion_tokens",
		"cache_creation_tokens", "cache_read_tokens",
	}).AddRow("worker", "claude", 5.0, int64(10), int64(1000), int64(500), int64(60), int64(2400))

	mock.ExpectQuery(regexp.QuoteMeta("GROUP BY role, model ORDER BY cost_usd DESC")).
		WithArgs(since, until, "p-1", 5).
		WillReturnRows(rows)

	out, err := repo.AggregateByRoleModel(context.Background(), since, until, 5, "p-1")
	if err != nil {
		t.Fatalf("AggregateByRoleModel: %v", err)
	}
	if len(out) != 1 || out[0].Role != "worker" {
		t.Errorf("rows: %+v", out)
	}
	if out[0].CacheCreationTokens != 60 || out[0].CacheReadTokens != 2400 {
		t.Errorf("cache token columns lost in scan: %+v", out[0])
	}
}
