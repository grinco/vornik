package sqlite

import (
	"context"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// InstinctLiftRepository is the SQLite mirror of the lift-measurement
// repository (migration 128). Behaviour parity with the Postgres side is
// proven by the shared repotest.RunInstinctLiftSuite.
//
// Snapshot upsert/get landed in Task 2; Recovery*/Budget* in Task 3.
// Architect* (Task 4) is a deliberate zero-stub: SQLite has no
// architect surface (see the two methods' doc comments below).
type InstinctLiftRepository struct {
	db DBTX
}

// NewInstinctLiftRepository creates a new repository.
func NewInstinctLiftRepository(db DBTX) *InstinctLiftRepository {
	return &InstinctLiftRepository{db: db}
}

// UpsertLiftSnapshot writes/replaces the latest snapshot (PK instinct_id).
func (r *InstinctLiftRepository) UpsertLiftSnapshot(ctx context.Context, s *persistence.InstinctLiftSnapshot) error {
	if s == nil {
		return fmt.Errorf("lift snapshot is nil")
	}
	if s.InstinctID == "" || s.Domain == "" || s.Verdict == "" {
		return fmt.Errorf("lift snapshot instinct_id, domain and verdict are required")
	}
	if s.ComputedAt.IsZero() {
		s.ComputedAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO instinct_lift (instinct_id, domain, lift, treatment_n, treatment_succ,
		    baseline_n, baseline_succ, std_error, verdict, computed_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (instinct_id) DO UPDATE SET
		    domain=excluded.domain, lift=excluded.lift,
		    treatment_n=excluded.treatment_n, treatment_succ=excluded.treatment_succ,
		    baseline_n=excluded.baseline_n, baseline_succ=excluded.baseline_succ,
		    std_error=excluded.std_error, verdict=excluded.verdict,
		    computed_at=excluded.computed_at
	`, s.InstinctID, s.Domain, s.Lift, s.TreatmentN, s.TreatmentSucc,
		s.BaselineN, s.BaselineSucc, s.StdError, s.Verdict, sqliteTime(s.ComputedAt))
	return err
}

// GetLiftSnapshots batch-fetches snapshots for the given instinct IDs.
// Missing IDs are simply absent from the map. Empty input → empty map, no SQL.
func (r *InstinctLiftRepository) GetLiftSnapshots(ctx context.Context, instinctIDs []string) (map[string]*persistence.InstinctLiftSnapshot, error) {
	out := map[string]*persistence.InstinctLiftSnapshot{}
	if len(instinctIDs) == 0 {
		return out, nil
	}
	var b strings.Builder
	b.WriteString(`
		SELECT instinct_id, domain, lift, treatment_n, treatment_succ,
		       baseline_n, baseline_succ, std_error, verdict, computed_at
		FROM instinct_lift
		WHERE instinct_id IN (`)
	args := make([]any, 0, len(instinctIDs))
	for i, id := range instinctIDs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
		args = append(args, id)
	}
	b.WriteString(")")

	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			s          persistence.InstinctLiftSnapshot
			computedAt sqlTime
		)
		if err := rows.Scan(&s.InstinctID, &s.Domain, &s.Lift, &s.TreatmentN, &s.TreatmentSucc,
			&s.BaselineN, &s.BaselineSucc, &s.StdError, &s.Verdict, &computedAt); err != nil {
			return nil, err
		}
		s.ComputedAt = computedAt.Time
		cp := s
		out[s.InstinctID] = &cp
	}
	return out, rows.Err()
}

// RecoveryAppliedOutcomes — resolved lead_recovery applications of this
// instinct since the window start; success = result 'succeeded'.
func (r *InstinctLiftRepository) RecoveryAppliedOutcomes(ctx context.Context, instinctID string, since time.Time) (persistence.LiftOutcomes, error) {
	var o persistence.LiftOutcomes
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN result = 'succeeded' THEN 1 ELSE 0 END), 0)
		FROM instinct_applications
		WHERE instinct_id = ? AND surface = 'lead_recovery'
		  AND result IN ('succeeded','failed')
		  AND applied_at >= ?
	`, instinctID, sqliteTime(since)).Scan(&o.N, &o.Successes)
	return o, err
}

// RecoveryComplementOutcomes — failed steps in the same (project, role,
// error_class) context with no application of THIS instinct on that
// (execution, step); success = a later 'ok' outcome on the same pair.
// projectID "" (global-scope instinct) drops the project constraint.
func (r *InstinctLiftRepository) RecoveryComplementOutcomes(ctx context.Context, instinctID, projectID, role, errorClass string, since time.Time) (persistence.LiftOutcomes, error) {
	var o persistence.LiftOutcomes
	err := r.db.QueryRowContext(ctx, `
		WITH failures AS (
		    SELECT o.execution_id, o.step_id, MIN(o.recorded_at) AS failed_at
		    FROM execution_step_outcomes o
		    WHERE (? = '' OR o.project_id = ?)
		      AND o.role = ? AND o.error_class = ?
		      AND o.outcome <> 'ok' AND o.recorded_at >= ?
		    GROUP BY o.execution_id, o.step_id
		),
		eligible AS (
		    SELECT f.execution_id, f.step_id, f.failed_at
		    FROM failures f
		    WHERE NOT EXISTS (
		        SELECT 1 FROM instinct_applications a
		        WHERE a.instinct_id = ?
		          AND a.execution_id = f.execution_id AND a.step_id = f.step_id
		    )
		)
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN EXISTS (
		           SELECT 1 FROM execution_step_outcomes r
		           WHERE r.execution_id = e.execution_id AND r.step_id = e.step_id
		             AND r.outcome = 'ok' AND r.recorded_at > e.failed_at
		       ) THEN 1 ELSE 0 END), 0)
		FROM eligible e
	`, projectID, projectID, role, errorClass, sqliteTime(since), instinctID).Scan(&o.N, &o.Successes)
	return o, err
}

// BudgetAppliedOutcomes — distinct terminal tasks with a tool_budget
// application of this instinct since the window start; success = task
// COMPLETED.
func (r *InstinctLiftRepository) BudgetAppliedOutcomes(ctx context.Context, instinctID string, since time.Time) (persistence.LiftOutcomes, error) {
	var o persistence.LiftOutcomes
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN t.status = 'COMPLETED' THEN 1 ELSE 0 END), 0)
		FROM (
		    SELECT DISTINCT a.task_id
		    FROM instinct_applications a
		    WHERE a.instinct_id = ? AND a.surface = 'tool_budget'
		      AND a.applied_at >= ? AND a.task_id <> ''
		) x
		JOIN tasks t ON t.id = x.task_id
		WHERE t.status IN ('COMPLETED','FAILED','CANCELLED')
	`, instinctID, sqliteTime(since)).Scan(&o.N, &o.Successes)
	return o, err
}

// BudgetComplementOutcomes — distinct terminal tasks seen in the same
// (project, role) step-outcome context with NO tool_budget application
// of this instinct; success = task COMPLETED.
func (r *InstinctLiftRepository) BudgetComplementOutcomes(ctx context.Context, instinctID, projectID, role string, since time.Time) (persistence.LiftOutcomes, error) {
	var o persistence.LiftOutcomes
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN t.status = 'COMPLETED' THEN 1 ELSE 0 END), 0)
		FROM (
		    SELECT DISTINCT o.task_id
		    FROM execution_step_outcomes o
		    WHERE (? = '' OR o.project_id = ?)
		      AND o.role = ? AND o.recorded_at >= ? AND o.task_id <> ''
		) x
		JOIN tasks t ON t.id = x.task_id
		WHERE t.status IN ('COMPLETED','FAILED','CANCELLED')
		  AND NOT EXISTS (
		      SELECT 1 FROM instinct_applications a
		      WHERE a.instinct_id = ? AND a.surface = 'tool_budget'
		        AND a.task_id = x.task_id
		  )
	`, projectID, projectID, role, sqliteTime(since), instinctID).Scan(&o.N, &o.Successes)
	return o, err
}

