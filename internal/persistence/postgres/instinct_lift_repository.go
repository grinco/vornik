package postgres

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// InstinctLiftRepository persists the latest true-lift snapshot per
// instinct (migration 128) and runs the per-domain treatment/complement
// outcome queries over the audit spine.
//
// Snapshot upsert/get landed in Task 2; Recovery*/Budget* in Task 3;
// Architect* (this file's only Postgres-only domain — see LLD §5) in
// Task 4.
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
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (instinct_id) DO UPDATE SET
		    domain=EXCLUDED.domain, lift=EXCLUDED.lift,
		    treatment_n=EXCLUDED.treatment_n, treatment_succ=EXCLUDED.treatment_succ,
		    baseline_n=EXCLUDED.baseline_n, baseline_succ=EXCLUDED.baseline_succ,
		    std_error=EXCLUDED.std_error, verdict=EXCLUDED.verdict,
		    computed_at=EXCLUDED.computed_at
	`, s.InstinctID, s.Domain, s.Lift, s.TreatmentN, s.TreatmentSucc,
		s.BaselineN, s.BaselineSucc, s.StdError, s.Verdict, s.ComputedAt)
	return mapDBError(err)
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
		b.WriteString("$" + strconv.Itoa(i+1))
		args = append(args, id)
	}
	b.WriteString(")")

	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var s persistence.InstinctLiftSnapshot
		if err := rows.Scan(&s.InstinctID, &s.Domain, &s.Lift, &s.TreatmentN, &s.TreatmentSucc,
			&s.BaselineN, &s.BaselineSucc, &s.StdError, &s.Verdict, &s.ComputedAt); err != nil {
			return nil, mapDBError(err)
		}
		cp := s
		out[s.InstinctID] = &cp
	}
	return out, mapDBError(rows.Err())
}

// RecoveryAppliedOutcomes — resolved lead_recovery applications of this
// instinct since the window start; success = result 'succeeded'.
func (r *InstinctLiftRepository) RecoveryAppliedOutcomes(ctx context.Context, instinctID string, since time.Time) (persistence.LiftOutcomes, error) {
	var o persistence.LiftOutcomes
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN result = 'succeeded' THEN 1 ELSE 0 END), 0)
		FROM instinct_applications
		WHERE instinct_id = $1 AND surface = 'lead_recovery'
		  AND result IN ('succeeded','failed')
		  AND applied_at >= $2
	`, instinctID, since).Scan(&o.N, &o.Successes)
	return o, mapDBError(err)
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
		    WHERE ($2 = '' OR o.project_id = $2)
		      AND o.role = $3 AND o.error_class = $4
		      AND o.outcome <> 'ok' AND o.recorded_at >= $5
		    GROUP BY o.execution_id, o.step_id
		),
		eligible AS (
		    SELECT f.execution_id, f.step_id, f.failed_at
		    FROM failures f
		    WHERE NOT EXISTS (
		        SELECT 1 FROM instinct_applications a
		        WHERE a.instinct_id = $1
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
	`, instinctID, projectID, role, errorClass, since).Scan(&o.N, &o.Successes)
	return o, mapDBError(err)
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
		    WHERE a.instinct_id = $1 AND a.surface = 'tool_budget'
		      AND a.applied_at >= $2 AND a.task_id <> ''
		) x
		JOIN tasks t ON t.id = x.task_id
		WHERE t.status IN ('COMPLETED','FAILED','CANCELLED')
	`, instinctID, since).Scan(&o.N, &o.Successes)
	return o, mapDBError(err)
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
		    WHERE ($2 = '' OR o.project_id = $2)
		      AND o.role = $3 AND o.recorded_at >= $4 AND o.task_id <> ''
		) x
		JOIN tasks t ON t.id = x.task_id
		WHERE t.status IN ('COMPLETED','FAILED','CANCELLED')
		  AND NOT EXISTS (
		      SELECT 1 FROM instinct_applications a
		      WHERE a.instinct_id = $1 AND a.surface = 'tool_budget'
		        AND a.task_id = x.task_id
		  )
	`, instinctID, projectID, role, since).Scan(&o.N, &o.Successes)
	return o, mapDBError(err)
}

// ArchitectAppliedOutcomes — decided workflow_proposals whose
// instinct_ids contains this instinct; success = status <> 'rejected'.
// Pending proposals (decided_at IS NULL) are excluded — they haven't
// resolved to a treatment outcome yet.
func (r *InstinctLiftRepository) ArchitectAppliedOutcomes(ctx context.Context, instinctID string, since time.Time) (persistence.LiftOutcomes, error) {
	var o persistence.LiftOutcomes
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN p.status <> 'rejected' THEN 1 ELSE 0 END), 0)
		FROM workflow_proposals p
		WHERE COALESCE(p.instinct_ids, '{}') @> ARRAY[$1]::text[]
		  AND p.decided_at IS NOT NULL
		  AND p.created_at >= $2
	`, instinctID, since).Scan(&o.N, &o.Successes)
	return o, mapDBError(err)
}

// ArchitectComplementOutcomes — decided proposals for the SAME
// workflow_ids and kinds the instinct's treatment set touched in the
// window, whose instinct_ids does NOT contain this instinct.
// Workflow-scoped (workflow implies project) — a refinement of the
// LLD's earlier `(project, proposal-kind)` context-key sketch (LLD
// §5, review-20260719-4396 S4). Notably this query never references
// a project column at all: for a GLOBAL-scope instinct the treatment
// set can span workflows belonging to different projects, and the
// complement then spans those same workflows across those same
// projects by construction — that cross-project reach is the
// intended counterfactual for a global instinct, not a bug.
func (r *InstinctLiftRepository) ArchitectComplementOutcomes(ctx context.Context, instinctID string, since time.Time) (persistence.LiftOutcomes, error) {
	var o persistence.LiftOutcomes
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN p.status <> 'rejected' THEN 1 ELSE 0 END), 0)
		FROM workflow_proposals p
		WHERE p.decided_at IS NOT NULL
		  AND p.created_at >= $2
		  AND NOT (COALESCE(p.instinct_ids, '{}') @> ARRAY[$1]::text[])
		  AND p.workflow_id IN (
		      SELECT DISTINCT workflow_id FROM workflow_proposals
		      WHERE COALESCE(instinct_ids, '{}') @> ARRAY[$1]::text[]
		        AND created_at >= $2)
		  AND p.kind IN (
		      SELECT DISTINCT kind FROM workflow_proposals
		      WHERE COALESCE(instinct_ids, '{}') @> ARRAY[$1]::text[]
		        AND created_at >= $2)
	`, instinctID, since).Scan(&o.N, &o.Successes)
	return o, mapDBError(err)
}

// ensure interface compliance at compile time.
var _ persistence.InstinctLiftRepository = (*InstinctLiftRepository)(nil)
