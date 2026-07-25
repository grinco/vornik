package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// CostTuningCanaryRepository is the SQLite persistence.CostTuningCanaryRepository
// — the cost/quality canary + regression auto-rollback guard's row store (design
// 2026-07-24 §4.3). JSON columns (project_ids/workflow_ids/baseline) are TEXT; times
// are RFC3339Nano TEXT (sqliteTime). Parity with the Postgres repo is asserted by
// repotest.RunCostTuningCanarySuite on both backends.
type CostTuningCanaryRepository struct {
	db DBTX
}

// NewCostTuningCanaryRepository constructs a repo over db.
func NewCostTuningCanaryRepository(db DBTX) *CostTuningCanaryRepository {
	return &CostTuningCanaryRepository{db: db}
}

var _ persistence.CostTuningCanaryRepository = (*CostTuningCanaryRepository)(nil)

const canaryColumns = `proposal_id, swarm_id, role, knob,
    project_ids, workflow_ids, applied_at, baseline_start, window_until,
    baseline, status, reason, opened_at, closed_at`

// Open inserts a new canary row (status defaults to open when unset).
func (r *CostTuningCanaryRepository) Open(ctx context.Context, c *persistence.CostTuningCanary) error {
	if c == nil {
		return fmt.Errorf("cost_tuning_canaries: nil canary")
	}
	if c.ProposalID == "" {
		return fmt.Errorf("cost_tuning_canaries: proposal_id required")
	}
	if c.Status == "" {
		c.Status = persistence.CanaryStatusOpen
	}
	if c.OpenedAt.IsZero() {
		c.OpenedAt = time.Now().UTC()
	}
	baseline, err := json.Marshal(c.Baseline)
	if err != nil {
		return fmt.Errorf("cost_tuning_canaries: marshal baseline: %w", err)
	}
	_, err = r.db.ExecContext(ctx, `
        INSERT INTO cost_tuning_canaries (`+canaryColumns+`)
        VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.ProposalID, c.SwarmID, c.Role, c.Knob,
		sqliteStringArray(c.ProjectIDs), sqliteStringArray(c.WorkflowIDs),
		sqliteTime(c.AppliedAt), sqliteTime(c.BaselineStart), sqliteTime(c.WindowUntil),
		string(baseline), c.Status, nullStr(c.Reason), sqliteTime(c.OpenedAt), sqliteTimePtr(c.ClosedAt),
	)
	if err != nil {
		return fmt.Errorf("cost_tuning_canaries: insert: %w", err)
	}
	return nil
}

// GetByProposalID fetches a canary by its proposal id (ErrNotFound if absent).
func (r *CostTuningCanaryRepository) GetByProposalID(ctx context.Context, proposalID string) (*persistence.CostTuningCanary, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+canaryColumns+` FROM cost_tuning_canaries WHERE proposal_id = ?`, proposalID)
	c, err := scanCanary(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("cost_tuning_canaries: get: %w", err)
	}
	return c, nil
}

// ListOpen returns every open canary, oldest-opened first.
func (r *CostTuningCanaryRepository) ListOpen(ctx context.Context) ([]*persistence.CostTuningCanary, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+canaryColumns+`
        FROM cost_tuning_canaries WHERE status = 'open' ORDER BY opened_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("cost_tuning_canaries: list_open: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.CostTuningCanary
	for rows.Next() {
		c, serr := scanCanary(rows)
		if serr != nil {
			return nil, fmt.Errorf("cost_tuning_canaries: list_open scan: %w", serr)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Finalize transitions an open canary to a terminal status in one row write.
// Matches only a currently-open row (idempotent).
func (r *CostTuningCanaryRepository) Finalize(ctx context.Context, proposalID, status, reason string, closedAt time.Time) error {
	if status == persistence.CanaryStatusOpen {
		return fmt.Errorf("cost_tuning_canaries: finalize requires a terminal status, got %q", status)
	}
	_, err := r.db.ExecContext(ctx, `
        UPDATE cost_tuning_canaries
        SET status = ?, reason = ?, closed_at = ?
        WHERE proposal_id = ? AND status = 'open'`,
		status, nullStr(reason), sqliteTime(closedAt), proposalID)
	if err != nil {
		return fmt.Errorf("cost_tuning_canaries: finalize: %w", err)
	}
	return nil
}

// HasOpenForSwarmRole reports whether an open canary exists on (swarm, role).
func (r *CostTuningCanaryRepository) HasOpenForSwarmRole(ctx context.Context, swarmID, role string) (bool, error) {
	var one int
	err := r.db.QueryRowContext(ctx, `
        SELECT 1 FROM cost_tuning_canaries
        WHERE status = 'open' AND swarm_id = ? AND role = ? LIMIT 1`,
		swarmID, role).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cost_tuning_canaries: has_open: %w", err)
	}
	return true, nil
}

// HasActiveCooldown reports whether a regressed canary for (swarm, role, knob)
// closed after notBefore (design §7 cooldown skip).
func (r *CostTuningCanaryRepository) HasActiveCooldown(ctx context.Context, swarmID, role, knob string, notBefore time.Time) (bool, error) {
	var one int
	err := r.db.QueryRowContext(ctx, `
        SELECT 1 FROM cost_tuning_canaries
        WHERE status = 'regressed' AND swarm_id = ? AND role = ? AND knob = ?
          AND closed_at IS NOT NULL AND closed_at > ? LIMIT 1`,
		swarmID, role, knob, sqliteTime(notBefore)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cost_tuning_canaries: has_cooldown: %w", err)
	}
	return true, nil
}

// LatestWindowUntil returns the greatest window_until among canaries on
// (swarm, role) whose window_until <= before — the baseline clamp anchor.
// Times are TEXT (RFC3339Nano) so lexical MAX == chronological MAX for the
// fixed-width, UTC, zero-padded encoding sqliteTime emits.
func (r *CostTuningCanaryRepository) LatestWindowUntil(ctx context.Context, swarmID, role string, before time.Time) (time.Time, bool, error) {
	var s sql.NullString
	err := r.db.QueryRowContext(ctx, `
        SELECT MAX(window_until) FROM cost_tuning_canaries
        WHERE swarm_id = ? AND role = ? AND window_until <= ?`,
		swarmID, role, sqliteTime(before)).Scan(&s)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("cost_tuning_canaries: latest_window_until: %w", err)
	}
	if !s.Valid || s.String == "" {
		return time.Time{}, false, nil
	}
	t, perr := parseSqliteTime(s.String)
	if perr != nil {
		return time.Time{}, false, fmt.Errorf("cost_tuning_canaries: parse window_until: %w", perr)
	}
	return t, true, nil
}

// CountPassedForKnob counts terminal status='passed' canaries for the exact
// (swarm, role, knob) — the cost-auto-apply track-record trust signal (design D1).
func (r *CostTuningCanaryRepository) CountPassedForKnob(ctx context.Context, swarmID, role, knob string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `
        SELECT count(*) FROM cost_tuning_canaries
        WHERE status = 'passed' AND swarm_id = ? AND role = ? AND knob = ?`,
		swarmID, role, knob).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("cost_tuning_canaries: count_passed_for_knob: %w", err)
	}
	return n, nil
}

// LastApplyActorForKnob returns applied_by of the proposal behind the most-recent
// canary (by applied_at) on (swarm, role, knob) — the M=1 re-seed signal (design D1).
func (r *CostTuningCanaryRepository) LastApplyActorForKnob(ctx context.Context, swarmID, role, knob string) (string, bool, error) {
	var actor sql.NullString
	err := r.db.QueryRowContext(ctx, `
        SELECT p.applied_by
        FROM cost_tuning_canaries c
        JOIN control_plane_proposals p ON p.id = c.proposal_id
        WHERE c.swarm_id = ? AND c.role = ? AND c.knob = ?
        ORDER BY c.applied_at DESC
        LIMIT 1`,
		swarmID, role, knob).Scan(&actor)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("cost_tuning_canaries: last_apply_actor_for_knob: %w", err)
	}
	return actor.String, true, nil
}

