package postgres

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"vornik.io/vornik/internal/persistence"
)

// newTaskRepo wires a TaskRepository against a sqlmock-backed *sql.DB.
func newTaskRepo(t *testing.T) (*TaskRepository, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return NewTaskRepository(db), mock, func() { _ = db.Close() }
}

// ---------- ReleaseLease ----------

func TestReleaseLease_RejectsEmptyLeaseID(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	err := repo.ReleaseLease(
		context.Background(), "task-1", "",
		persistence.TaskStatusCompleted, persistence.ReleaseOptions{},
	)
	if err == nil || !strings.Contains(err.Error(), "leaseID required") {
		t.Fatalf("expected leaseID-required error, got %v", err)
	}
	// No SQL must have been issued — the guard runs before db.ExecContext.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("did not expect any DB call: %v", err)
	}
}

func TestReleaseLease_HappyPath(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta("UPDATE tasks")).
		WithArgs(
			"task-1", "lease-abc",
			persistence.TaskStatusCompleted,
			0, 0, "", "",
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := repo.ReleaseLease(
		context.Background(), "task-1", "lease-abc",
		persistence.TaskStatusCompleted, persistence.ReleaseOptions{},
	)
	if err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestReleaseLease_PassesAllReleaseOptions confirms attempt /
// max_attempts / error / error_class travel into the right
// placeholder slots. ReleaseOptions is the kind of all-pointer-or-
// all-by-name struct that masks transposition bugs at compile time
// — a positional SQL binding still needs an explicit pin.
func TestReleaseLease_PassesAllReleaseOptions(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta("UPDATE tasks")).
		WithArgs(
			"task-1", "lease-abc",
			persistence.TaskStatusFailed,
			3, 5, "boom", "transient",
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := repo.ReleaseLease(
		context.Background(), "task-1", "lease-abc",
		persistence.TaskStatusFailed,
		persistence.ReleaseOptions{
			Attempt: 3, MaxAttempts: 5,
			Error: "boom", ErrorClass: "transient",
		},
	)
	if err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestReleaseLease_PropagatesDuplicateKey(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta("UPDATE tasks")).
		WillReturnError(&pq.Error{Code: "23505"})

	err := repo.ReleaseLease(
		context.Background(), "task-1", "lease-abc",
		persistence.TaskStatusCompleted, persistence.ReleaseOptions{},
	)
	if !errors.Is(err, persistence.ErrDuplicateKey) {
		t.Fatalf("expected ErrDuplicateKey, got %v", err)
	}
}

// ---------- RequeueTerminalTask ----------

func TestRequeueTerminalTask_TerminalRowRequeued(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta("status            = 'QUEUED'")).
		WithArgs("task-1", 2, 5).
		WillReturnResult(sqlmock.NewResult(0, 1))

	ok, err := repo.RequeueTerminalTask(context.Background(), "task-1", 2, 5)
	if err != nil {
		t.Fatalf("RequeueTerminalTask: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true when one row was requeued")
	}
}

// TestRequeueTerminalTask_NoMatchReturnsFalse covers the atomic
// guard: when the row is no longer terminal (someone else won the
// race), RowsAffected = 0 and the caller sees (false, nil) instead
// of a silently-no-op nil error.
func TestRequeueTerminalTask_NoMatchReturnsFalse(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta("FAILED")).
		WithArgs("task-1", 1, 3).
		WillReturnResult(sqlmock.NewResult(0, 0))

	ok, err := repo.RequeueTerminalTask(context.Background(), "task-1", 1, 3)
	if err != nil {
		t.Fatalf("RequeueTerminalTask: %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false when no row matched the terminal-state guard")
	}
}

func TestRequeueTerminalTask_ExecError(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectExec("UPDATE tasks").
		WillReturnError(errors.New("conn closed"))

	ok, err := repo.RequeueTerminalTask(context.Background(), "task-1", 1, 3)
	if err == nil || !strings.Contains(err.Error(), "conn closed") {
		t.Fatalf("expected raw DB error, got %v", err)
	}
	if ok {
		t.Errorf("ok should be false on error")
	}
}

