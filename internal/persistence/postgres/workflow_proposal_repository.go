package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"

	"vornik.io/vornik/internal/persistence"
)

// WorkflowProposalRepository persists architect-emitted workflow
// proposals (Slice 2 of the memetic-workflows arc). State machine
// transitions live in the Decide / MarkApplied / MarkRolledBack
// methods so the DB CHECK surface stays small.
type WorkflowProposalRepository struct {
	db DBTX
}

// NewWorkflowProposalRepository constructs the repo.
func NewWorkflowProposalRepository(db DBTX) *WorkflowProposalRepository {
	return &WorkflowProposalRepository{db: db}
}

const defaultProposalPageSize = 50

// Insert persists a new pending proposal. The partial unique index
// `uq_workflow_proposals_pending` enforces one-pending-per-workflow
// at the DB layer; a 23505 surfaces as ErrProposalRateLimited so
// the admin endpoint can return 429 with the existing proposal ID
// (looked up by the caller).
func (r *WorkflowProposalRepository) Insert(ctx context.Context, p *persistence.WorkflowProposal) error {
	if p == nil {
		return fmt.Errorf("workflow_proposal: nil insert")
	}
	if p.ID == "" {
		return fmt.Errorf("workflow_proposal: ID is required")
	}
	if p.WorkflowID == "" {
		return fmt.Errorf("workflow_proposal: WorkflowID is required")
	}
	if p.Status == "" {
		p.Status = persistence.WorkflowProposalStatusPending
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	// Default unset / untagged proposals to the sentinel so the
	// NOT NULL column (migration 83) is always satisfied.
	if p.Kind == "" {
		p.Kind = persistence.WorkflowProposalKindUnspecified
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO workflow_proposals (
		    id, workflow_id, status, kind, proposal_yaml, motivation,
		    evidence_run_ids, instinct_ids, confidence, architect_model,
		    created_at, notes
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		p.ID, p.WorkflowID, string(p.Status), string(p.Kind), p.ProposalYAML, p.Motivation,
		pq.Array(p.EvidenceRunIDs), pq.Array(p.InstinctIDs), p.Confidence, p.ArchitectModel,
		p.CreatedAt, nullable(p.Notes),
	)
	if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" &&
		strings.Contains(pqErr.Constraint, "pending") {
		return persistence.ErrProposalRateLimited
	}
	return mapDBError(err)
}

// Get returns the row by primary key.
func (r *WorkflowProposalRepository) Get(ctx context.Context, id string) (*persistence.WorkflowProposal, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, workflow_id, status, kind, proposal_yaml, motivation,
		       evidence_run_ids, instinct_ids, confidence, architect_model, created_at,
		       decided_at, decided_by, applied_at, applied_commit,
		       rollback_commit, notes
		FROM workflow_proposals
		WHERE id = $1`, id)
	return scanWorkflowProposal(row.Scan)
}

// List returns proposals matching the filter, newest first.
func (r *WorkflowProposalRepository) List(ctx context.Context, filter persistence.WorkflowProposalFilter) ([]*persistence.WorkflowProposal, error) {
	pageSize := filter.PageSize
	if pageSize <= 0 {
		pageSize = defaultProposalPageSize
	}
	q := strings.Builder{}
	q.WriteString(`
		SELECT id, workflow_id, status, kind, proposal_yaml, motivation,
		       evidence_run_ids, instinct_ids, confidence, architect_model, created_at,
		       decided_at, decided_by, applied_at, applied_commit,
		       rollback_commit, notes
		FROM workflow_proposals
		WHERE 1=1`)
	args := []any{}
	if filter.WorkflowID != "" {
		args = append(args, filter.WorkflowID)
		fmt.Fprintf(&q, " AND workflow_id = $%d", len(args))
	}
	if len(filter.Statuses) > 0 {
		raw := make([]string, len(filter.Statuses))
		for i, s := range filter.Statuses {
			raw[i] = string(s)
		}
		args = append(args, pq.Array(raw))
		fmt.Fprintf(&q, " AND status = ANY($%d)", len(args))
	}
	if len(filter.Kinds) > 0 {
		raw := make([]string, len(filter.Kinds))
		for i, k := range filter.Kinds {
			raw[i] = string(k)
		}
		args = append(args, pq.Array(raw))
		fmt.Fprintf(&q, " AND kind = ANY($%d)", len(args))
	}
	args = append(args, pageSize)
	fmt.Fprintf(&q, " ORDER BY created_at DESC LIMIT $%d", len(args))

	rows, err := r.db.QueryContext(ctx, q.String(), args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.WorkflowProposal
	for rows.Next() {
		p, err := scanWorkflowProposal(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Decide transitions a proposal from pending → approved or pending →
// rejected. Stamps decided_at + decided_by; optional notes for the
// operator's rationale. Returns ErrInvalidProposalTransition if the
// row isn't pending.
func (r *WorkflowProposalRepository) Decide(ctx context.Context, id string, status persistence.WorkflowProposalStatus, decidedBy, notes string) error {
	if status != persistence.WorkflowProposalStatusApproved &&
		status != persistence.WorkflowProposalStatusRejected {
		return fmt.Errorf("workflow_proposal: Decide accepts approved|rejected, got %q", status)
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE workflow_proposals
		SET status = $1,
		    decided_at = NOW(),
		    decided_by = $2,
		    notes = COALESCE(NULLIF($3, ''), notes)
		WHERE id = $4 AND status = 'pending'`,
		string(status), decidedBy, notes, id,
	)
	if err != nil {
		return mapDBError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Disambiguate not-found vs already-decided so the API
		// returns a clearer status code.
		var current sql.NullString
		row := r.db.QueryRowContext(ctx, `SELECT status FROM workflow_proposals WHERE id = $1`, id)
		if err := row.Scan(&current); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return persistence.ErrNotFound
			}
			return mapDBError(err)
		}
		return persistence.ErrInvalidProposalTransition
	}
	return nil
}

