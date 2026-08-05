package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ProposalRepository is the PostgreSQL persistence.ProposalRepository — the
// control-plane proposal ledger (LLD 2026-07-07-control-plane-design,
// migration 117). project_id "" is persisted as NULL (daemon-scope).
type ProposalRepository struct {
	db DBTX
}

// NewProposalRepository constructs a ProposalRepository over db.
func NewProposalRepository(db DBTX) *ProposalRepository { return &ProposalRepository{db: db} }

const pgProposalColumns = `id, project_id, kind, blast_radius, title, diff, rationale,
	evidence, status, proposed_by, approver, pre_apply_snapshot,
	apply_target, apply_content, apply_ops, applied_by, live_apply,
	created_at, decided_at, applied_at`

// Create inserts a new proposal, rejecting an oversized text field.
func (r *ProposalRepository) Create(ctx context.Context, p *persistence.ControlPlaneProposal) error {
	if err := validatePGProposalFieldSizes(p); err != nil {
		return err
	}
	if p.Status == "" {
		p.Status = persistence.ProposalStatusDraft
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO control_plane_proposals (`+pgProposalColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
		p.ID, pgNullStr(p.ProjectID), p.Kind, p.BlastRadius, p.Title, p.Diff, p.Rationale,
		p.Evidence, p.Status, p.ProposedBy, p.Approver, p.PreApplySnapshot,
		p.ApplyTarget, p.ApplyContent, p.ApplyOps, p.AppliedBy, p.LiveApply,
		p.CreatedAt, p.DecidedAt, p.AppliedAt,
	)
	return mapDBError(err)
}

func validatePGProposalFieldSizes(p *persistence.ControlPlaneProposal) error {
	// Free-text prose rendered into the review pane — tight bound.
	for _, f := range []string{p.Diff, p.Rationale, p.Evidence} {
		if len(f) > persistence.ProposalMaxFieldBytes {
			return persistence.ErrProposalFieldTooLarge
		}
	}
	// Whole-file payloads. ApplyContent is validated HERE for the first time:
	// leaving it out is what let the hub create a proposal that Apply would
	// then refuse with ErrContentTooLarge before any write (2026-08-05).
	for _, f := range []string{p.ApplyContent, p.PreApplySnapshot, p.ApplyOps} {
		if len(f) > persistence.ProposalMaxContentBytes {
			return persistence.ErrProposalFieldTooLarge
		}
	}
	return nil
}

// GetByID fetches a proposal by id, returning ErrNotFound if absent.
func (r *ProposalRepository) GetByID(ctx context.Context, id string) (*persistence.ControlPlaneProposal, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+pgProposalColumns+` FROM control_plane_proposals WHERE id = $1`, id)
	p, err := scanPGProposal(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	return p, err
}

// List returns proposals matching the filter, newest-created first.
func (r *ProposalRepository) List(ctx context.Context, f persistence.ProposalListFilter) ([]*persistence.ControlPlaneProposal, error) {
	var b strings.Builder
	b.WriteString(`SELECT ` + pgProposalColumns + ` FROM control_plane_proposals WHERE 1=1`)
	args := []interface{}{}
	pos := 1
	next := func(v interface{}) string {
		args = append(args, v)
		p := fmt.Sprintf("$%d", pos)
		pos++
		return p
	}
	if f.ProjectID != "" {
		b.WriteString(` AND project_id = ` + next(f.ProjectID))
	}
	if len(f.Statuses) > 0 {
		parts := make([]string, 0, len(f.Statuses))
		for _, s := range f.Statuses {
			parts = append(parts, next(s))
		}
		b.WriteString(` AND status IN (` + strings.Join(parts, ",") + `)`)
	}
	b.WriteString(` ORDER BY created_at DESC`)
	if f.Limit > 0 {
		b.WriteString(` LIMIT ` + next(f.Limit))
	}
	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.ControlPlaneProposal
	for rows.Next() {
		p, serr := scanPGProposal(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetStatus transitions a DRAFT proposal to APPROVED/REJECTED, recording the
// actor as approver and stamping decided_at. Rejects self-approval and any
// transition from a non-DRAFT state.
func (r *ProposalRepository) SetStatus(ctx context.Context, id, status, actor string) error {
	existing, err := r.GetByID(ctx, id)
	if err != nil {
		return err
	}
	// Approve is DRAFT-only. Reject/withdraw is allowed from DRAFT (reject a
	// pending proposal) OR APPROVED (withdraw an approved-but-unappliable
	// proposal — e.g. a daemon-scope change superseded by a re-draft).
	whereStates := "status = 'DRAFT'"
	switch status {
	case persistence.ProposalStatusApproved:
		if existing.Status != persistence.ProposalStatusDraft {
			return persistence.ErrProposalNotDraft
		}
		if actor != "" && actor == existing.ProposedBy {
			return persistence.ErrProposalSelfApprove
		}
	case persistence.ProposalStatusRejected:
		if existing.Status != persistence.ProposalStatusDraft && existing.Status != persistence.ProposalStatusApproved {
			return persistence.ErrProposalNotPending
		}
		whereStates = "status IN ('DRAFT','APPROVED')"
	default:
		return persistence.ErrProposalNotDraft
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE control_plane_proposals SET status = $1, approver = $2, decided_at = $3
		WHERE id = $4 AND `+whereStates,
		status, actor, time.Now().UTC(), id)
	if err != nil {
		return mapDBError(err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		if status == persistence.ProposalStatusRejected {
			return persistence.ErrProposalNotPending
		}
		return persistence.ErrProposalNotDraft
	}
	return nil
}

// MarkApplied transitions APPROVED → APPLIED, storing snapshot + applied_by.
func (r *ProposalRepository) MarkApplied(ctx context.Context, id, appliedBy, snapshot string) error {
	existing, err := r.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if existing.Status != persistence.ProposalStatusApproved {
		return persistence.ErrProposalNotApproved
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE control_plane_proposals
		SET status = $1, applied_by = $2, pre_apply_snapshot = $3, applied_at = $4
		WHERE id = $5 AND status = 'APPROVED'`,
		persistence.ProposalStatusApplied, appliedBy, snapshot, time.Now().UTC(), id)
	if err != nil {
		return mapDBError(err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		return persistence.ErrProposalNotApproved
	}
	return nil
}

// StagePreApplySnapshot writes pre_apply_snapshot on an APPROVED proposal
// without changing status (cost-auto-apply crash-safety, design D8).
func (r *ProposalRepository) StagePreApplySnapshot(ctx context.Context, id, snapshot string) error {
	existing, err := r.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if existing.Status != persistence.ProposalStatusApproved {
		return persistence.ErrProposalNotApproved
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE control_plane_proposals
		SET pre_apply_snapshot = $1
		WHERE id = $2 AND status = 'APPROVED'`, snapshot, id)
	if err != nil {
		return mapDBError(err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		return persistence.ErrProposalNotApproved
	}
	return nil
}

// MarkRolledBack transitions APPLIED → ROLLED_BACK.
func (r *ProposalRepository) MarkRolledBack(ctx context.Context, id string) error {
	existing, err := r.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if existing.Status != persistence.ProposalStatusApplied {
		return persistence.ErrProposalNotApplied
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE control_plane_proposals SET status = $1 WHERE id = $2 AND status = 'APPLIED'`,
		persistence.ProposalStatusRolledBack, id)
	if err != nil {
		return mapDBError(err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		return persistence.ErrProposalNotApplied
	}
	return nil
}

// MarkRegressed stamps the best-effort REGRESSED audit badge (design §4.4). It
// accepts BOTH APPLIED→REGRESSED and ROLLED_BACK→REGRESSED (the latter is the
// transition the canary guard's trip path executes). Any other source status is
// ErrProposalNotRegressable. The durable trip reason lives on the canary row;
// this badge only flips the proposal status (a lost badge never re-opens a trip).
func (r *ProposalRepository) MarkRegressed(ctx context.Context, id, reason string) error {
	_ = reason // durable reason is the canary row; badge flips status only (design §4.4)
	existing, err := r.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if existing.Status != persistence.ProposalStatusApplied && existing.Status != persistence.ProposalStatusRolledBack {
		return persistence.ErrProposalNotRegressable
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE control_plane_proposals SET status = $1
		WHERE id = $2 AND status IN ('APPLIED','ROLLED_BACK')`,
		persistence.ProposalStatusRegressed, id)
	if err != nil {
		return mapDBError(err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		return persistence.ErrProposalNotRegressable
	}
	return nil
}

func scanPGProposal(sc pgSkillScanner) (*persistence.ControlPlaneProposal, error) {
	var (
		p         persistence.ControlPlaneProposal
		projectID sql.NullString
		decidedAt sql.NullTime
		appliedAt sql.NullTime
	)
	if err := sc.Scan(
		&p.ID, &projectID, &p.Kind, &p.BlastRadius, &p.Title, &p.Diff, &p.Rationale,
		&p.Evidence, &p.Status, &p.ProposedBy, &p.Approver, &p.PreApplySnapshot,
		&p.ApplyTarget, &p.ApplyContent, &p.ApplyOps, &p.AppliedBy, &p.LiveApply,
		&p.CreatedAt, &decidedAt, &appliedAt,
	); err != nil {
		return nil, err
	}
	p.ProjectID = projectID.String
	if decidedAt.Valid {
		t := decidedAt.Time
		p.DecidedAt = &t
	}
	if appliedAt.Valid {
		t := appliedAt.Time
		p.AppliedAt = &t
	}
	return &p, nil
}