func TestRequeueTerminalTask_RowsAffectedError(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectExec("UPDATE tasks").
		WillReturnResult(sqlmock.NewErrorResult(errors.New("rowcount unavailable")))

	ok, err := repo.RequeueTerminalTask(context.Background(), "task-1", 1, 3)
	if err == nil || !strings.Contains(err.Error(), "rowcount unavailable") {
		t.Fatalf("expected RowsAffected error to surface, got %v", err)
	}
	if ok {
		t.Errorf("ok should be false on error")
	}
}

// ---------- TransitionConditional ----------

func TestTransitionConditional_RequiresArgs(t *testing.T) {
	repo, _, cleanup := newTaskRepo(t)
	defer cleanup()

	cases := []struct {
		name string
		id   string
		to   persistence.TaskStatus
		from []persistence.TaskStatus
	}{
		{"empty id", "", "DONE", []persistence.TaskStatus{"QUEUED"}},
		{"empty to", "task-1", "", []persistence.TaskStatus{"QUEUED"}},
		{"empty from", "task-1", "DONE", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := repo.TransitionConditional(
				context.Background(), tc.id, tc.from, tc.to, persistence.TransitionOpts{},
			)
			if err == nil || !strings.Contains(err.Error(), "id, from, to required") {
				t.Errorf("expected validation error, got %v", err)
			}
			if ok {
				t.Errorf("expected ok=false on validation error")
			}
		})
	}
}

// TestTransitionConditional_MinimalSet exercises the no-opts path:
// SET stays at "status = $1, updated_at = NOW()" and the WHERE
// clause uses ANY($N) with the from-array passed via pq.Array.
func TestTransitionConditional_MinimalSet(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta("UPDATE tasks SET status = $1, updated_at = NOW()")).
		WithArgs(
			"COMPLETED", "task-1",
			pq.Array([]string{"RUNNING", "LEASED"}),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	ok, err := repo.TransitionConditional(
		context.Background(), "task-1",
		[]persistence.TaskStatus{"RUNNING", "LEASED"},
		"COMPLETED",
		persistence.TransitionOpts{},
	)
	if err != nil {
		t.Fatalf("TransitionConditional: %v", err)
	}
	if !ok {
		t.Errorf("expected ok=true on successful transition")
	}
}

// TestTransitionConditional_ZeroRowsMeansNoTransition is the
// concurrency race: another writer already moved the row, so the
// guard's WHERE clause skips this UPDATE. Must surface as
// (false, nil), not (true, nil) and not an error.
func TestTransitionConditional_ZeroRowsMeansNoTransition(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectExec("UPDATE tasks").
		WillReturnResult(sqlmock.NewResult(0, 0))

	ok, err := repo.TransitionConditional(
		context.Background(), "task-1",
		[]persistence.TaskStatus{"RUNNING"}, "COMPLETED",
		persistence.TransitionOpts{},
	)
	if err != nil {
		t.Fatalf("TransitionConditional: %v", err)
	}
	if ok {
		t.Errorf("expected ok=false when no row matched")
	}
}

