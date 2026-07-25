package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/lib/pq"
	"vornik.io/vornik/internal/quality"
)

// QualityRepository reads the two-tier quality aggregates for the cost/quality
// tuning loop (https://docs.vornik.io
// design.md §A) off the existing audit spine. Read-only; no schema changes.
type QualityRepository struct {
	db DBTX
}

// NewQualityRepository constructs a QualityRepository.
func NewQualityRepository(db DBTX) *QualityRepository {
	return &QualityRepository{db: db}
}

// perStepUsage pre-aggregates task_llm_usage to one prompt-token sum per
// (execution_id, step_id) — task_llm_usage has multiple rows per step (verified:
// 4200+ such keys), so this collapse keeps the LEFT JOIN 1:1 on the usage side.
const perStepUsage = `
	SELECT execution_id, step_id, SUM(prompt_tokens) AS pt
	FROM task_llm_usage GROUP BY execution_id, step_id`

// A step can also have >1 execution_step_outcomes row (retries + the original's
// 'superseded' marker; verified ~3% of step keys). Left un-deduped, COUNT and
// the token SUM fan out per outcome row (review-20260721-78d1 #1/#2). We
// canonicalise to ONE row per (execution_id, step_id) — the latest by
// (recorded_at, id) DESC, excluding 'superseded' replaced rows — before aggregating.
//
// DECISION (review-20260721-0f0e): ~9/90d steps are abandoned-superseded (their
// latest row IS 'superseded' with no successor — operator retried from earlier
// and no replacement ran). We INTENTIONALLY drop these: such a step produced no
// real final outcome, so counting it as success or failure would bias the rate;
// dropping it from both numerator and denominator is least-biased. canonStepFilter
// relies on the `o.` alias bound by `FROM execution_step_outcomes o` below.
const canonStepFilter = `o.recorded_at >= $1 AND o.outcome <> 'superseded'`

// canonStepFilterBetween is the bounded twin of canonStepFilter (design §4.1):
// the ONLY difference is the upper recorded_at bound, so the *Between aggregate
// queries below reuse the identical canonicalisation/fold/score and cannot drift
// from the unbounded queries except in the window they cover. $1=from (inclusive),
// $2=to (exclusive).
const canonStepFilterBetween = `o.recorded_at >= $1 AND o.recorded_at < $2 AND o.outcome <> 'superseded'`

