package postgres

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"

	"vornik.io/vornik/internal/persistence"
)

func newWorkflowProposalFixture() *persistence.WorkflowProposal {
	return &persistence.WorkflowProposal{
		ID:             "wpr-1",
		WorkflowID:     "wf-research",
		Status:         persistence.WorkflowProposalStatusPending,
		Kind:           persistence.WorkflowProposalKindAddStep,
		ProposalYAML:   "steps:\n  - id: a\n",
		Motivation:     "judge-fail rate 32% over 9 runs",
		EvidenceRunIDs: []string{"run-1", "run-2", "run-3"},
		InstinctIDs:    []string{"inst_verify-before-review"},
		Confidence:     0.74,
		ArchitectModel: "qwen3.6:35b",
		CreatedAt:      time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC),
	}
}

// TestWorkflowProposalRepository_Insert_HappyPath — INSERT pins the
// column ordering migration 65 depends on. Empty Notes drops to NULL.
func TestWorkflowProposalRepository_Insert_HappyPath(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewWorkflowProposalRepository(db)

	p := newWorkflowProposalFixture()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO workflow_proposals")).
		WithArgs(p.ID, p.WorkflowID, "pending", "add_step", p.ProposalYAML, p.Motivation,
			pq.Array(p.EvidenceRunIDs), pq.Array(p.InstinctIDs), p.Confidence, p.ArchitectModel,
			p.CreatedAt, sql.NullString{}).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Insert(context.Background(), p); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestWorkflowProposalRepository_List_KindFilter — the Kinds filter
// adds a `kind = ANY($n)` predicate. Pins the §8.5 read path.
func TestWorkflowProposalRepository_List_KindFilter(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewWorkflowProposalRepository(db)

	cols := []string{
		"id", "workflow_id", "status", "kind", "proposal_yaml", "motivation",
		"evidence_run_ids", "instinct_ids", "confidence", "architect_model", "created_at",
		"decided_at", "decided_by", "applied_at", "applied_commit",
		"rollback_commit", "notes",
	}
	rows := sqlmock.NewRows(cols).
		AddRow("wpr-k", "wf-x", "pending", "add_step", "", "", pq.Array([]string{}), nil, float32(0.6), "m", time.Now(),
			nil, nil, nil, nil, nil, nil)

	mock.ExpectQuery(regexp.QuoteMeta("kind = ANY(")).
		WithArgs(pq.Array([]string{"add_step"}), 50).
		WillReturnRows(rows)

	got, err := repo.List(context.Background(), persistence.WorkflowProposalFilter{
		Kinds: []persistence.WorkflowProposalKind{persistence.WorkflowProposalKindAddStep},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Kind != persistence.WorkflowProposalKindAddStep {
		t.Fatalf("kind filter result wrong: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestWorkflowProposalRepository_Insert_RateLimit — a 23505 hitting
// the partial unique index `..._pending` MUST surface as
// ErrProposalRateLimited so the admin endpoint returns 429 (not 500
// or a generic ErrDuplicateKey, which would be confusing).
func TestWorkflowProposalRepository_Insert_RateLimit(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewWorkflowProposalRepository(db)

	p := newWorkflowProposalFixture()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO workflow_proposals")).
		WillReturnError(&pq.Error{Code: "23505", Constraint: "uq_workflow_proposals_pending"})

	err := repo.Insert(context.Background(), p)
	if !errors.Is(err, persistence.ErrProposalRateLimited) {
		t.Fatalf("want ErrProposalRateLimited, got %v", err)
	}
}

// TestWorkflowProposalRepository_Insert_DuplicateID — a 23505 on the
// primary key (constraint name without "pending") must NOT be
// confused for a rate-limit. ErrDuplicateKey is the right error.
func TestWorkflowProposalRepository_Insert_DuplicateID(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewWorkflowProposalRepository(db)

	p := newWorkflowProposalFixture()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO workflow_proposals")).
		WillReturnError(&pq.Error{Code: "23505", Constraint: "workflow_proposals_pkey"})

	err := repo.Insert(context.Background(), p)
	if !errors.Is(err, persistence.ErrDuplicateKey) {
		t.Fatalf("want ErrDuplicateKey, got %v", err)
	}
}

// TestWorkflowProposalRepository_Insert_AppliesDefaults — caller
// can leave Status + CreatedAt unset; the repo fills them with
// pending + NOW(). Pins that contract so a future "make these
// strict" change has to update this test.
func TestWorkflowProposalRepository_Insert_AppliesDefaults(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewWorkflowProposalRepository(db)

	p := &persistence.WorkflowProposal{
		ID:             "wpr-d",
		WorkflowID:     "wf-x",
		EvidenceRunIDs: []string{"r-1"},
		Confidence:     0.3,
		ArchitectModel: "m",
		// Status + CreatedAt deliberately omitted.
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO workflow_proposals")).
		WithArgs(p.ID, p.WorkflowID, "pending", "unspecified", "", "",
			pq.Array(p.EvidenceRunIDs), pq.Array(p.InstinctIDs), p.Confidence, p.ArchitectModel,
			sqlmock.AnyArg(), // CreatedAt defaulted to NOW
			sql.NullString{}).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Insert(context.Background(), p); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if p.Status != persistence.WorkflowProposalStatusPending {
		t.Errorf("Status default not applied: %q", p.Status)
	}
	if p.Kind != persistence.WorkflowProposalKindUnspecified {
		t.Errorf("Kind default not applied: %q", p.Kind)
	}
	if p.CreatedAt.IsZero() {
		t.Error("CreatedAt default not applied")
	}
}

// TestWorkflowProposalRepository_Insert_DefaultsAndValidation —
// missing ID surfaces a hard error before any SQL fires.
func TestWorkflowProposalRepository_Insert_Validation(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewWorkflowProposalRepository(db)

	if err := repo.Insert(context.Background(), nil); err == nil {
		t.Error("nil input should error")
	}
	if err := repo.Insert(context.Background(), &persistence.WorkflowProposal{WorkflowID: "x"}); err == nil {
		t.Error("missing ID should error")
	}
	if err := repo.Insert(context.Background(), &persistence.WorkflowProposal{ID: "x"}); err == nil {
		t.Error("missing WorkflowID should error")
	}
}

// TestWorkflowProposalRepository_Get_Found pins the SELECT column
// order + nullable-field unmarshalling. NULL decided_at / decided_by
// remain unset on the Go side.
func TestWorkflowProposalRepository_Get_Found(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewWorkflowProposalRepository(db)

	p := newWorkflowProposalFixture()
	rows := sqlmock.NewRows([]string{
		"id", "workflow_id", "status", "kind", "proposal_yaml", "motivation",
		"evidence_run_ids", "instinct_ids", "confidence", "architect_model", "created_at",
		"decided_at", "decided_by", "applied_at", "applied_commit",
		"rollback_commit", "notes",
	}).AddRow(
		p.ID, p.WorkflowID, "pending", "add_step", p.ProposalYAML, p.Motivation,
		pq.Array(p.EvidenceRunIDs), pq.Array(p.InstinctIDs), p.Confidence, p.ArchitectModel, p.CreatedAt,
		nil, nil, nil, nil, nil, nil,
	)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, workflow_id, status")).
		WithArgs("wpr-1").WillReturnRows(rows)

	got, err := repo.Get(context.Background(), "wpr-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != p.ID || got.WorkflowID != p.WorkflowID {
		t.Errorf("Get returned wrong row: %+v", got)
	}
	if got.Status != persistence.WorkflowProposalStatusPending {
		t.Errorf("status not threaded: %v", got.Status)
	}
	if got.Kind != persistence.WorkflowProposalKindAddStep {
		t.Errorf("kind not threaded: %v", got.Kind)
	}
	if len(got.EvidenceRunIDs) != 3 {
		t.Errorf("evidence_run_ids lost: %v", got.EvidenceRunIDs)
	}
	if got.DecidedAt != nil || got.DecidedBy != "" {
		t.Errorf("nullable fields should be empty: %+v %v", got.DecidedAt, got.DecidedBy)
	}
}

// TestWorkflowProposalRepository_Get_NotFound — sql.ErrNoRows maps
// to persistence.ErrNotFound so consumers get a stable sentinel.
func TestWorkflowProposalRepository_Get_NotFound(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewWorkflowProposalRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, workflow_id, status")).
		WithArgs("missing").WillReturnError(sql.ErrNoRows)

	_, err := repo.Get(context.Background(), "missing")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestWorkflowProposalRepository_List — newest-first + status ANY()
// filter + default 50 page size.
func TestWorkflowProposalRepository_List(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewWorkflowProposalRepository(db)

	cols := []string{
		"id", "workflow_id", "status", "kind", "proposal_yaml", "motivation",
		"evidence_run_ids", "instinct_ids", "confidence", "architect_model", "created_at",
		"decided_at", "decided_by", "applied_at", "applied_commit",
		"rollback_commit", "notes",
	}
	rows := sqlmock.NewRows(cols).
		AddRow("wpr-2", "wf-research", "pending", "unspecified", "", "", pq.Array([]string{}), nil, float32(0.5), "m", time.Now(),
			nil, nil, nil, nil, nil, nil)

	mock.ExpectQuery(regexp.QuoteMeta("FROM workflow_proposals")).
		WithArgs("wf-research", pq.Array([]string{"pending"}), 50).
		WillReturnRows(rows)

	got, err := repo.List(context.Background(), persistence.WorkflowProposalFilter{
		WorkflowID: "wf-research",
		Statuses:   []persistence.WorkflowProposalStatus{persistence.WorkflowProposalStatusPending},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d", len(got))
	}
}

// TestWorkflowProposalRepository_Decide_Approve — pending → approved
// transition stamps decided_at + decided_by, COALESCEs notes.
func TestWorkflowProposalRepository_Decide_Approve(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewWorkflowProposalRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE workflow_proposals")).
		WithArgs("approved", "vadim", "looks good", "wpr-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := repo.Decide(context.Background(), "wpr-1",
		persistence.WorkflowProposalStatusApproved, "vadim", "looks good")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
}

// TestWorkflowProposalRepository_Decide_RejectsTerminal — invalid
// status (e.g. asking to flip to 'applied' via Decide) is refused
// before any SQL fires.
func TestWorkflowProposalRepository_Decide_RejectsTerminal(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewWorkflowProposalRepository(db)
	err := repo.Decide(context.Background(), "wpr-1",
		persistence.WorkflowProposalStatusApplied, "vadim", "")
	if err == nil {
		t.Error("Decide should refuse a non-approved/rejected target")
	}
}

// TestWorkflowProposalRepository_Decide_AlreadyDecided — UPDATE
// matched 0 rows + row exists → ErrInvalidProposalTransition (not
// ErrNotFound). This is the signal the UI uses to say "someone else
// already approved this".
func TestWorkflowProposalRepository_Decide_AlreadyDecided(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewWorkflowProposalRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE workflow_proposals")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT status FROM workflow_proposals")).
		WithArgs("wpr-1").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("approved"))

	err := repo.Decide(context.Background(), "wpr-1",
		persistence.WorkflowProposalStatusApproved, "vadim", "")
	if !errors.Is(err, persistence.ErrInvalidProposalTransition) {
		t.Fatalf("want ErrInvalidProposalTransition, got %v", err)
	}
}

// TestWorkflowProposalRepository_Decide_NotFound — UPDATE matched 0
// rows + row missing → ErrNotFound, so the API can return 404.
func TestWorkflowProposalRepository_Decide_NotFound(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewWorkflowProposalRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE workflow_proposals")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT status FROM workflow_proposals")).
		WithArgs("wpr-1").WillReturnError(sql.ErrNoRows)

	err := repo.Decide(context.Background(), "wpr-1",
		persistence.WorkflowProposalStatusApproved, "vadim", "")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestWorkflowProposalRepository_MarkApplied_HappyPath — approved →
// applied transition stamps the commit + applied_at.
func TestWorkflowProposalRepository_MarkApplied_HappyPath(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewWorkflowProposalRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE workflow_proposals\n\t\tSET status = 'applied'")).
		WithArgs("abc1234", "wpr-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.MarkApplied(context.Background(), "wpr-1", "abc1234"); err != nil {
		t.Fatalf("MarkApplied: %v", err)
	}
}

// TestWorkflowProposalRepository_MarkApplied_RequiresCommit — empty
// commit hash is refused so the applied row always has a revert
// target.
func TestWorkflowProposalRepository_MarkApplied_RequiresCommit(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewWorkflowProposalRepository(db)
	if err := repo.MarkApplied(context.Background(), "wpr-1", ""); err == nil {
		t.Error("MarkApplied should require a commit hash")
	}
}

// TestWorkflowProposalRepository_MarkRolledBack_HappyPath — applied
// → rolled_back transition stamps the rollback commit.
func TestWorkflowProposalRepository_MarkRolledBack_HappyPath(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewWorkflowProposalRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE workflow_proposals\n\t\tSET status = 'rolled_back'")).
		WithArgs("def5678", "wpr-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.MarkRolledBack(context.Background(), "wpr-1", "def5678"); err != nil {
		t.Fatalf("MarkRolledBack: %v", err)
	}
}

// TestWorkflowProposalRepository_MarkRolledBack_WrongState — UPDATE
// matched 0 rows + row exists → ErrInvalidProposalTransition.
func TestWorkflowProposalRepository_MarkRolledBack_WrongState(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewWorkflowProposalRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE workflow_proposals\n\t\tSET status = 'rolled_back'")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT status FROM workflow_proposals")).
		WithArgs("wpr-1").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("pending"))

	err := repo.MarkRolledBack(context.Background(), "wpr-1", "def5678")
	if !errors.Is(err, persistence.ErrInvalidProposalTransition) {
		t.Fatalf("want ErrInvalidProposalTransition, got %v", err)
	}
}

// TestWorkflowProposalRepository_MarkApplied_NotFound — UPDATE
// matched 0 rows + row missing → ErrNotFound, so the API returns
// 404 rather than 409.
func TestWorkflowProposalRepository_MarkApplied_NotFound(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewWorkflowProposalRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE workflow_proposals\n\t\tSET status = 'applied'")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT status FROM workflow_proposals")).
		WithArgs("ghost").WillReturnError(sql.ErrNoRows)

	err := repo.MarkApplied(context.Background(), "ghost", "abc1234")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestWorkflowProposalRepository_MarkRolledBack_RequiresCommit —
// guard fires before any SQL so the rolled_back row always has a
// reverse-pointer to the revert commit.
func TestWorkflowProposalRepository_MarkRolledBack_RequiresCommit(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewWorkflowProposalRepository(db)
	if err := repo.MarkRolledBack(context.Background(), "wpr-1", ""); err == nil {
		t.Error("MarkRolledBack should require a commit hash")
	}
}

// TestWorkflowProposalRepository_Get_AllNullableFieldsSet — pins the
// scan path that materialises decided_at / decided_by / applied_at /
// applied_commit / rollback_commit / notes when none are NULL.
// Without this, the *time.Time and *string branches in
// scanWorkflowProposal stay uncovered.
func TestWorkflowProposalRepository_Get_AllNullableFieldsSet(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewWorkflowProposalRepository(db)

	created := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)
	decided := created.Add(1 * time.Hour)
	applied := created.Add(2 * time.Hour)

	rows := sqlmock.NewRows([]string{
		"id", "workflow_id", "status", "kind", "proposal_yaml", "motivation",
		"evidence_run_ids", "instinct_ids", "confidence", "architect_model", "created_at",
		"decided_at", "decided_by", "applied_at", "applied_commit",
		"rollback_commit", "notes",
	}).AddRow(
		"wpr-1", "wf-x", "rolled_back", "change_timeout", "yaml", "motiv",
		pq.Array([]string{"r-1"}), pq.Array([]string{"inst-1"}), float32(0.5), "model", created,
		decided, "operator-y", applied, "abc1234",
		"def5678", "rollback because regressed",
	)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, workflow_id, status")).
		WithArgs("wpr-1").WillReturnRows(rows)

	got, err := repo.Get(context.Background(), "wpr-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.DecidedAt == nil || !got.DecidedAt.Equal(decided) {
		t.Errorf("decided_at not threaded: %v", got.DecidedAt)
	}
	if got.AppliedAt == nil || !got.AppliedAt.Equal(applied) {
		t.Errorf("applied_at not threaded: %v", got.AppliedAt)
	}
	if got.DecidedBy != "operator-y" {
		t.Errorf("decided_by: %q", got.DecidedBy)
	}
	if got.AppliedCommit != "abc1234" {
		t.Errorf("applied_commit: %q", got.AppliedCommit)
	}
	if got.RollbackCommit != "def5678" {
		t.Errorf("rollback_commit: %q", got.RollbackCommit)
	}
	if got.Notes != "rollback because regressed" {
		t.Errorf("notes: %q", got.Notes)
	}
	if got.Status != persistence.WorkflowProposalStatusRolledBack {
		t.Errorf("status: %q", got.Status)
	}
}

// TestWorkflowProposalRepository_List_DefaultsAndNoFilter — empty
// filter must use default page size (50) and no WHERE filters
// beyond `1=1`. Pins the SQL shape so a future "tighten the
// filter" change has to update this test.
func TestWorkflowProposalRepository_List_DefaultsAndNoFilter(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewWorkflowProposalRepository(db)

	rows := sqlmock.NewRows([]string{
		"id", "workflow_id", "status", "kind", "proposal_yaml", "motivation",
		"evidence_run_ids", "instinct_ids", "confidence", "architect_model", "created_at",
		"decided_at", "decided_by", "applied_at", "applied_commit",
		"rollback_commit", "notes",
	})
	mock.ExpectQuery(regexp.QuoteMeta("FROM workflow_proposals")).
		WithArgs(50).
		WillReturnRows(rows)

	got, err := repo.List(context.Background(), persistence.WorkflowProposalFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 rows, got %d", len(got))
	}
}
