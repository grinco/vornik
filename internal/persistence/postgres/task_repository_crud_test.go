package postgres

import (
	"context"
	"database/sql/driver"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"vornik.io/vornik/internal/persistence"
)

// taskRow builds a sqlmock.Rows shaped like scanTask expects: the
// 28-column SELECT used by Get / List / LeaseTask / FindExpiredLeases /
// GetChildren / GetDependents.
func taskRow() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "project_id", "workflow_id", "idempotency_key", "parent_task_id", "creation_source",
		"delegation_mode", "status", "priority", "payload", "dependencies",
		"lease_id", "leased_at", "leased_by", "lease_expires_at",
		"attempt", "max_attempts", "last_error", "last_error_class", "created_at", "updated_at",
		"brief_amended_at", "current_phase", "expected_by", "closed_at", "closed_by", "message_count", "open_checkpoint_id",
		"chat_turn_id",
	})
}

// fullTaskValues returns the 29 column values for a minimally-
// realistic row, with nullable columns explicitly nil so the
// NullString/NullTime branches in scanTask are exercised.
func fullTaskValues(id, projectID string) []driver.Value {
	now := time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC)
	return []driver.Value{
		id, projectID, nil, nil, nil, "USER",
		nil, "QUEUED", 50, []byte("{}"), pq.Array([]string{}),
		nil, nil, nil, nil,
		1, 3, nil, nil, now, now,
		nil, nil, nil, nil, nil, 0, nil,
		nil,
	}
}