// ArchitectAppliedOutcomes always returns a zero LiftOutcomes, nil.
// SQLite has no architect surface to measure: memetic-workflows
// proposals are a Postgres-only feature (single-process SQLite
// deployments aren't the architect's audience, and the per-workflow
// pending-proposal rate limit relies on a partial unique index that
// doesn't exist on this backend either — see the sqlite
// WorkflowProposalRepository stub in workflow_proposal_repository.go,
// whose Insert deliberately returns persistence.ErrNotFound for the
// same reason). There is no workflow_proposals table here to query,
// so the honest answer is zero outcomes, not an error (LLD §5: "SQLite
// reports zero outcomes → unknown").
func (r *InstinctLiftRepository) ArchitectAppliedOutcomes(_ context.Context, _ string, _ time.Time) (persistence.LiftOutcomes, error) {
	return persistence.LiftOutcomes{}, nil
}

// ArchitectComplementOutcomes always returns a zero LiftOutcomes,
// nil — same rationale as ArchitectAppliedOutcomes above.
func (r *InstinctLiftRepository) ArchitectComplementOutcomes(_ context.Context, _ string, _ time.Time) (persistence.LiftOutcomes, error) {
	return persistence.LiftOutcomes{}, nil
}

// ensure interface compliance at compile time.
var _ persistence.InstinctLiftRepository = (*InstinctLiftRepository)(nil)
