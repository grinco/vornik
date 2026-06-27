package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// WorkflowHealingOverrideRepository implements
// persistence.HealingTriggerOverrideRepository over postgres. Backs
// the Black Box Phase B per-workflow threshold-override + mute
// surface (migration 81).
type WorkflowHealingOverrideRepository struct {
	db DBTX
}

// NewWorkflowHealingOverrideRepository constructs a repo over db.
func NewWorkflowHealingOverrideRepository(db DBTX) *WorkflowHealingOverrideRepository {
	return &WorkflowHealingOverrideRepository{db: db}
}

const healingOverrideColumns = `project_id, workflow_id, trigger_class,
    threshold_override, muted_until, notes,
    created_by, created_at, updated_at`

func (r *WorkflowHealingOverrideRepository) Upsert(ctx context.Context, o *persistence.HealingTriggerOverride) error {
	if o == nil {
		return fmt.Errorf("healing_overrides: nil override")
	}
	if o.ProjectID == "" || o.WorkflowID == "" || o.TriggerClass == "" {
		return fmt.Errorf("healing_overrides: project_id, workflow_id, trigger_class required")
	}
	var threshArg interface{}
	if o.ThresholdOverride != nil {
		threshArg = *o.ThresholdOverride
	}
	var muteArg interface{}
	if o.MutedUntil != nil {
		muteArg = o.MutedUntil.UTC()
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO workflow_healing_overrides (
    project_id, workflow_id, trigger_class,
    threshold_override, muted_until, notes,
    created_by, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, now(), now())
ON CONFLICT (project_id, workflow_id, trigger_class) DO UPDATE SET
    threshold_override = EXCLUDED.threshold_override,
    muted_until        = EXCLUDED.muted_until,
    notes              = EXCLUDED.notes,
    created_by         = EXCLUDED.created_by,
    updated_at         = now()
`,
		o.ProjectID, o.WorkflowID, string(o.TriggerClass),
		threshArg, muteArg, o.Notes, o.CreatedBy,
	)
	if err != nil {
		return fmt.Errorf("healing_overrides: upsert: %w", err)
	}
	return nil
}

func (r *WorkflowHealingOverrideRepository) Get(ctx context.Context, projectID, workflowID string, class persistence.HealingTriggerClass) (*persistence.HealingTriggerOverride, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+healingOverrideColumns+` FROM workflow_healing_overrides
         WHERE project_id = $1 AND workflow_id = $2 AND trigger_class = $3`,
		projectID, workflowID, string(class),
	)
	o, err := scanHealingOverride(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, fmt.Errorf("healing_overrides: get: %w", err)
	}
	return o, nil
}

func (r *WorkflowHealingOverrideRepository) List(ctx context.Context, pageSize int) ([]*persistence.HealingTriggerOverride, error) {
	if pageSize <= 0 {
		pageSize = 200
	}
	if pageSize > 1000 {
		pageSize = 1000
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+healingOverrideColumns+` FROM workflow_healing_overrides
         ORDER BY updated_at DESC LIMIT $1`,
		pageSize,
	)
	if err != nil {
		return nil, fmt.Errorf("healing_overrides: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.HealingTriggerOverride
	for rows.Next() {
		o, err := scanHealingOverride(rows)
		if err != nil {
			return nil, fmt.Errorf("healing_overrides: list scan: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (r *WorkflowHealingOverrideRepository) Delete(ctx context.Context, projectID, workflowID string, class persistence.HealingTriggerClass) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM workflow_healing_overrides
         WHERE project_id = $1 AND workflow_id = $2 AND trigger_class = $3`,
		projectID, workflowID, string(class),
	)
	if err != nil {
		return fmt.Errorf("healing_overrides: delete: %w", err)
	}
	return nil
}

func scanHealingOverride(scanner interface {
	Scan(dest ...interface{}) error
}) (*persistence.HealingTriggerOverride, error) {
	var (
		o            persistence.HealingTriggerOverride
		triggerClass string
		threshold    sql.NullFloat64
		mutedUntil   sql.NullTime
	)
	var updatedAt time.Time
	if err := scanner.Scan(
		&o.ProjectID, &o.WorkflowID, &triggerClass,
		&threshold, &mutedUntil, &o.Notes,
		&o.CreatedBy, &o.CreatedAt, &updatedAt,
	); err != nil {
		return nil, err
	}
	o.TriggerClass = persistence.HealingTriggerClass(triggerClass)
	o.UpdatedAt = updatedAt
	if threshold.Valid {
		v := threshold.Float64
		o.ThresholdOverride = &v
	}
	if mutedUntil.Valid {
		t := mutedUntil.Time
		o.MutedUntil = &t
	}
	return &o, nil
}
