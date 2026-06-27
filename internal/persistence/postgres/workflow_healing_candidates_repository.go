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

// WorkflowHealingCandidateRepository implements
// persistence.WorkflowHealingCandidateRepository over postgres. Backs
// the Self-Healing Workflow Genome v1 candidate ledger (migration
// 87). A candidate is a trial-tracking record that links to a
// memetic WorkflowProposal; this repo owns only the candidate row,
// not the proposal content.
type WorkflowHealingCandidateRepository struct {
	db DBTX
}

// NewWorkflowHealingCandidateRepository constructs a repo over db.
func NewWorkflowHealingCandidateRepository(db DBTX) *WorkflowHealingCandidateRepository {
	return &WorkflowHealingCandidateRepository{db: db}
}

var _ persistence.WorkflowHealingCandidateRepository = (*WorkflowHealingCandidateRepository)(nil)

const healingCandidateColumns = `id, trigger_id, project_id, workflow_id, proposal_id,
    baseline_genome_hash, candidate_genome_hash, candidate_class,
    proposal_diff, motivation, expected_effect, risk_level,
    status, created_at, promoted_at, promoted_by`

func (r *WorkflowHealingCandidateRepository) Insert(ctx context.Context, c *persistence.HealingCandidate) error {
	if c == nil {
		return fmt.Errorf("healing_candidates: nil candidate")
	}
	if c.TriggerID == "" || c.ProjectID == "" || c.WorkflowID == "" || c.ProposalID == "" {
		return fmt.Errorf("healing_candidates: trigger_id, project_id, workflow_id, proposal_id required")
	}
	if c.ID == "" {
		c.ID = persistence.GenerateID("whc")
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	if c.Status == "" {
		c.Status = persistence.HealingCandidateDraft
	}
	if c.RiskLevel == "" {
		c.RiskLevel = persistence.HealingRiskMedium
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO workflow_healing_candidates (
    id, trigger_id, project_id, workflow_id, proposal_id,
    baseline_genome_hash, candidate_genome_hash, candidate_class,
    proposal_diff, motivation, expected_effect, risk_level,
    status, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
`,
		c.ID, c.TriggerID, c.ProjectID, c.WorkflowID, c.ProposalID,
		c.BaselineGenomeHash, c.CandidateGenomeHash, string(c.CandidateClass),
		c.ProposalDiff, c.Motivation, c.ExpectedEffect, string(c.RiskLevel),
		string(c.Status), c.CreatedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("healing_candidates: insert: %w", err)
	}
	return nil
}

func (r *WorkflowHealingCandidateRepository) Get(ctx context.Context, id string) (*persistence.HealingCandidate, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+healingCandidateColumns+` FROM workflow_healing_candidates WHERE id = $1`, id)
	c, err := scanHealingCandidate(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, fmt.Errorf("healing_candidates: get: %w", err)
	}
	return c, nil
}

func (r *WorkflowHealingCandidateRepository) List(ctx context.Context, filter persistence.HealingCandidateListFilter) ([]*persistence.HealingCandidate, error) {
	if filter.PageSize <= 0 {
		filter.PageSize = 100
	}
	if filter.PageSize > 500 {
		filter.PageSize = 500
	}
	var (
		conditions []string
		args       []interface{}
	)
	add := func(cond string, val interface{}) {
		args = append(args, val)
		conditions = append(conditions, fmt.Sprintf(cond, len(args)))
	}
	if filter.ProjectID != "" {
		add("project_id = $%d", filter.ProjectID)
	}
	if filter.WorkflowID != "" {
		add("workflow_id = $%d", filter.WorkflowID)
	}
	if filter.TriggerID != "" {
		add("trigger_id = $%d", filter.TriggerID)
	}
	if filter.Status != "" {
		add("status = $%d", string(filter.Status))
	}
	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}
	args = append(args, filter.PageSize)
	q := `SELECT ` + healingCandidateColumns + ` FROM workflow_healing_candidates` + where +
		` ORDER BY created_at DESC LIMIT $` + fmt.Sprint(len(args))

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("healing_candidates: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.HealingCandidate
	for rows.Next() {
		c, err := scanHealingCandidate(rows)
		if err != nil {
			return nil, fmt.Errorf("healing_candidates: list scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *WorkflowHealingCandidateRepository) SetStatus(ctx context.Context, id string, status persistence.HealingCandidateStatus) error {
	if status.IsTerminal() {
		return fmt.Errorf("healing_candidates: set_status: %q is terminal; use Promote/Reject", status)
	}
	// Never overwrite a terminal status (promoted/rejected) — those
	// are settled operator decisions.
	res, err := r.db.ExecContext(ctx, `
UPDATE workflow_healing_candidates
SET status = $2
WHERE id = $1 AND status NOT IN ('promoted','rejected')
`, id, string(status))
	if err != nil {
		return fmt.Errorf("healing_candidates: set_status: %w", err)
	}
	return rowsAffectedNotFound(res)
}

// BeginTrial is the compare-and-set claim for opening a trial: it flips
// the candidate to trial_running only from a trial-eligible state, so two
// concurrent run-trial calls can't both proceed. The loser sees 0 rows
// affected (the winner already moved it out of the eligible set) and gets
// won=false.
func (r *WorkflowHealingCandidateRepository) BeginTrial(ctx context.Context, id string) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
UPDATE workflow_healing_candidates
SET status = 'trial_running'
WHERE id = $1 AND status NOT IN ('promoted','rejected','trial_running')
`, id)
	if err != nil {
		return false, fmt.Errorf("healing_candidates: begin_trial: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("healing_candidates: begin_trial rows: %w", err)
	}
	return n == 1, nil
}

func (r *WorkflowHealingCandidateRepository) Promote(ctx context.Context, id, promotedBy string) error {
	res, err := r.db.ExecContext(ctx, `
UPDATE workflow_healing_candidates
SET status = 'promoted', promoted_at = NOW(), promoted_by = $2
WHERE id = $1 AND status NOT IN ('promoted','rejected')
`, id, promotedBy)
	if err != nil {
		return fmt.Errorf("healing_candidates: promote: %w", err)
	}
	return rowsAffectedNotFound(res)
}

func (r *WorkflowHealingCandidateRepository) Reject(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `
UPDATE workflow_healing_candidates
SET status = 'rejected'
WHERE id = $1 AND status NOT IN ('promoted','rejected')
`, id)
	if err != nil {
		return fmt.Errorf("healing_candidates: reject: %w", err)
	}
	return rowsAffectedNotFound(res)
}

// rowsAffectedNotFound maps a zero-row UPDATE to ErrNotFound. A
// driver that can't report RowsAffected is treated as success (the
// UPDATE ran without error) — same lenient contract as the trigger
// repo's Dismiss/MarkGenerated.
func rowsAffectedNotFound(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return nil
	}
	if n == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

func scanHealingCandidate(scanner interface {
	Scan(dest ...interface{}) error
}) (*persistence.HealingCandidate, error) {
	var (
		c          persistence.HealingCandidate
		class      string
		risk       string
		status     string
		promotedAt sql.NullTime
	)
	if err := scanner.Scan(
		&c.ID, &c.TriggerID, &c.ProjectID, &c.WorkflowID, &c.ProposalID,
		&c.BaselineGenomeHash, &c.CandidateGenomeHash, &class,
		&c.ProposalDiff, &c.Motivation, &c.ExpectedEffect, &risk,
		&status, &c.CreatedAt, &promotedAt, &c.PromotedBy,
	); err != nil {
		return nil, err
	}
	c.CandidateClass = persistence.HealingCandidateClass(class)
	c.RiskLevel = persistence.HealingRiskLevel(risk)
	c.Status = persistence.HealingCandidateStatus(status)
	if promotedAt.Valid {
		ts := promotedAt.Time
		c.PromotedAt = &ts
	}
	return &c, nil
}
