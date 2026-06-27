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

// WorkflowHealingTriggerRepository implements
// persistence.WorkflowHealingTriggerRepository over postgres.
// The partial unique index on (project_id, workflow_id,
// trigger_class) WHERE status='open' is what makes Insert
// idempotent across detector ticks — a duplicate open trigger
// surfaces as a unique-violation that the repo maps to
// ErrAlreadyExists.
type WorkflowHealingTriggerRepository struct {
	db DBTX
}

// NewWorkflowHealingTriggerRepository constructs a repo over db.
func NewWorkflowHealingTriggerRepository(db DBTX) *WorkflowHealingTriggerRepository {
	return &WorkflowHealingTriggerRepository{db: db}
}

const healingTriggerColumns = `id, project_id, workflow_id, trigger_class,
    baseline_start, baseline_end, comparison_start, comparison_end,
    metric_name, baseline_value, comparison_value, threshold_value,
    evidence_execution_ids, status, created_at, resolved_at, proposal_id`

func (r *WorkflowHealingTriggerRepository) Insert(ctx context.Context, t *persistence.HealingTrigger) error {
	if t == nil {
		return fmt.Errorf("healing_triggers: nil trigger")
	}
	if t.ID == "" {
		t.ID = persistence.GenerateID("hb")
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	if t.Status == "" {
		t.Status = persistence.HealingTriggerStatusOpen
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO workflow_healing_triggers (
    id, project_id, workflow_id, trigger_class,
    baseline_start, baseline_end, comparison_start, comparison_end,
    metric_name, baseline_value, comparison_value, threshold_value,
    evidence_execution_ids, status, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
`,
		t.ID, t.ProjectID, t.WorkflowID, string(t.TriggerClass),
		t.BaselineStart.UTC(), t.BaselineEnd.UTC(),
		t.ComparisonStart.UTC(), t.ComparisonEnd.UTC(),
		t.MetricName, t.BaselineValue, t.ComparisonValue, t.ThresholdValue,
		pq.Array(t.EvidenceExecutionIDs), string(t.Status), t.CreatedAt.UTC(),
	)
	if err != nil {
		// Unique-violation against the partial index means
		// another tick already opened this regression — the
		// detector treats this as "already triaging".
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" {
			return persistence.ErrAlreadyExists
		}
		return fmt.Errorf("healing_triggers: insert: %w", err)
	}
	return nil
}

func (r *WorkflowHealingTriggerRepository) Get(ctx context.Context, id string) (*persistence.HealingTrigger, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+healingTriggerColumns+` FROM workflow_healing_triggers WHERE id = $1`, id)
	t, err := scanHealingTrigger(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, fmt.Errorf("healing_triggers: get: %w", err)
	}
	return t, nil
}

func (r *WorkflowHealingTriggerRepository) List(ctx context.Context, filter persistence.HealingTriggerListFilter) ([]*persistence.HealingTrigger, error) {
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
	if filter.Status != "" {
		add("status = $%d", string(filter.Status))
	}
	if filter.TriggerClass != "" {
		add("trigger_class = $%d", string(filter.TriggerClass))
	}
	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}
	args = append(args, filter.PageSize)
	q := `SELECT ` + healingTriggerColumns + ` FROM workflow_healing_triggers` + where +
		` ORDER BY created_at DESC LIMIT $` + fmt.Sprint(len(args))

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("healing_triggers: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.HealingTrigger
	for rows.Next() {
		t, err := scanHealingTrigger(rows)
		if err != nil {
			return nil, fmt.Errorf("healing_triggers: list scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *WorkflowHealingTriggerRepository) Dismiss(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `
UPDATE workflow_healing_triggers
SET status = 'dismissed', resolved_at = NOW()
WHERE id = $1 AND status = 'open'
`, id)
	if err != nil {
		return fmt.Errorf("healing_triggers: dismiss: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil
	}
	if n == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

func (r *WorkflowHealingTriggerRepository) MarkGenerated(ctx context.Context, id, proposalID string) error {
	res, err := r.db.ExecContext(ctx, `
UPDATE workflow_healing_triggers
SET status = 'generated_candidate', resolved_at = NOW(), proposal_id = $2
WHERE id = $1 AND status = 'open'
`, id, proposalID)
	if err != nil {
		return fmt.Errorf("healing_triggers: mark_generated: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil
	}
	if n == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

func scanHealingTrigger(scanner interface {
	Scan(dest ...interface{}) error
}) (*persistence.HealingTrigger, error) {
	var (
		t            persistence.HealingTrigger
		triggerClass string
		status       string
		evidence     pq.StringArray
		resolvedAt   sql.NullTime
		proposalID   sql.NullString
	)
	if err := scanner.Scan(
		&t.ID, &t.ProjectID, &t.WorkflowID, &triggerClass,
		&t.BaselineStart, &t.BaselineEnd, &t.ComparisonStart, &t.ComparisonEnd,
		&t.MetricName, &t.BaselineValue, &t.ComparisonValue, &t.ThresholdValue,
		&evidence, &status, &t.CreatedAt, &resolvedAt, &proposalID,
	); err != nil {
		return nil, err
	}
	t.TriggerClass = persistence.HealingTriggerClass(triggerClass)
	t.Status = persistence.HealingTriggerStatus(status)
	t.EvidenceExecutionIDs = []string(evidence)
	if resolvedAt.Valid {
		ts := resolvedAt.Time
		t.ResolvedAt = &ts
	}
	if proposalID.Valid {
		t.ProposalID = proposalID.String
	}
	return &t, nil
}