// RoleQualityAggregates returns per-(project, role) A1 aggregates since `since`.
// Passing = steps with outcome 'ok' (constrained exits like prompt_token_budget
// / budget_tripwire are intentionally NOT counted as passing). Empty roles are
// excluded. Fan-out-proof via the per-step canonicalisation above.
func (r *QualityRepository) RoleQualityAggregates(ctx context.Context, since time.Time) ([]quality.RoleAggregate, error) {
	q := `
		WITH step_canon AS (
		    SELECT DISTINCT ON (o.execution_id, o.step_id)
		           o.execution_id, o.step_id, o.project_id, o.role, o.outcome
		    FROM execution_step_outcomes o
		    WHERE ` + canonStepFilter + ` AND o.role <> ''
		    ORDER BY o.execution_id, o.step_id, o.recorded_at DESC, o.id DESC
		)
		SELECT c.project_id, c.role,
		       COUNT(*) AS total,
		       COUNT(*) FILTER (WHERE c.outcome = 'ok') AS passing,
		       COALESCE(SUM(u.pt) FILTER (WHERE c.outcome = 'ok'), 0) AS passing_prompt_tokens
		FROM step_canon c
		LEFT JOIN (` + perStepUsage + `) u
		  ON u.execution_id = c.execution_id AND u.step_id = c.step_id
		GROUP BY c.project_id, c.role`
	rows, err := r.db.QueryContext(ctx, q, since)
	if err != nil {
		return nil, fmt.Errorf("role quality aggregates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []quality.RoleAggregate
	for rows.Next() {
		var a quality.RoleAggregate
		if err := rows.Scan(&a.ProjectID, &a.Role, &a.Total, &a.Passing, &a.PromptTokens); err != nil {
			return nil, fmt.Errorf("scan role aggregate: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// TaskQualityAggregates returns per-(project, workflow) A2 aggregates since
// `since`. Passing = terminal COMPLETED tasks with no hard-failure step
// (schema_violation / failed / refused). Only terminal tasks are counted.
func (r *QualityRepository) TaskQualityAggregates(ctx context.Context, since time.Time) ([]quality.TaskAggregate, error) {
	q := `
		WITH step_canon AS (
		    SELECT DISTINCT ON (o.execution_id, o.step_id)
		           o.execution_id, o.step_id, o.task_id, o.outcome
		    FROM execution_step_outcomes o
		    WHERE ` + canonStepFilter + `
		    ORDER BY o.execution_id, o.step_id, o.recorded_at DESC, o.id DESC
		),
		task_roll AS (
			SELECT t.project_id, COALESCE(t.workflow_id, '') AS workflow_id, c.task_id, t.status,
			       bool_or(c.outcome IN ('schema_violation','failed','refused')) AS had_hard_fail,
			       COALESCE(SUM(u.pt), 0) AS task_prompt_tokens
			FROM step_canon c
			JOIN tasks t ON t.id = c.task_id
			LEFT JOIN (` + perStepUsage + `) u
			  ON u.execution_id = c.execution_id AND u.step_id = c.step_id
			WHERE t.status IN ('COMPLETED','FAILED','CANCELLED')
			GROUP BY t.project_id, t.workflow_id, c.task_id, t.status
		)
		SELECT project_id, workflow_id,
		       COUNT(*) AS total,
		       COUNT(*) FILTER (WHERE status = 'COMPLETED' AND NOT had_hard_fail) AS passing,
		       COALESCE(SUM(task_prompt_tokens) FILTER (WHERE status = 'COMPLETED' AND NOT had_hard_fail), 0) AS passing_prompt_tokens
		FROM task_roll
		GROUP BY project_id, workflow_id`
	rows, err := r.db.QueryContext(ctx, q, since)
	if err != nil {
		return nil, fmt.Errorf("task quality aggregates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []quality.TaskAggregate
	for rows.Next() {
		var a quality.TaskAggregate
		// workflow_id can be NULL for tasks created without a workflow; the SQL
		// COALESCEs it, and this NullString scan is defense-in-depth so a NULL
		// never crashes the whole refresh (live bug 2026-07-21).
		var wf sql.NullString
		if err := rows.Scan(&a.ProjectID, &wf, &a.Total, &a.Passing, &a.PromptTokens); err != nil {
			return nil, fmt.Errorf("scan task aggregate: %w", err)
		}
		a.WorkflowID = wf.String
		out = append(out, a)
	}
	return out, rows.Err()
}

// RoleQualityAggregatesBetween is the bounded twin of RoleQualityAggregates
// (design §4.1): same query, recorded_at bounded to [from, to). $1=from, $2=to.
func (r *QualityRepository) RoleQualityAggregatesBetween(ctx context.Context, from, to time.Time) ([]quality.RoleAggregate, error) {
	q := `
		WITH step_canon AS (
		    SELECT DISTINCT ON (o.execution_id, o.step_id)
		           o.execution_id, o.step_id, o.project_id, o.role, o.outcome
		    FROM execution_step_outcomes o
		    WHERE ` + canonStepFilterBetween + ` AND o.role <> ''
		    ORDER BY o.execution_id, o.step_id, o.recorded_at DESC, o.id DESC
		)
		SELECT c.project_id, c.role,
		       COUNT(*) AS total,
		       COUNT(*) FILTER (WHERE c.outcome = 'ok') AS passing,
		       COALESCE(SUM(u.pt) FILTER (WHERE c.outcome = 'ok'), 0) AS passing_prompt_tokens
		FROM step_canon c
		LEFT JOIN (` + perStepUsage + `) u
		  ON u.execution_id = c.execution_id AND u.step_id = c.step_id
		GROUP BY c.project_id, c.role`
	rows, err := r.db.QueryContext(ctx, q, from, to)
	if err != nil {
		return nil, fmt.Errorf("role quality aggregates between: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []quality.RoleAggregate
	for rows.Next() {
		var a quality.RoleAggregate
		if err := rows.Scan(&a.ProjectID, &a.Role, &a.Total, &a.Passing, &a.PromptTokens); err != nil {
			return nil, fmt.Errorf("scan role aggregate: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// TaskQualityAggregatesBetween is the bounded twin of TaskQualityAggregates
// (design §4.1): same query, recorded_at bounded to [from, to). $1=from, $2=to.
func (r *QualityRepository) TaskQualityAggregatesBetween(ctx context.Context, from, to time.Time) ([]quality.TaskAggregate, error) {
	q := `
		WITH step_canon AS (
		    SELECT DISTINCT ON (o.execution_id, o.step_id)
		           o.execution_id, o.step_id, o.task_id, o.outcome
		    FROM execution_step_outcomes o
		    WHERE ` + canonStepFilterBetween + `
		    ORDER BY o.execution_id, o.step_id, o.recorded_at DESC, o.id DESC
		),
		task_roll AS (
			SELECT t.project_id, COALESCE(t.workflow_id, '') AS workflow_id, c.task_id, t.status,
			       bool_or(c.outcome IN ('schema_violation','failed','refused')) AS had_hard_fail,
			       COALESCE(SUM(u.pt), 0) AS task_prompt_tokens
			FROM step_canon c
			JOIN tasks t ON t.id = c.task_id
			LEFT JOIN (` + perStepUsage + `) u
			  ON u.execution_id = c.execution_id AND u.step_id = c.step_id
			WHERE t.status IN ('COMPLETED','FAILED','CANCELLED')
			GROUP BY t.project_id, t.workflow_id, c.task_id, t.status
		)
		SELECT project_id, workflow_id,
		       COUNT(*) AS total,
		       COUNT(*) FILTER (WHERE status = 'COMPLETED' AND NOT had_hard_fail) AS passing,
		       COALESCE(SUM(task_prompt_tokens) FILTER (WHERE status = 'COMPLETED' AND NOT had_hard_fail), 0) AS passing_prompt_tokens
		FROM task_roll
		GROUP BY project_id, workflow_id`
	rows, err := r.db.QueryContext(ctx, q, from, to)
	if err != nil {
		return nil, fmt.Errorf("task quality aggregates between: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []quality.TaskAggregate
	for rows.Next() {
		var a quality.TaskAggregate
		var wf sql.NullString
		if err := rows.Scan(&a.ProjectID, &wf, &a.Total, &a.Passing, &a.PromptTokens); err != nil {
			return nil, fmt.Errorf("scan task aggregate: %w", err)
		}
		a.WorkflowID = wf.String
		out = append(out, a)
	}
	return out, rows.Err()
}

// RolePercentiles returns prompt-tokens-per-step p95/p99 per (swarm, role) since
// `since`, computed over the COMBINED distribution of the projects sharing each
// swarm. projectIDs[i]→swarmIDs[i] is the project→swarm map (from the registry);
// steps whose project isn't in the map are excluded (INNER JOIN). Percentiles
// can't be folded post-hoc, so the swarm grouping happens in-query. Uses the
// same per-step canonicalisation as the quality aggregates (fan-out-proof).
func (r *QualityRepository) RolePercentiles(ctx context.Context, since time.Time, projectIDs, swarmIDs []string) ([]quality.SwarmRolePercentile, error) {
	q := `
		WITH pmap AS (
		    SELECT unnest($2::text[]) AS project_id, unnest($3::text[]) AS swarm_id
		),
		step_canon AS (
		    SELECT DISTINCT ON (o.execution_id, o.step_id)
		           o.execution_id, o.step_id, o.project_id, o.role
		    FROM execution_step_outcomes o
		    WHERE ` + canonStepFilter + ` AND o.role <> ''
		    ORDER BY o.execution_id, o.step_id, o.recorded_at DESC, o.id DESC
		),
		step_tokens AS (
		    SELECT m.swarm_id, c.role, COALESCE(u.pt, 0) AS pt
		    FROM step_canon c
		    JOIN pmap m ON m.project_id = c.project_id
		    LEFT JOIN (` + perStepUsage + `) u
		      ON u.execution_id = c.execution_id AND u.step_id = c.step_id
		)
		SELECT swarm_id, role,
		       COUNT(*) AS n,
		       COALESCE(percentile_disc(0.95) WITHIN GROUP (ORDER BY pt), 0) AS p95,
		       COALESCE(percentile_disc(0.99) WITHIN GROUP (ORDER BY pt), 0) AS p99
		FROM step_tokens
		GROUP BY swarm_id, role`
	rows, err := r.db.QueryContext(ctx, q, since, pq.Array(projectIDs), pq.Array(swarmIDs))
	if err != nil {
		return nil, fmt.Errorf("role percentiles: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []quality.SwarmRolePercentile
	for rows.Next() {
		var p quality.SwarmRolePercentile
		if err := rows.Scan(&p.Swarm, &p.Role, &p.N, &p.P95, &p.P99); err != nil {
			return nil, fmt.Errorf("scan role percentile: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
