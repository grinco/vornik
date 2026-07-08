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
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		p.ID, pgNullStr(p.ProjectID), p.Kind, p.BlastRadius, p.Title, p.Diff, p.Rationale,
		p.Evidence, p.Status, p.ProposedBy, p.Approver, p.PreApplySnapshot,
		p.CreatedAt, p.DecidedAt, p.AppliedAt,
	)
	return mapDBError(err)
}

func validatePGProposalFieldSizes(p *persistence.ControlPlaneProposal) error {
	for _, f := range []string{p.Diff, p.Rationale, p.Evidence, p.PreApplySnapshot} {
		if len(f) > persistence.ProposalMaxFieldBytes {
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
	if existing.Status != persistence.ProposalStatusDraft {
		return persistence.ErrProposalNotDraft
	}
	if status == persistence.ProposalStatusApproved && actor != "" && actor == existing.ProposedBy {
		return persistence.ErrProposalSelfApprove
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE control_plane_proposals SET status = $1, approver = $2, decided_at = $3
		WHERE id = $4 AND status = 'DRAFT'`,
		status, actor, time.Now().UTC(), id)
	if err != nil {
		return mapDBError(err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		return persistence.ErrProposalNotDraft
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