// TestTransitionConditional_AllOpts pins the dynamic SET-clause
// builder. Each non-nil opt adds its column; the test asserts both
// the SQL fragments and the *positional* arg order so a refactor
// that reshuffles the if-chain shows up immediately.
func TestTransitionConditional_AllOpts(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	closedBy := "operator-1"
	expectedBy := mustTime(t, "2026-06-01T00:00:00Z")
	phase := "discovery"
	briefAmendedAt := mustTime(t, "2026-05-20T12:00:00Z")
	lastErr := "downstream-timeout"
	lastErrClass := "transient"

	opts := persistence.TransitionOpts{
		SetClosedAtNow: true,
		ClosedBy:       &closedBy,
		ExpectedBy:     &expectedBy,
		CurrentPhase:   &phase,
		BriefAmendedAt: &briefAmendedAt,
		LastError:      &lastErr,
		LastErrorClass: &lastErrClass,
		ClearLease:     true,
	}

	// Args follow the in-source if-chain order: status ($1), then
	// each opt in turn, then id, then the from-array. SetClosedAtNow
	// and ClearLease emit literal NOW() / NULLs, so they don't
	// consume placeholders.
	mock.ExpectExec(regexp.QuoteMeta("UPDATE tasks SET")).
		WithArgs(
			"CLOSED",                      // $1 status
			closedBy,                      // $2 closed_by
			expectedBy,                    // $3 expected_by
			phase,                         // $4 current_phase
			briefAmendedAt,                // $5 brief_amended_at
			lastErr,                       // $6 last_error
			lastErrClass,                  // $7 last_error_class
			"task-1",                      // $8 id
			pq.Array([]string{"RUNNING"}), // $9 from-array
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	ok, err := repo.TransitionConditional(
		context.Background(), "task-1",
		[]persistence.TaskStatus{"RUNNING"}, "CLOSED", opts,
	)
	if err != nil {
		t.Fatalf("TransitionConditional: %v", err)
	}
	if !ok {
		t.Errorf("expected ok=true on transition")
	}

	// Independently assert the SET-clause contents: the regex matcher
	// only checked a fragment, so any missing column would have passed
	// arg validation but landed in the wrong shape. Re-run the path
	// against a custom matcher that captures the actual SQL.
	verifyTransitionConditionalSQL(t, opts, []string{
		"status = $1",
		"closed_at = NOW()",
		"closed_by = $2",
		"expected_by = $3",
		"current_phase = $4",
		"brief_amended_at = $5",
		"last_error = $6",
		"last_error_class = $7",
		"lease_id = NULL",
		"leased_at = NULL",
		"leased_by = NULL",
		"lease_expires_at = NULL",
		"WHERE id = $8",
		"AND status = ANY($9)",
	})
}

// TestTransitionConditional_PartialOpts walks a representative
// subset of single-opt invocations to lock down the dynamic
// placeholder numbering. Each sub-test isolates one opt.
func TestTransitionConditional_PartialOpts(t *testing.T) {
	cases := []struct {
		name      string
		opts      persistence.TransitionOpts
		expectSQL []string
	}{
		{
			name: "SetClosedAtNow only",
			opts: persistence.TransitionOpts{SetClosedAtNow: true},
			expectSQL: []string{
				"status = $1", "closed_at = NOW()", "WHERE id = $2", "AND status = ANY($3)",
			},
		},
		{
			name: "ClosedBy only",
			opts: persistence.TransitionOpts{ClosedBy: strPtrLocal("ops")},
			expectSQL: []string{
				"status = $1", "closed_by = $2", "WHERE id = $3", "AND status = ANY($4)",
			},
		},
		{
			name: "ExpectedBy only",
			opts: persistence.TransitionOpts{ExpectedBy: timePtrLocal(t, "2026-06-01T00:00:00Z")},
			expectSQL: []string{
				"status = $1", "expected_by = $2", "WHERE id = $3", "AND status = ANY($4)",
			},
		},
		{
			name: "CurrentPhase only",
			opts: persistence.TransitionOpts{CurrentPhase: strPtrLocal("discovery")},
			expectSQL: []string{
				"status = $1", "current_phase = $2", "WHERE id = $3", "AND status = ANY($4)",
			},
		},
		{
			name: "BriefAmendedAt only",
			opts: persistence.TransitionOpts{BriefAmendedAt: timePtrLocal(t, "2026-06-01T00:00:00Z")},
			expectSQL: []string{
				"status = $1", "brief_amended_at = $2", "WHERE id = $3", "AND status = ANY($4)",
			},
		},
		{
			name: "LastError only",
			opts: persistence.TransitionOpts{LastError: strPtrLocal("boom")},
			expectSQL: []string{
				"status = $1", "last_error = $2", "WHERE id = $3", "AND status = ANY($4)",
			},
		},
		{
			name: "LastErrorClass only",
			opts: persistence.TransitionOpts{LastErrorClass: strPtrLocal("transient")},
			expectSQL: []string{
				"status = $1", "last_error_class = $2", "WHERE id = $3", "AND status = ANY($4)",
			},
		},
		{
			name: "ClearLease only",
			opts: persistence.TransitionOpts{ClearLease: true},
			expectSQL: []string{
				"status = $1",
				"lease_id = NULL", "leased_at = NULL",
				"leased_by = NULL", "lease_expires_at = NULL",
				"WHERE id = $2", "AND status = ANY($3)",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verifyTransitionConditionalSQL(t, tc.opts, tc.expectSQL)
		})
	}
}

// TestTransitionConditional_ExecError ensures DB errors are mapped
// through mapDBError (here pq.Error 23503 -> ErrNotFound).
func TestTransitionConditional_ExecError(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectExec("UPDATE tasks").
		WillReturnError(&pq.Error{Code: "23503"})

	ok, err := repo.TransitionConditional(
		context.Background(), "task-1",
		[]persistence.TaskStatus{"RUNNING"}, "COMPLETED",
		persistence.TransitionOpts{},
	)
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if ok {
		t.Errorf("ok should be false on error")
	}
}

// TestTransitionConditional_RowsAffectedError covers the seldom-
// touched second error branch: the UPDATE succeeded but the driver
// can't report rows.affected. Surface the raw error rather than
// fabricating success.
func TestTransitionConditional_RowsAffectedError(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectExec("UPDATE tasks").
		WillReturnResult(sqlmock.NewErrorResult(errors.New("rowcount unavailable")))

	ok, err := repo.TransitionConditional(
		context.Background(), "task-1",
		[]persistence.TaskStatus{"RUNNING"}, "COMPLETED",
		persistence.TransitionOpts{},
	)
	if err == nil || !strings.Contains(err.Error(), "rowcount unavailable") {
		t.Fatalf("expected RowsAffected error to surface, got %v", err)
	}
	if ok {
		t.Errorf("ok should be false on error")
	}
}

// ---------- TransitionToCancelled ----------

// TransitionToCancelled and ReleaseLease share the rows-affected
// pattern; cover the same two outcomes so a refactor that breaks
// one doesn't pass the other's test silently.

func TestTransitionToCancelled_RowsMatchedReturnsTrue(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta("status = 'CANCELLED'")).
		WithArgs("task-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	ok, err := repo.TransitionToCancelled(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("TransitionToCancelled: %v", err)
	}
	if !ok {
		t.Errorf("expected ok=true")
	}
}

