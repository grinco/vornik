package postgres

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"vornik.io/vornik/internal/persistence"
)

func newMockDBTX(t *testing.T) (*sql.DB, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return db, mock, func() { _ = db.Close() }
}

func TestToolAuditRepositoryLogListAndCount(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewToolAuditRepository(db)

	created := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	entry := &persistence.ToolAuditEntry{
		ID: "audit-1", ProjectID: "proj-a", TaskID: "task-1", ExecutionID: "exec-1", StepID: "step-1",
		ToolName: "shell", ToolInput: "go test ./...", ToolOutput: "ok", DurationMs: 42, CreatedAt: created,
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO tool_audit_log")).
		WithArgs(entry.ID, entry.ProjectID, entry.TaskID, entry.ExecutionID, entry.StepID, entry.ToolName, entry.ToolInput, entry.ToolOutput, entry.DurationMs, entry.CreatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Log(context.Background(), entry); err != nil {
		t.Fatalf("Log() error = %v", err)
	}

	projectID, taskID, executionID, toolName := "proj-a", "task-1", "exec-1", "shell"
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, project_id, task_id, execution_id, step_id")).
		WithArgs(projectID, taskID, executionID, toolName, 10, 5).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "task_id", "execution_id", "step_id",
			"tool_name", "tool_input", "tool_output", "duration_ms", "created_at",
		}).AddRow(entry.ID, entry.ProjectID, entry.TaskID, entry.ExecutionID, entry.StepID, entry.ToolName, entry.ToolInput, entry.ToolOutput, entry.DurationMs, entry.CreatedAt))

	entries, err := repo.List(context.Background(), persistence.ToolAuditFilter{
		ProjectID: &projectID, TaskID: &taskID, ExecutionID: &executionID, ToolName: &toolName,
		PageSize: 10, Offset: 5,
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(entries) != 1 || entries[0].ID != entry.ID || entries[0].ToolName != "shell" {
		t.Fatalf("List() = %#v", entries)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT tool_name, COUNT(*) FROM tool_audit_log")).
		WithArgs("exec-1").
		WillReturnRows(sqlmock.NewRows([]string{"tool_name", "count"}).AddRow("shell", int64(2)).AddRow("file_read", int64(1)))

	counts, err := repo.CountByTool(context.Background(), "exec-1")
	if err != nil {
		t.Fatalf("CountByTool() error = %v", err)
	}
	if counts["shell"] != 2 || counts["file_read"] != 1 {
		t.Fatalf("CountByTool() = %#v", counts)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestWebhookEventRepositoryRecordAndList(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewWebhookEventRepository(db)

	taskID := "task-1"
	created := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	event := &persistence.WebhookEvent{
		ID: "evt-1", ProjectID: "proj-a", Source: "github", EventID: "delivery-1",
		PayloadHash: "hash", Status: persistence.WebhookEventStatusAccepted, TaskID: &taskID, CreatedAt: created,
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO webhook_events")).
		WithArgs(event.ID, event.ProjectID, event.Source, event.EventID, event.PayloadHash, event.Status, event.TaskID, event.ErrorCode, event.ErrorMessage, event.CreatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Record(context.Background(), event); err != nil {
		t.Fatalf("Record() error = %v", err)
	}

	projectID, source, status := "proj-a", "github", persistence.WebhookEventStatusAccepted
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, project_id, source, event_id, payload_hash")).
		WithArgs(projectID, source, status, 25, 50).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "source", "event_id", "payload_hash",
			"status", "task_id", "error_code", "error_message", "created_at",
		}).AddRow(event.ID, event.ProjectID, event.Source, event.EventID, event.PayloadHash, event.Status, taskID, "", "", event.CreatedAt))

	events, err := repo.List(context.Background(), persistence.WebhookEventFilter{
		ProjectID: &projectID, Source: &source, Status: &status, PageSize: 25, Offset: 50,
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(events) != 1 || events[0].ID != event.ID || events[0].TaskID == nil || *events[0].TaskID != taskID {
		t.Fatalf("List() = %#v", events)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestTaskScratchpadRepositoryGetAndUpsert(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewTaskScratchpadRepository(db)

	if _, err := repo.Get(context.Background(), ""); err == nil {
		t.Fatal("expected missing task id error")
	}
	if err := repo.Upsert(context.Background(), nil); err == nil {
		t.Fatal("expected nil scratchpad error")
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT task_id, summary, facts, open_questions")).
		WithArgs("missing").
		WillReturnError(sql.ErrNoRows)
	got, err := repo.Get(context.Background(), "missing")
	if err != nil || got != nil {
		t.Fatalf("Get(missing) = %#v, %v; want nil, nil", got, err)
	}

	phase := "build"
	execID := "exec-1"
	updated := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT task_id, summary, facts, open_questions")).
		WithArgs("task-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"task_id", "summary", "facts", "open_questions", "current_phase", "phase_history", "last_execution_id", "updated_at",
		}).AddRow("task-1", "summary", `{"a":1}`, `["q"]`, phase, `[{"name":"build"}]`, execID, updated))

	got, err = repo.Get(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.TaskID != "task-1" || string(got.Facts) != `{"a":1}` || got.CurrentPhase == nil || *got.CurrentPhase != phase {
		t.Fatalf("Get() = %#v", got)
	}

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO task_scratchpad")).
		WithArgs("task-1", "summary", sqlmock.AnyArg(), sqlmock.AnyArg(), &phase, sqlmock.AnyArg(), &execID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Upsert(context.Background(), got); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	if got.UpdatedAt.IsZero() || got.UpdatedAt.Equal(updated) {
		t.Fatalf("Upsert() did not refresh UpdatedAt: %s", got.UpdatedAt)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestTaskPostMortemRepositoryRecordAndGet(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewTaskPostMortemRepository(db)

	if err := repo.Record(context.Background(), nil); err == nil {
		t.Fatal("expected nil post-mortem error")
	}
	if _, err := repo.Get(context.Background(), ""); err == nil {
		t.Fatal("expected missing task id error")
	}

	pm := &persistence.TaskPostMortem{
		TaskID: "task-1", ProjectID: "proj-a", Summary: "failed because tests failed", Model: "gpt-test",
		PromptTokens: 10, CompletionTokens: 20, CostUSD: 0.03,
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO task_post_mortems")).
		WithArgs(pm.TaskID, pm.ProjectID, pm.Summary, pm.Model, pm.PromptTokens, pm.CompletionTokens, pm.CostUSD, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Record(context.Background(), pm); err != nil {
		t.Fatalf("Record() error = %v", err)
	}

	recorded := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT task_id, project_id, summary, model")).
		WithArgs("task-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"task_id", "project_id", "summary", "model", "prompt_tokens", "completion_tokens", "cost_usd", "recorded_at",
		}).AddRow(pm.TaskID, pm.ProjectID, pm.Summary, pm.Model, pm.PromptTokens, pm.CompletionTokens, pm.CostUSD, recorded))

	got, err := repo.Get(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.TaskID != pm.TaskID || got.Summary != pm.Summary || got.RecordedAt != recorded {
		t.Fatalf("Get() = %#v", got)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT task_id, project_id, summary, model")).
		WithArgs("missing").
		WillReturnError(sql.ErrNoRows)
	if _, err := repo.Get(context.Background(), "missing"); err != persistence.ErrNotFound {
		t.Fatalf("Get(missing) error = %v, want ErrNotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}