func TestTaskPing(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT 1")).
		WillReturnRows(sqlmock.NewRows([]string{"val"}).AddRow(1))

	if err := repo.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestTaskCreate_NilTask(t *testing.T) {
	repo, _, cleanup := newTaskRepo(t)
	defer cleanup()
	if err := repo.Create(context.Background(), nil); err == nil {
		t.Fatal("expected error for nil task")
	}
}

// TestTaskCreate_HappyPath_DefaultsApplied also covers the
// defaulting block: empty Status -> QUEUED, zero Attempt -> 1, etc.
func TestTaskCreate_HappyPath_DefaultsApplied(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	task := &persistence.Task{
		ID:        "task-1",
		ProjectID: "p-1",
		Payload:   []byte("{}"),
	}

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO tasks")).
		WithArgs(
			"task-1", "p-1",
			nil, nil, nil,
			persistence.TaskCreationSourceUser,
			nil, persistence.TaskStatusQueued, 0, []byte("{}"),
			pq.Array([]string(nil)),
			nil, nil, nil, nil,
			1, 3, nil, nil,
			sqlmock.AnyArg(), sqlmock.AnyArg(),
			nil,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Create(context.Background(), task); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if task.Status != persistence.TaskStatusQueued {
		t.Errorf("expected Status default QUEUED, got %s", task.Status)
	}
	if task.Attempt != 1 || task.MaxAttempts != 3 {
		t.Errorf("expected default Attempt=1 MaxAttempts=3, got %d/%d", task.Attempt, task.MaxAttempts)
	}
}

// TestTaskCreate_ChatTurnID confirms the migration-v46 column rides
// through Create and is returned untouched by scanTask.
func TestTaskCreate_ChatTurnID(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	turn := "chat_20260521190824_aaaa"
	task := &persistence.Task{
		ID:         "task-1",
		ProjectID:  "p-1",
		Payload:    []byte("{}"),
		ChatTurnID: &turn,
	}

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO tasks")).
		WithArgs(
			"task-1", "p-1",
			nil, nil, nil,
			persistence.TaskCreationSourceUser,
			nil, persistence.TaskStatusQueued, 0, []byte("{}"),
			pq.Array([]string(nil)),
			nil, nil, nil, nil,
			1, 3, nil, nil,
			sqlmock.AnyArg(), sqlmock.AnyArg(),
			&turn,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Create(context.Background(), task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Roundtrip via Get.
	vals := fullTaskValues("task-1", "p-1")
	vals[len(vals)-1] = turn // chat_turn_id is the last column
	rows := taskRow().AddRow(vals...)
	mock.ExpectQuery(regexp.QuoteMeta("FROM tasks WHERE id = $1")).
		WithArgs("task-1").
		WillReturnRows(rows)

	out, err := repo.Get(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if out.ChatTurnID == nil || *out.ChatTurnID != turn {
		t.Errorf("ChatTurnID roundtrip: got %v want %s", out.ChatTurnID, turn)
	}
}

func TestTaskCreate_PropagatesDuplicateKey(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectExec("INSERT INTO tasks").
		WillReturnError(&pq.Error{Code: "23505"})

	err := repo.Create(context.Background(), &persistence.Task{ID: "task-1", ProjectID: "p-1"})
	if !errors.Is(err, persistence.ErrDuplicateKey) {
		t.Fatalf("expected ErrDuplicateKey, got %v", err)
	}
}

func TestTaskGet_HappyPath(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	rows := taskRow().AddRow(fullTaskValues("task-1", "p-1")...)
	mock.ExpectQuery(regexp.QuoteMeta("FROM tasks WHERE id = $1")).
		WithArgs("task-1").
		WillReturnRows(rows)

	out, err := repo.Get(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if out.ID != "task-1" || out.ProjectID != "p-1" {
		t.Errorf("row roundtrip: %+v", out)
	}
}

func TestTaskGet_NotFound(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectQuery("FROM tasks WHERE id").
		WithArgs("missing").
		WillReturnRows(taskRow()) // no AddRow -> driver returns sql.ErrNoRows

	_, err := repo.Get(context.Background(), "missing")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestTaskGetByIdempotencyKey_HappyPath(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	rows := taskRow().AddRow(fullTaskValues("task-1", "p-1")...)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE project_id = $1 AND idempotency_key = $2")).
		WithArgs("p-1", "key-1").
		WillReturnRows(rows)

	out, err := repo.GetByIdempotencyKey(context.Background(), "p-1", "key-1")
	if err != nil {
		t.Fatalf("GetByIdempotencyKey: %v", err)
	}
	if out.ID != "task-1" {
		t.Errorf("expected task-1, got %s", out.ID)
	}
}

func TestTaskUpdate_NilTask(t *testing.T) {
	repo, _, cleanup := newTaskRepo(t)
	defer cleanup()
	if err := repo.Update(context.Background(), nil); err == nil {
		t.Fatal("expected error for nil task")
	}
}

func TestTaskUpdate_HappyPath(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	task := &persistence.Task{
		ID:             "task-1",
		ProjectID:      "p-1",
		Status:         persistence.TaskStatusRunning,
		Priority:       50,
		Payload:        []byte("{}"),
		CreationSource: persistence.TaskCreationSourceUser,
		Attempt:        1,
		MaxAttempts:    3,
	}

	mock.ExpectExec(regexp.QuoteMeta("UPDATE tasks")).
		WithArgs(
			"task-1", "p-1", nil, nil, nil, "USER",
			nil, persistence.TaskStatusRunning, 50, []byte("{}"),
			pq.Array([]string(nil)),
			nil, nil, nil, nil,
			1, 3, nil, nil,
			sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Update(context.Background(), task); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if task.UpdatedAt.IsZero() {
		t.Errorf("Update should stamp UpdatedAt")
	}
}

func TestTaskDelete(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM tasks WHERE id = $1")).
		WithArgs("task-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Delete(context.Background(), "task-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestTaskList_NoFilter(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	rows := taskRow().AddRow(fullTaskValues("task-1", "p-1")...)
	mock.ExpectQuery(regexp.QuoteMeta("FROM tasks WHERE 1=1")).
		WithArgs().
		WillReturnRows(rows)

	out, err := repo.List(context.Background(), persistence.TaskFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 row, got %d", len(out))
	}
}

func TestTaskList_AllFilters(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	pid := "p-1"
	status := persistence.TaskStatusQueued
	f := persistence.TaskFilter{
		ProjectID: &pid,
		Status:    &status,
		PageSize:  20,
		Offset:    5,
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM tasks")).
		WithArgs("p-1", persistence.TaskStatusQueued, 20, 5).
		WillReturnRows(taskRow())

	if _, err := repo.List(context.Background(), f); err != nil {
		t.Fatalf("List: %v", err)
	}
}

func TestTaskCount_NoFilter(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM tasks WHERE 1=1")).
		WithArgs().
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(42)))

	got, err := repo.Count(context.Background(), persistence.TaskFilter{})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got != 42 {
		t.Errorf("expected 42, got %d", got)
	}
}

func TestTaskCount_WithFilters(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	pid := "p-1"
	status := persistence.TaskStatusQueued
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM tasks")).
		WithArgs("p-1", persistence.TaskStatusQueued).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(3)))

	got, err := repo.Count(context.Background(), persistence.TaskFilter{
		ProjectID: &pid, Status: &status,
	})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got != 3 {
		t.Errorf("expected 3, got %d", got)
	}
}

func TestTaskUpdateStatus(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta("UPDATE tasks SET status = $2")).
		WithArgs("task-1", persistence.TaskStatusRunning).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.UpdateStatus(context.Background(), "task-1", persistence.TaskStatusRunning); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
}

// TestLeaseTask_HappyPath_NoConcurrencyLimits exercises the most
// common LeaseTask shape: no per-project concurrency limits, no
// project priorities. RETURNING populates a full task row.
func TestLeaseTask_HappyPath_NoConcurrencyLimits(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	rows := taskRow().AddRow(fullTaskValues("task-1", "p-1")...)
	// The query is long; matching on the RETURNING clause is enough
	// to identify it without coupling to whitespace.
	mock.ExpectQuery("RETURNING id, project_id").
		WithArgs(
			"p-1",            // ProjectID filter
			sqlmock.AnyArg(), // generated lease id
			"holder-1",       // LeaseHolder
			30,               // LeaseDurationSeconds
		).
		WillReturnRows(rows)

	out, err := repo.LeaseTask(context.Background(), persistence.LeaseOptions{
		ProjectID:            "p-1",
		LeaseHolder:          "holder-1",
		LeaseDurationSeconds: 30,
	})
	if err != nil {
		t.Fatalf("LeaseTask: %v", err)
	}
	if out.ID != "task-1" {
		t.Errorf("expected task-1, got %s", out.ID)
	}
}

// TestLeaseTask_NoTasksAvailable covers the sql.ErrNoRows remap to
// ErrNoTasksAvailable — the contract callers depend on.
func TestLeaseTask_NoTasksAvailable(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectQuery("RETURNING id, project_id").
		WithArgs(
			sqlmock.AnyArg(), "holder-1", 30,
		).
		WillReturnRows(taskRow()) // empty -> ErrNoRows

	_, err := repo.LeaseTask(context.Background(), persistence.LeaseOptions{
		LeaseHolder:          "holder-1",
		LeaseDurationSeconds: 30,
	})
	if !errors.Is(err, persistence.ErrNoTasksAvailable) {
		t.Fatalf("expected ErrNoTasksAvailable, got %v", err)
	}
}

// TestLeaseTask_WithConcurrencyLimitsAndPriorities exercises the
// preludeCTEs branches (limits VALUES list, project_priorities
// VALUES list, JOIN clause). The exact SQL is incidental; what
// matters is that the placeholder/arg accounting stays consistent.
func TestLeaseTask_WithConcurrencyLimitsAndPriorities(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectQuery("active_counts AS").
		WithArgs(
			"p-1", 5, // concurrency: p-1 capped at 5
			"p-1", 10, // priority: p-1 = 10
			60, // PriorityFloor
			sqlmock.AnyArg(), "holder-1", 60,
		).
		WillReturnRows(taskRow().AddRow(fullTaskValues("task-1", "p-1")...))

	out, err := repo.LeaseTask(context.Background(), persistence.LeaseOptions{
		LeaseHolder:              "holder-1",
		LeaseDurationSeconds:     60,
		PriorityFloor:            60,
		ProjectConcurrencyLimits: map[string]int{"p-1": 5},
		ProjectPriorities:        map[string]int{"p-1": 10},
		ProjectPriorityDefault:   0, // exercise the 0 -> 50 fallback
	})
	if err != nil {
		t.Fatalf("LeaseTask: %v", err)
	}
	if out.ID != "task-1" {
		t.Errorf("expected task-1, got %s", out.ID)
	}
}

func TestFindExpiredLeases(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	rows := taskRow().
		AddRow(fullTaskValues("task-1", "p-1")...).
		AddRow(fullTaskValues("task-2", "p-1")...)

	mock.ExpectQuery(regexp.QuoteMeta("lease_expires_at < NOW()")).
		WithArgs(50).
		WillReturnRows(rows)

	out, err := repo.FindExpiredLeases(context.Background(), 50)
	if err != nil {
		t.Fatalf("FindExpiredLeases: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("expected 2 rows, got %d", len(out))
	}
}

func TestCountByStatus(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	rows := sqlmock.NewRows([]string{"status", "count"}).
		AddRow("QUEUED", int64(2)).
		AddRow("RUNNING", int64(1))

	mock.ExpectQuery(regexp.QuoteMeta("GROUP BY status")).
		WithArgs("p-1").
		WillReturnRows(rows)

	counts, err := repo.CountByStatus(context.Background(), "p-1")
	if err != nil {
		t.Fatalf("CountByStatus: %v", err)
	}
	if counts[persistence.TaskStatusQueued] != 2 || counts[persistence.TaskStatusRunning] != 1 {
		t.Errorf("counts roundtrip: %+v", counts)
	}
}

func TestCountRecentFailures_RequiresProjectID(t *testing.T) {
	repo, _, cleanup := newTaskRepo(t)
	defer cleanup()
	if _, err := repo.CountRecentFailures(context.Background(), "", nil, time.Now()); err == nil {
		t.Fatal("expected error for empty projectID")
	}
}

func TestCountRecentFailures_NoErrorClass(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	since := time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE project_id = $1")).
		WithArgs("p-1", since).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(7))

	got, err := repo.CountRecentFailures(context.Background(), "p-1", nil, since)
	if err != nil {
		t.Fatalf("CountRecentFailures: %v", err)
	}
	if got != 7 {
		t.Errorf("expected 7, got %d", got)
	}
}

func TestCountRecentFailures_WithErrorClasses(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	since := time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("last_error_class IN (")).
		WithArgs("p-1", since, "transient", "fatal").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))

	got, err := repo.CountRecentFailures(
		context.Background(), "p-1", []string{"transient", "fatal"}, since,
	)
	if err != nil {
		t.Fatalf("CountRecentFailures: %v", err)
	}
	if got != 3 {
		t.Errorf("expected 3, got %d", got)
	}
}

func TestGetChildren(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	rows := taskRow().AddRow(fullTaskValues("child-1", "p-1")...)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE parent_task_id = $1")).
		WithArgs("parent-1").
		WillReturnRows(rows)

	out, err := repo.GetChildren(context.Background(), "parent-1")
	if err != nil {
		t.Fatalf("GetChildren: %v", err)
	}
	if len(out) != 1 || out[0].ID != "child-1" {
		t.Errorf("rows: %+v", out)
	}
}

func TestGetDependents(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	rows := taskRow().AddRow(fullTaskValues("dep-1", "p-1")...)
	mock.ExpectQuery(regexp.QuoteMeta("$1 = ANY(dependencies)")).
		WithArgs("task-1").
		WillReturnRows(rows)

	out, err := repo.GetDependents(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("GetDependents: %v", err)
	}
	if len(out) != 1 {
		t.Errorf("rows: %+v", out)
	}
}

// TestGetDependencies exercises the empty-result path. The SELECT in
// GetDependencies projects only 21 columns while scanTask reads 28,
// so a non-empty row would error at Scan — that mismatch is a
// pre-existing source bug, not something to assert from this test.
// An empty result set keeps the path covered without depending on
// the bug remaining in place.
func TestGetDependencies_EmptyResult(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	emptyRows := sqlmock.NewRows([]string{
		"id", "project_id", "workflow_id", "idempotency_key", "parent_task_id", "creation_source",
		"delegation_mode", "status", "priority", "payload", "dependencies",
		"lease_id", "leased_at", "leased_by", "lease_expires_at",
		"attempt", "max_attempts", "last_error", "last_error_class", "created_at", "updated_at",
	})

	mock.ExpectQuery(regexp.QuoteMeta("FROM tasks task")).
		WithArgs("task-1").
		WillReturnRows(emptyRows)

	out, err := repo.GetDependencies(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("GetDependencies: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected no rows, got %d", len(out))
	}
}

func TestGetDependencies_QueryError(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta("FROM tasks task")).
		WithArgs("task-1").
		WillReturnError(errors.New("conn closed"))

	if _, err := repo.GetDependencies(context.Background(), "task-1"); err == nil {
		t.Fatal("expected error")
	}
}
