package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ProposalRepository is the SQLite persistence.ProposalRepository — the
// control-plane proposal ledger (LLD 2026-07-07-control-plane-design,
// Phase 1). project_id "" is persisted as NULL (daemon-scope).
type ProposalRepository struct {
	db DBTX
}

// NewProposalRepository constructs a ProposalRepository over db.
func NewProposalRepository(db DBTX) *ProposalRepository { return &ProposalRepository{db: db} }

const proposalColumns = `id, project_id, kind, blast_radius, title, diff, rationale,
	evidence, status, proposed_by, approver, pre_apply_snapshot,
	created_at, decided_at, applied_at`

// Create inserts a new proposal, rejecting an oversized text field.
func (r *ProposalRepository) Create(ctx context.Context, p *persistence.ControlPlaneProposal) error {
	if err := validateProposalFieldSizes(p); err != nil {
		return err
	}
	if p.Status == "" {
		p.Status = persistence.ProposalStatusDraft
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO control_plane_proposals (`+proposalColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, nullStr(p.ProjectID), p.Kind, p.BlastRadius, p.Title, p.Diff, p.Rationale,
		p.Evidence, p.Status, p.ProposedBy, p.Approver, p.PreApplySnapshot,
		sqliteTime(p.CreatedAt), sqliteTimePtr(p.DecidedAt), sqliteTimePtr(p.AppliedAt),
	)
	return err
}

// validateProposalFieldSizes rejects a text field over the 64 KiB cap.
func validateProposalFieldSizes(p *persistence.ControlPlaneProposal) error {
	for _, f := range []string{p.Diff, p.Rationale, p.Evidence, p.PreApplySnapshot} {
		if len(f) > persistence.ProposalMaxFieldBytes {
			return persistence.ErrProposalFieldTooLarge
		}
	}
	return nil
}

// GetByID fetches a proposal by id, returning ErrNotFound if absent.
func (r *ProposalRepository) GetByID(ctx context.Context, id string) (*persistence.ControlPlaneProposal, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+proposalColumns+` FROM control_plane_proposals WHERE id = ?`, id)
	p, err := scanProposal(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	return p, err
}

// List returns proposals matching the filter, newest-created first.
func (r *ProposalRepository) List(ctx context.Context, f persistence.ProposalListFilter) ([]*persistence.ControlPlaneProposal, error) {
	var b strings.Builder
	b.WriteString(`SELECT ` + proposalColumns + ` FROM control_plane_proposals WHERE 1=1`)
	args := []interface{}{}
	if f.ProjectID != "" {
		b.WriteString(` AND project_id = ?`)
		args = append(args, f.ProjectID)
	}
	if len(f.Statuses) > 0 {
		b.WriteString(` AND status IN (` + placeholders(len(f.Statuses)) + `)`)
		for _, s := range f.Statuses {
			args = append(args, s)
		}
	}
	b.WriteString(` ORDER BY created_at DESC`)
	if f.Limit > 0 {
		b.WriteString(` LIMIT ?`)
		args = append(args, f.Limit)
	}
	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.ControlPlaneProposal
	for rows.Next() {
		p, serr := scanProposal(rows)
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
	if existing.Status != persistence.ProposalStatusDraft {
		return persistence.ErrProposalNotDraft
	}
	if status == persistence.ProposalStatusApproved && actor != "" && actor == existing.ProposedBy {
		return persistence.ErrProposalSelfApprove
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE control_plane_proposals SET status = ?, approver = ?, decided_at = ?
		WHERE id = ? AND status = 'DRAFT'`,
		status, actor, sqliteTime(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	// 0 rows = a racing decision flipped it out of DRAFT between our read and
	// write; report it as not-draft rather than a silent no-op.
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		return persistence.ErrProposalNotDraft
	}
	return nil
}

func scanProposal(sc skillScanner) (*persistence.ControlPlaneProposal, error) {
	var (
		p         persistence.ControlPlaneProposal
		projectID sql.NullString
		decidedAt sql.NullString
		appliedAt sql.NullString
		createdAt sqlTime
	)
	if err := sc.Scan(
		&p.ID, &projectID, &p.Kind, &p.BlastRadius, &p.Title, &p.Diff, &p.Rationale,
		&p.Evidence, &p.Status, &p.ProposedBy, &p.Approver, &p.PreApplySnapshot,
		&createdAt, &decidedAt, &appliedAt,
	); err != nil {
		return nil, err
	}
	p.ProjectID = projectID.String
	p.CreatedAt = createdAt.Time
	if decidedAt.Valid && decidedAt.String != "" {
		if t, err := parseSqliteTime(decidedAt.String); err == nil {
			p.DecidedAt = &t
		}
	}
	if appliedAt.Valid && appliedAt.String != "" {
		if t, err := parseSqliteTime(appliedAt.String); err == nil {
			p.AppliedAt = &t
		}
	}
	return &p, nil
}