func TestTransitionToCancelled_NoRowsReturnsFalse(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectExec("UPDATE tasks").
		WithArgs("task-1").
		WillReturnResult(sqlmock.NewResult(0, 0))

	ok, err := repo.TransitionToCancelled(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("TransitionToCancelled: %v", err)
	}
	if ok {
		t.Errorf("expected ok=false when terminal-state guard blocked the write")
	}
}

// ---------- shared helpers ----------

func strPtrLocal(s string) *string { return &s }

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return tm
}

func timePtrLocal(t *testing.T, s string) *time.Time {
	t.Helper()
	tm := mustTime(t, s)
	return &tm
}

// verifyTransitionConditionalSQL re-issues a TransitionConditional
// call against a sqlmock with QueryMatcherFunc that captures the
// real SQL, then asserts every expected fragment is present in
// order. Independent of arg matching so the SET-clause shape is
// pinned even when args are unchanged.
func verifyTransitionConditionalSQL(
	t *testing.T,
	opts persistence.TransitionOpts,
	expectFragments []string,
) {
	t.Helper()

	var capturedSQL string
	matcher := sqlmock.QueryMatcherFunc(func(_, actualSQL string) error {
		capturedSQL = actualSQL
		return nil
	})
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	repo := NewTaskRepository(db)

	mock.ExpectExec("ignored").WillReturnResult(sqlmock.NewResult(0, 1))

	_, err = repo.TransitionConditional(
		context.Background(), "task-1",
		[]persistence.TaskStatus{"RUNNING"}, "COMPLETED", opts,
	)
	if err != nil {
		t.Fatalf("TransitionConditional: %v", err)
	}

	for _, frag := range expectFragments {
		if !strings.Contains(capturedSQL, frag) {
			t.Errorf("expected SQL fragment %q not found in:\n%s", frag, capturedSQL)
		}
	}
}