// MarkApplied flips approved → applied + stamps the git commit.
func (r *WorkflowProposalRepository) MarkApplied(ctx context.Context, id, appliedCommit string) error {
	if appliedCommit == "" {
		return fmt.Errorf("workflow_proposal: MarkApplied requires a commit hash")
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE workflow_proposals
		SET status = 'applied', applied_at = NOW(), applied_commit = $1
		WHERE id = $2 AND status = 'approved'`,
		appliedCommit, id,
	)
	if err != nil {
		return mapDBError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return r.classifyMissedTransition(ctx, id)
	}
	return nil
}

// MarkRolledBack flips applied → rolled_back + stamps the revert
// commit.
func (r *WorkflowProposalRepository) MarkRolledBack(ctx context.Context, id, rollbackCommit string) error {
	if rollbackCommit == "" {
		return fmt.Errorf("workflow_proposal: MarkRolledBack requires a commit hash")
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE workflow_proposals
		SET status = 'rolled_back', rollback_commit = $1
		WHERE id = $2 AND status = 'applied'`,
		rollbackCommit, id,
	)
	if err != nil {
		return mapDBError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return r.classifyMissedTransition(ctx, id)
	}
	return nil
}

// UpdateProposalYAML replaces a PENDING proposal's proposal_yaml —
// the operator review UI's "Modify" button (§8.5). Refuses when the
// row isn't pending so a decided/applied proposal's recorded YAML
// stays immutable. editedBy is appended to notes for provenance.
func (r *WorkflowProposalRepository) UpdateProposalYAML(ctx context.Context, id, newYAML, editedBy string) error {
	if newYAML == "" {
		return fmt.Errorf("workflow_proposal: UpdateProposalYAML requires non-empty YAML")
	}
	note := "yaml modified by operator"
	if editedBy != "" {
		note = "yaml modified by " + editedBy
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE workflow_proposals
		SET proposal_yaml = $1,
		    notes = CASE WHEN notes IS NULL OR notes = '' THEN $2
		                 ELSE notes || '; ' || $2 END
		WHERE id = $3 AND status = 'pending'`,
		newYAML, note, id,
	)
	if err != nil {
		return mapDBError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return r.classifyMissedTransition(ctx, id)
	}
	return nil
}

// classifyMissedTransition disambiguates "row missing" from "row in
// wrong state" after a WHERE id=... AND status=... UPDATE matched
// zero rows. The caller already proved 0 rows affected; this only
// runs on the unhappy path.
func (r *WorkflowProposalRepository) classifyMissedTransition(ctx context.Context, id string) error {
	var current sql.NullString
	row := r.db.QueryRowContext(ctx, `SELECT status FROM workflow_proposals WHERE id = $1`, id)
	if err := row.Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return persistence.ErrNotFound
		}
		return mapDBError(err)
	}
	return persistence.ErrInvalidProposalTransition
}

// scanWorkflowProposal materialises one row from a Scan-compatible
// closure (sql.Row.Scan or sql.Rows.Scan share the signature). The
// nullable timestamp + string fields collapse to zero-value /
// empty-string on the Go side rather than carrying *string.
func scanWorkflowProposal(scan func(...any) error) (*persistence.WorkflowProposal, error) {
	var p persistence.WorkflowProposal
	var status string
	var kind sql.NullString
	var evidence, instincts pq.StringArray
	var decidedAt, appliedAt sql.NullTime
	var decidedBy, appliedCommit, rollbackCommit, notes sql.NullString
	if err := scan(
		&p.ID, &p.WorkflowID, &status, &kind, &p.ProposalYAML, &p.Motivation,
		&evidence, &instincts, &p.Confidence, &p.ArchitectModel, &p.CreatedAt,
		&decidedAt, &decidedBy, &appliedAt, &appliedCommit,
		&rollbackCommit, &notes,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, mapDBError(err)
	}
	p.Status = persistence.WorkflowProposalStatus(status)
	// kind is NOT NULL with a default in the schema, but stay
	// defensive: a NULL (or empty) collapses to the sentinel.
	if kind.Valid && kind.String != "" {
		p.Kind = persistence.WorkflowProposalKind(kind.String)
	} else {
		p.Kind = persistence.WorkflowProposalKindUnspecified
	}
	p.EvidenceRunIDs = []string(evidence)
	// NULL (pre-migration-92 rows / no priors) scans as an empty
	// array; keep the field nil-vs-empty distinction unimportant.
	p.InstinctIDs = []string(instincts)
	if decidedAt.Valid {
		t := decidedAt.Time
		p.DecidedAt = &t
	}
	if appliedAt.Valid {
		t := appliedAt.Time
		p.AppliedAt = &t
	}
	if decidedBy.Valid {
		p.DecidedBy = decidedBy.String
	}
	if appliedCommit.Valid {
		p.AppliedCommit = appliedCommit.String
	}
	if rollbackCommit.Valid {
		p.RollbackCommit = rollbackCommit.String
	}
	if notes.Valid {
		p.Notes = notes.String
	}
	return &p, nil
}
