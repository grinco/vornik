package postgres

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"vornik.io/vornik/internal/persistence"
)

// TestCrossProjectCall_Create_StampsDefaults asserts a row
// inserted without an id or status picks up generated values
// rather than landing as empty strings. ID prefix follows the
// "ccp_" convention from persistence.GenerateID.
func TestCrossProjectCall_Create_StampsDefaults(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewCrossProjectCallRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO cross_project_calls")).
		WithArgs(
			sqlmock.AnyArg(), // id
			"task-caller", "step-1", "marketing",
			"architect", "produce-spec", nil, // callee_task_id NULL
			sqlmock.AnyArg(), // payload jsonb
			"spec_envelope.v1", string(persistence.CPCStatusPending),
			nil,   // timeout_at NULL
			false, // cancel_on_timeout default
		).WillReturnResult(sqlmock.NewResult(0, 1))

	c := &persistence.CrossProjectCall{
		CallerTaskID:   "task-caller",
		CallerStepID:   "step-1",
		CallerProject:  "marketing",
		CalleeProject:  "architect",
		CalleeWorkflow: "produce-spec",
		Payload:        []byte(`{"brief":"launch Q3"}`),
		ExpectedSchema: "spec_envelope.v1",
	}
	if err := repo.Create(context.Background(), c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if c.ID == "" {
		t.Error("Create should have stamped ID")
	}
	if c.Status != persistence.CPCStatusPending {
		t.Errorf("status = %q, want pending", c.Status)
	}
}

// TestCrossProjectCall_SetCalleeTaskID_RejectsRebind asserts
// the row's callee_task_id is set exactly once. A second
// SetCalleeTaskID for the same row returns ErrNotFound (the
// UPDATE matches 0 rows because callee_task_id IS NULL is
// no longer true). This protects against double-spawn races.
func TestCrossProjectCall_SetCalleeTaskID_RejectsRebind(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewCrossProjectCallRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE cross_project_calls")).
		WithArgs("ccp_1", "task-callee").
		WillReturnResult(sqlmock.NewResult(0, 0)) // 0 rows updated

	err := repo.SetCalleeTaskID(context.Background(), "ccp_1", "task-callee")
	if err != persistence.ErrNotFound {
		t.Errorf("SetCalleeTaskID rebind = %v, want ErrNotFound", err)
	}
}

// TestCrossProjectCall_MarkCompleted_OnlyFromNonTerminal asserts
// the resolve path doesn't override an already-resolved row.
// Defense against: a callee task that terminates twice (retry-
// from-step pattern) shouldn't overwrite the first envelope.
func TestCrossProjectCall_MarkCompleted_OnlyFromNonTerminal(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewCrossProjectCallRepository(db)

	// Production query guards with `status NOT IN ('completed',
	// 'failed', 'timed_out', 'rejected')`; we assert the SQL
	// shape so a future edit that drops the guard fails this
	// test.
	mock.ExpectExec(`status NOT IN \('completed', 'failed', 'timed_out', 'rejected'\)`).
		WithArgs("ccp_1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.MarkCompleted(context.Background(), "ccp_1", []byte(`{"schema":"x"}`)); err != nil {
		t.Fatalf("MarkCompleted: %v", err)
	}
}

// TestCrossProjectCall_MarkRejected_DistinctFromFailed asserts
// rejected is a separate terminal state — the caller's
// on_failure branch may want to differentiate "schema
// mismatch / acceptCallsFrom denial" (rejected) from "callee
// task ran but errored" (failed). Both call MarkXxx; both
// flip resolved_at; the status string differs.
func TestCrossProjectCall_MarkRejected_DistinctFromFailed(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewCrossProjectCallRepository(db)

	mock.ExpectExec(`status = 'rejected'`).
		WithArgs("ccp_1", "acceptCallsFrom rejects marketing").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.MarkRejected(context.Background(), "ccp_1", "acceptCallsFrom rejects marketing"); err != nil {
		t.Fatalf("MarkRejected: %v", err)
	}
}

// TestCrossProjectCall_Get_NotFound asserts ErrNotFound is
// returned for missing rows (the executor's resolve hook
// uses Get on every callee terminal — most tasks AREN'T CPC
// callees, so the NotFound path runs constantly).
func TestCrossProjectCall_Get_NotFound(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewCrossProjectCallRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, caller_task_id")).
		WithArgs("ccp_missing").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "caller_task_id", "caller_step_id", "caller_project",
			"callee_project", "callee_workflow", "callee_task_id",
			"payload", "expected_schema", "status", "result_envelope",
			"error_message", "timeout_at", "created_at", "resolved_at",
		}))

	_, err := repo.Get(context.Background(), "ccp_missing")
	if err != persistence.ErrNotFound {
		t.Errorf("Get missing = %v, want ErrNotFound", err)
	}
}