func scanCanary(sc interface {
	Scan(dest ...interface{}) error
}) (*persistence.CostTuningCanary, error) {
	var (
		c           persistence.CostTuningCanary
		projectIDs  sqliteStringArray
		workflowIDs sqliteStringArray
		baseline    string
		reason      sql.NullString
		appliedAt   sqlTime
		baselineAt  sqlTime
		windowUntil sqlTime
		openedAt    sqlTime
		closedAt    sqlNullTime
	)
	if err := sc.Scan(
		&c.ProposalID, &c.SwarmID, &c.Role, &c.Knob,
		&projectIDs, &workflowIDs, &appliedAt, &baselineAt, &windowUntil,
		&baseline, &c.Status, &reason, &openedAt, &closedAt,
	); err != nil {
		return nil, err
	}
	c.ProjectIDs = []string(projectIDs)
	c.WorkflowIDs = []string(workflowIDs)
	if baseline != "" {
		if err := json.Unmarshal([]byte(baseline), &c.Baseline); err != nil {
			return nil, fmt.Errorf("unmarshal baseline: %w", err)
		}
	}
	c.AppliedAt = appliedAt.Time
	c.BaselineStart = baselineAt.Time
	c.WindowUntil = windowUntil.Time
	c.OpenedAt = openedAt.Time
	c.Reason = reason.String
	if closedAt.Valid {
		t := closedAt.Time
		c.ClosedAt = &t
	}
	return &c, nil
}
