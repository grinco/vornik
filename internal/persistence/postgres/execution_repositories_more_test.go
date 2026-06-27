package postgres

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"vornik.io/vornik/internal/persistence"
)

func executionRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "task_id", "project_id", "workflow_id", "workflow_revision",
		"status", "current_step_id", "completed_steps", "state_snapshot",
		"result", "error_message", "error_code", "started_at", "completed_at",
		"created_at", "updated_at",
		"parent_execution_id", "forked_from_step_id", "forked_prompt_override",
	})
}

func TestExecutionRepositoryCreateDefaultsAndGet(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewExecutionRepository(db)

	if err := repo.Create(context.Background(), nil); err == nil {
		t.Fatal("expected nil execution error")
	}

	exec := &persistence.Execution{
		ID: "exec-1", TaskID: "task-1", ProjectID: "proj-a", WorkflowID: "wf-1",
		CompletedSteps: []string{"plan"}, StateSnapshot: []byte(`{"state":1}`),
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO executions")).
		WithArgs(exec.ID, exec.TaskID, exec.ProjectID, exec.WorkflowID, "v1", persistence.ExecutionStatusPending, exec.CurrentStepID, sqlmock.AnyArg(), `{"state":1}`, nil, exec.ErrorMessage, exec.ErrorCode, exec.StartedAt, exec.CompletedAt, sqlmock.AnyArg(), sqlmock.AnyArg(), exec.ParentExecutionID, exec.ForkedFromStepID, exec.ForkedPromptOverride).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.Create(context.Background(), exec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if exec.Status != persistence.ExecutionStatusPending || exec.WorkflowRevision != "v1" || exec.CreatedAt.IsZero() || exec.UpdatedAt.IsZero() {
		t.Fatalf("Create() did not apply defaults: %#v", exec)
	}

	stepID := "plan"
	errMsg := "failed"
	errCode := "E_TEST"
	started := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)
	completed := time.Date(2026, 5, 14, 10, 1, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, task_id, project_id, workflow_id, workflow_revision")).
		WithArgs("exec-1").
		WillReturnRows(executionRows().AddRow(
			"exec-1", "task-1", "proj-a", "wf-1", "v1",
			persistence.ExecutionStatusFailed, stepID, "{plan,run}", []byte(`{"state":1}`),
			[]byte(`{"ok":false}`), errMsg, errCode, started, completed, started, completed,
			nil, nil, nil,
		))

	got, err := repo.Get(context.Background(), "exec-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.ID != "exec-1" || got.CurrentStepID == nil || *got.CurrentStepID != "plan" || len(got.CompletedSteps) != 2 {
		t.Fatalf("Get() = %#v", got)
	}
	if got.ErrorMessage == nil || *got.ErrorMessage != errMsg || got.StartedAt == nil || !got.StartedAt.Equal(started) {
		t.Fatalf("Get() nullable fields = %#v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestExecutionRepositoryUpdatesAndCounts(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewExecutionRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE executions")).
		WithArgs("exec-1", persistence.ExecutionStatusRunning).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.UpdateStatus(context.Background(), "exec-1", persistence.ExecutionStatusRunning); err != nil {
		t.Fatalf("UpdateStatus() error = %v", err)
	}

	mock.ExpectExec(regexp.QuoteMeta("UPDATE executions")).
		WithArgs("exec-1", `{"resume":true}`, "run", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.SaveStateSnapshot(context.Background(), "exec-1", []byte(`{"resume":true}`), "run", []string{"plan"}); err != nil {
		t.Fatalf("SaveStateSnapshot() error = %v", err)
	}

	if err := repo.SetWorkflowSnapshot(context.Background(), "exec-1", nil); err != nil {
		t.Fatalf("SetWorkflowSnapshot(empty) error = %v", err)
	}
	mock.ExpectExec(regexp.QuoteMeta("UPDATE executions")).
		WithArgs("exec-1", []byte(`{"steps":[]}`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.SetWorkflowSnapshot(context.Background(), "exec-1", []byte(`{"steps":[]}`)); err != nil {
		t.Fatalf("SetWorkflowSnapshot() error = %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT workflow_snapshot")).
		WithArgs("exec-1").
		WillReturnRows(sqlmock.NewRows([]string{"workflow_snapshot"}).AddRow([]byte(`{"steps":[]}`)))
	snapshot, err := repo.GetWorkflowSnapshot(context.Background(), "exec-1")
	if err != nil {
		t.Fatalf("GetWorkflowSnapshot() error = %v", err)
	}
	if string(snapshot) != `{"steps":[]}` {
		t.Fatalf("GetWorkflowSnapshot() = %s", snapshot)
	}

	mock.ExpectExec(regexp.QuoteMeta("UPDATE executions")).
		WithArgs("exec-1", `{"result":true}`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.RecordCompletion(context.Background(), "exec-1", []byte(`{"result":true}`)); err != nil {
		t.Fatalf("RecordCompletion() error = %v", err)
	}

	mock.ExpectExec(regexp.QuoteMeta("UPDATE executions")).
		WithArgs("exec-1", "boom", "E_BOOM").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.RecordFailure(context.Background(), "exec-1", "boom", "E_BOOM"); err != nil {
		t.Fatalf("RecordFailure() error = %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT status, COUNT(*)")).
		WithArgs("proj-a").
		WillReturnRows(sqlmock.NewRows([]string{"status", "count"}).
			AddRow(persistence.ExecutionStatusCompleted, int64(3)).
			AddRow(persistence.ExecutionStatusFailed, int64(1)))
	counts, err := repo.CountByStatus(context.Background(), "proj-a")
	if err != nil {
		t.Fatalf("CountByStatus() error = %v", err)
	}
	if counts[persistence.ExecutionStatusCompleted] != 3 || counts[persistence.ExecutionStatusFailed] != 1 {
		t.Fatalf("CountByStatus() = %#v", counts)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestExecutionRepositoryListCountAndRoleQuality(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewExecutionRepository(db)

	projectID := "proj-a"
	taskID := "task-1"
	status := persistence.ExecutionStatusCompleted
	created := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, task_id, project_id, workflow_id, workflow_revision")).
		WithArgs(projectID, taskID, status, 10, 5).
		WillReturnRows(executionRows().AddRow(
			"exec-1", taskID, projectID, "wf-1", "v1",
			status, nil, "{}", nil, nil, nil, nil, nil, nil, created, created,
			nil, nil, nil,
		))
	list, err := repo.List(context.Background(), persistence.ExecutionFilter{
		ProjectID: &projectID, TaskID: &taskID, Status: &status, PageSize: 10, Offset: 5,
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 || list[0].ID != "exec-1" {
		t.Fatalf("List() = %#v", list)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM executions WHERE 1=1")).
		WithArgs(projectID, taskID, status).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(7)))
	total, err := repo.Count(context.Background(), persistence.ExecutionFilter{ProjectID: &projectID, TaskID: &taskID, Status: &status})
	if err != nil {
		t.Fatalf("Count() error = %v", err)
	}
	if total != 7 {
		t.Fatalf("Count() = %d, want 7", total)
	}

	if _, err := repo.GetRoleQuality(context.Background(), "", time.Hour); err == nil {
		t.Fatal("expected empty projectID error")
	}
	mock.ExpectQuery("WITH role_outcomes AS").
		WithArgs(projectID, int64((24 * time.Hour).Seconds())).
		WillReturnRows(sqlmock.NewRows([]string{"role_name", "total", "completed", "failed", "avg_duration_sec"}).
			AddRow("coder", int64(3), int64(2), int64(1), 1.25))
	quality, err := repo.GetRoleQuality(context.Background(), projectID, 24*time.Hour)
	if err != nil {
		t.Fatalf("GetRoleQuality() error = %v", err)
	}
	if quality["coder"].SuccessRatePct != 66.7 {
		t.Fatalf("GetRoleQuality() = %#v", quality["coder"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestExecutionRepositoryGetByTaskIDs(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewExecutionRepository(db)

	empty, err := repo.GetByTaskIDs(context.Background(), nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("GetByTaskIDs(nil) = %#v, %v", empty, err)
	}

	created := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT DISTINCT ON (task_id)")).
		WillReturnRows(executionRows().AddRow(
			"exec-1", "task-1", "proj-a", "wf-1", "v1",
			persistence.ExecutionStatusRunning, nil, "{}", nil, nil, nil, nil, nil, nil, created, created,
			nil, nil, nil,
		))
	got, err := repo.GetByTaskIDs(context.Background(), []string{"task-1"})
	if err != nil {
		t.Fatalf("GetByTaskIDs() error = %v", err)
	}
	if got["task-1"].ID != "exec-1" {
		t.Fatalf("GetByTaskIDs() = %#v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestExecutionStepOutcomeRepositoryRecordFinalizeListAndCounts(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewExecutionStepOutcomeRepository(db)

	if err := repo.Record(context.Background(), nil); err == nil {
		t.Fatal("expected nil outcome error")
	}
	duration := int64(1234)
	finalized := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)
	attr := "step-a"
	outcome := &persistence.ExecutionStepOutcome{
		ID: "out-1", ProjectID: "proj-a", TaskID: "task-1", ExecutionID: "exec-1", StepID: "step-1",
		Role: "coder", Model: "gpt-test", Outcome: "pending_validation", AttributedToStepID: &attr,
		ErrorClass: "none", DurationMS: &duration, FinalizedAt: &finalized, HallucinationSignals: []byte(`[{"kind":"url"}]`),
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO execution_step_outcomes")).
		WithArgs(outcome.ID, outcome.ProjectID, outcome.TaskID, outcome.ExecutionID, outcome.StepID, outcome.Role, outcome.Model, outcome.Outcome, sqlmock.AnyArg(), outcome.ErrorClass, outcome.ErrorDetail, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.Record(context.Background(), outcome); err != nil {
		t.Fatalf("Record() error = %v", err)
	}

	mock.ExpectExec(regexp.QuoteMeta("UPDATE execution_step_outcomes")).
		WithArgs("ok", "", "", sqlmock.AnyArg(), "out-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.Finalize(context.Background(), "out-1", "ok", "", "", nil); err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}

	mock.ExpectExec(regexp.QuoteMeta("UPDATE execution_step_outcomes")).
		WithArgs("ok", "", "", sqlmock.AnyArg(), "missing").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := repo.Finalize(context.Background(), "missing", "ok", "", "", nil); err != persistence.ErrNotFound {
		t.Fatalf("Finalize(missing) error = %v, want ErrNotFound", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("UPDATE execution_step_outcomes")).
		WithArgs("ok", "", "", sqlmock.AnyArg(), "exec-1", "step-1").
		WillReturnRows(sqlmock.NewRows([]string{"role", "model"}).AddRow("coder", "gpt-test"))
	role, model, err := repo.FinalizePending(context.Background(), "exec-1", "step-1", "ok", "", "", nil)
	if err != nil {
		t.Fatalf("FinalizePending() error = %v", err)
	}
	if role != "coder" || model != "gpt-test" {
		t.Fatalf("FinalizePending() = %q, %q", role, model)
	}

	mock.ExpectQuery(regexp.QuoteMeta("UPDATE execution_step_outcomes")).
		WithArgs("cancelled", "exec-1").
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "role", "model"}).AddRow("step-2", "tester", "gpt-test"))
	swept, err := repo.SweepPending(context.Background(), "exec-1", "cancelled")
	if err != nil {
		t.Fatalf("SweepPending() error = %v", err)
	}
	if len(swept) != 1 || swept[0].StepID != "step-2" {
		t.Fatalf("SweepPending() = %#v", swept)
	}

	projectID := "proj-a"
	stepID := "step-1"
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, project_id, task_id, execution_id, step_id")).
		WithArgs(projectID, stepID, 10).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "task_id", "execution_id", "step_id",
			"role", "model", "outcome", "attributed_to_step_id",
			"error_class", "error_detail", "duration_ms",
			"finalized_at", "recorded_at", "hallucination_signals",
			"complexity_tier", "effective_tool_budget", "tool_calls_used",
		}).AddRow("out-1", projectID, "task-1", "exec-1", stepID, "coder", "gpt-test", "ok", attr, "", "", duration, finalized, finalized, []byte(`[{"kind":"url"}]`), nil, nil, nil))
	list, err := repo.List(context.Background(), persistence.ExecutionStepOutcomeFilter{
		ProjectID: &projectID, StepID: &stepID, PageSize: 10,
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 || list[0].DurationMS == nil || *list[0].DurationMS != duration || string(list[0].HallucinationSignals) == "" {
		t.Fatalf("List() = %#v", list)
	}

	cutoff := time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE execution_step_outcomes")).
		WithArgs("exec-1", cutoff).
		WillReturnResult(sqlmock.NewResult(0, 4))
	n, err := repo.SupersedeAfter(context.Background(), "exec-1", cutoff)
	if err != nil || n != 4 {
		t.Fatalf("SupersedeAfter() = %d, %v", n, err)
	}

	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT role, model, COUNT(*)")).
		WithArgs("ok", since, until, projectID).
		WillReturnRows(sqlmock.NewRows([]string{"role", "model", "count"}).AddRow("coder", "gpt-test", int64(5)))
	counts, err := repo.CountByRoleModelOutcome(context.Background(), "ok", since, until, projectID)
	if err != nil {
		t.Fatalf("CountByRoleModelOutcome() error = %v", err)
	}
	if len(counts) != 1 || counts[0].Count != 5 {
		t.Fatalf("CountByRoleModelOutcome() = %#v", counts)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}
