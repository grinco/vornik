package postgres

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"vornik.io/vornik/internal/persistence"
)

// TestExecutionHintRepository_ListByExecution_HasLimit asserts
// the list query carries a LIMIT clause bound to maxHintListRows.
// Without the cap, a caller that inserts many small hint rows
// can OOM the daemon on the subsequent GET.
//
// Reversion sentinel: deleting the LIMIT or the maxHintListRows
// constant fails this test loud.
func TestExecutionHintRepository_ListByExecution_HasLimit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Expect a query containing both LIMIT and the cap value bound
	// as $2. If a future edit removes the LIMIT, the regex stops
	// matching and the test fails before any rows are returned.
	mock.ExpectQuery(regexp.QuoteMeta("LIMIT $2")).
		WithArgs("exec_1", maxHintListRows).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "task_id", "execution_id", "step_id", "content",
			"applied_at", "created_at", "created_by",
		}))

	repo := &ExecutionHintRepository{db: db}
	hints, err := repo.ListByExecution(context.Background(), "exec_1")
	if err != nil {
		t.Fatalf("ListByExecution: %v", err)
	}
	if len(hints) != 0 {
		t.Fatalf("expected 0 hints, got %d", len(hints))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations unmet: %v", err)
	}
}

// TestExecutionHintRepository_ListForExecution_IncludesTaskScope
// asserts the live-view query unions execution-scoped AND task-scoped
// (execution_id IS NULL) hints, binding execution_id=$1, task_id=$2,
// and the row cap as $3 (LLD-drift audit §8.6).
func TestExecutionHintRepository_ListForExecution_IncludesTaskScope(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(regexp.QuoteMeta("(task_id = $2 AND execution_id IS NULL)")).
		WithArgs("exec_1", "task_99", maxHintListRows).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "task_id", "execution_id", "step_id", "content",
			"applied_at", "created_at", "created_by",
		}).
			AddRow("h_exec", nil, "exec_1", nil, "exec scoped", nil, nowForTest(), "op").
			AddRow("h_task", "task_99", nil, nil, "task scoped", nil, nowForTest(), "op"))

	repo := &ExecutionHintRepository{db: db}
	hints, err := repo.ListForExecution(context.Background(), "exec_1", "task_99")
	if err != nil {
		t.Fatalf("ListForExecution: %v", err)
	}
	if len(hints) != 2 {
		t.Fatalf("expected 2 hints (exec + task scoped), got %d", len(hints))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations unmet: %v", err)
	}
}

// TestExecutionHintRepository_ListForExecution_RequiresExecutionID
// — an empty execution id is rejected before any query.
func TestExecutionHintRepository_ListForExecution_RequiresExecutionID(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := &ExecutionHintRepository{db: db}
	if _, err := repo.ListForExecution(context.Background(), "", "task_99"); err == nil {
		t.Error("expected error on empty execution id")
	}
}

// TestExecutionHintRepository_Insert_RejectsBothScopes — Insert must
// reject a hint that sets BOTH task_id and execution_id; the scope
// has to be unambiguous so ConsumePending knows which class of rows
// it's draining.
func TestExecutionHintRepository_Insert_RejectsBothScopes(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	repo := &ExecutionHintRepository{db: db}
	err = repo.Insert(context.Background(), &persistence.ExecutionHint{
		ID:          "h_1",
		TaskID:      "task_1",
		ExecutionID: "exec_1",
		Content:     "x",
	})
	if err == nil {
		t.Fatal("expected error when both task_id and execution_id are set")
	}
}

// TestExecutionHintRepository_Insert_RejectsNeitherScope — at least
// one of task_id / execution_id must be set; a hint with neither is
// orphaned and can never be consumed.
func TestExecutionHintRepository_Insert_RejectsNeitherScope(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	repo := &ExecutionHintRepository{db: db}
	err = repo.Insert(context.Background(), &persistence.ExecutionHint{
		ID:      "h_1",
		Content: "x",
	})
	if err == nil {
		t.Fatal("expected error when neither task_id nor execution_id is set")
	}
}

// TestExecutionHintRepository_ConsumePending_RequiresScope — the
// consume path is symmetric: must be told what to consume.
func TestExecutionHintRepository_ConsumePending_RequiresScope(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	repo := &ExecutionHintRepository{db: db}
	_, err = repo.ConsumePending(context.Background(), "", "", "")
	if err == nil {
		t.Fatal("expected error when both scope arguments are blank")
	}
}

// TestExecutionHintRepository_ListPendingForTask_FiltersExecutionScoped —
// task-pending must NOT return execution-scoped rows (execution_id IS
// NOT NULL), even if those rows are themselves still pending. Used
// by the UI to render "queued for next execution" specifically.
func TestExecutionHintRepository_ListPendingForTask_FiltersExecutionScoped(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	// Query body must filter execution_id IS NULL — sentinel for the
	// task-only scope.
	mock.ExpectQuery(regexp.QuoteMeta("execution_id IS NULL")).
		WithArgs("task_xyz", maxHintListRows).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "task_id", "execution_id", "step_id", "content",
			"applied_at", "created_at", "created_by",
		}))
	repo := &ExecutionHintRepository{db: db}
	if _, err := repo.ListPendingForTask(context.Background(), "task_xyz"); err != nil {
		t.Fatalf("ListPendingForTask: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations unmet: %v", err)
	}
}

// TestExecutionHintRepository_MaxRowsConstantSane guards against
// someone bumping the cap absurdly high (e.g. to math.MaxInt32
// in a "fix the LIMIT failure" reflex). The realistic operator
// hint count per execution is under 50; 500 leaves generous
// headroom; anything over a few thousand defeats the point of
// the cap.
func TestExecutionHintRepository_MaxRowsConstantSane(t *testing.T) {
	if maxHintListRows < 1 {
		t.Fatalf("maxHintListRows = %d; must be positive", maxHintListRows)
	}
	if maxHintListRows > 10000 {
		t.Fatalf("maxHintListRows = %d; cap raised beyond sane range — defeats DoS guard", maxHintListRows)
	}
}
