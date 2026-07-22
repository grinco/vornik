package persistence

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestCASToSubmitting_SingleWinner pins the exact approved→submitting CAS SQL
// and that a single changed row yields (true, nil) — the winner of the C3
// double-submit guard.
func TestCASToSubmitting_SingleWinner(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	r := NewWebWriteRepo(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE web_write_actions SET status='submitting', token_consumed_at=NOW() WHERE submission_id=$1 AND status='approved'")).
		WithArgs("s1").WillReturnResult(sqlmock.NewResult(0, 1))

	ok, err := r.CASToSubmitting(context.Background(), "s1")
	if err != nil || !ok {
		t.Fatalf("want ok=true, got ok=%v err=%v", ok, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestCASToSubmitting_LosesRace: the same CAS SQL, but 0 rows changed → the row
// was not in 'approved' (another caller already won) → (false, nil).
func TestCASToSubmitting_LosesRace(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	r := NewWebWriteRepo(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE web_write_actions SET status='submitting', token_consumed_at=NOW() WHERE submission_id=$1 AND status='approved'")).
		WithArgs("s1").WillReturnResult(sqlmock.NewResult(0, 0))

	ok, err := r.CASToSubmitting(context.Background(), "s1")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ok {
		t.Fatal("want ok=false when row not in approved state")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestWebWriteCreate pins the INSERT column list and that SubmissionID + Status
// default when left zero.
func TestWebWriteCreate(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	r := NewWebWriteRepo(db)

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO web_write_actions")).
		WithArgs(
			sqlmock.AnyArg(), // submission_id defaulted
			"proj-1",
			nil, // task_id "" → NULL
			nil, // agent_run_id "" → NULL
			"https://claims.airline.com/x",
			"claims.airline.com",
			[]byte(`{"name":"x"}`),
			[]byte(`{"name":"#name"}`),
			[]byte(`[]`),
			[]byte(`[]`),
			nil, // screenshot_ref "" → NULL
			nil, // confirmation_ref "" → NULL
			"pending",
			nil, // approval_token_hash "" → NULL
			false,
			nil,              // approver "" → NULL
			sqlmock.AnyArg(), // created_at defaulted
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	a := &WebWriteAction{
		ProjectID:        "proj-1",
		TargetURL:        "https://claims.airline.com/x",
		TargetHost:       "claims.airline.com",
		PayloadJSON:      []byte(`{"name":"x"}`),
		SelectorBindings: []byte(`{"name":"#name"}`),
		FieldTableJSON:   []byte(`[]`),
		VolatileFields:   []byte(`[]`),
	}
	if err := r.Create(context.Background(), a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if a.SubmissionID == "" {
		t.Error("Create should default SubmissionID")
	}
	if a.Status != "pending" {
		t.Errorf("Create should default Status=pending, got %q", a.Status)
	}
	if a.CreatedAt.IsZero() {
		t.Error("Create should default CreatedAt")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestWebWriteGet pins the SELECT and the row scan (incl. nullable columns).
func TestWebWriteGet(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	r := NewWebWriteRepo(db)

	rows := sqlmock.NewRows([]string{
		"submission_id", "project_id", "task_id", "agent_run_id", "target_url", "target_host",
		"payload_json", "selector_bindings", "field_table_json", "volatile_fields",
		"screenshot_ref", "confirmation_ref", "status", "approval_token_hash",
		"insecure_bypass", "approver", "created_at", "decided_at", "submitted_at", "token_consumed_at",
	}).AddRow(
		"s1", "proj-1", "task-9", "run-3", "https://claims.airline.com/x", "claims.airline.com",
		[]byte(`{"name":"x"}`), []byte(`{}`), []byte(`[]`), []byte(`[]`),
		"art://shot", nil, "approved", "hash-1",
		true, "operator@x", time.Now().UTC(), nil, nil, nil,
	)
	mock.ExpectQuery(regexp.QuoteMeta("FROM web_write_actions")).
		WithArgs("s1").WillReturnRows(rows)

	a, err := r.Get(context.Background(), "s1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if a.SubmissionID != "s1" || a.TaskID != "task-9" || a.AgentRunID != "run-3" {
		t.Errorf("scan mismatch: %+v", a)
	}
	if a.Status != "approved" || a.ApprovalTokenHash != "hash-1" || !a.InsecureBypass {
		t.Errorf("scan mismatch on status/hash/bypass: %+v", a)
	}
	if a.ScreenshotRef != "art://shot" || a.ConfirmationRef != "" || a.Approver != "operator@x" {
		t.Errorf("scan mismatch on refs/approver: %+v", a)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestWebWriteApprove pins the guarded pending→approved CAS and its args.
func TestWebWriteApprove(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	r := NewWebWriteRepo(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE web_write_actions SET status='approved', approval_token_hash=$2, approver=$3, decided_at=NOW() WHERE submission_id=$1 AND status='pending'")).
		WithArgs("s1", "hash-1", "operator@x").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := r.Approve(context.Background(), "s1", "hash-1", "operator@x"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestWebWriteApprove_NoTransition: 0 rows (row not pending) → ErrNoTransition.
func TestWebWriteApprove_NoTransition(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	r := NewWebWriteRepo(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE web_write_actions SET status='approved'")).
		WithArgs("s1", "hash-1", "operator@x").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := r.Approve(context.Background(), "s1", "hash-1", "operator@x"); !errors.Is(err, ErrNoTransition) {
		t.Fatalf("want ErrNoTransition, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestWebWriteFinalize pins the submitting→submitted CAS and rejects an invalid
// target status without touching the DB.
func TestWebWriteFinalize(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	r := NewWebWriteRepo(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE web_write_actions SET status=$2, submitted_at=NOW() WHERE submission_id=$1 AND status='submitting'")).
		WithArgs("s1", "submitted").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := r.Finalize(context.Background(), "s1", "submitted"); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	// Invalid target status is rejected before any DB call (no expectation set).
	if err := r.Finalize(context.Background(), "s1", "approved"); err == nil {
		t.Fatal("Finalize should reject an invalid target status")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestWebWriteResolve pins the (unknown|submitting)→failed operator-recovery CAS.
func TestWebWriteResolve(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	r := NewWebWriteRepo(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE web_write_actions SET status=$2, submitted_at=NOW() WHERE submission_id=$1 AND status IN ('unknown','submitting')")).
		WithArgs("s1", "failed").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := r.Resolve(context.Background(), "s1", "failed"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := r.Resolve(context.Background(), "s1", "unknown"); err == nil {
		t.Fatal("Resolve should reject an invalid target status")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestWebWriteReject pins the (pending|approved)→rejected CAS and its args.
func TestWebWriteReject(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	r := NewWebWriteRepo(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE web_write_actions SET status='rejected', approver=$2, decided_at=NOW() WHERE submission_id=$1 AND status IN ('pending','approved')")).
		WithArgs("s1", "operator@x").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := r.Reject(context.Background(), "s1", "operator@x"); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Guard: the repo satisfies the WebWriteRepo interface and Get surfaces
// sql.ErrNoRows unchanged (callers distinguish "absent" from a driver error).
var _ WebWriteRepo = (*webWriteRepo)(nil)

func TestWebWriteGet_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	r := NewWebWriteRepo(db)

	mock.ExpectQuery(regexp.QuoteMeta("FROM web_write_actions")).
		WithArgs("nope").WillReturnError(sql.ErrNoRows)

	if _, err := r.Get(context.Background(), "nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("want sql.ErrNoRows, got %v", err)
	}
}
