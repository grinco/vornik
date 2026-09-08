// Package ratingrollupsql holds the one query text both backends run for the
// rating rollup.
//
// It is shared rather than written twice because the query encodes the GRAIN
// RULE — collapsing per-(execution, rater) ratings to a per-execution verdict,
// and excluding executions whose raters disagreed
// (2026-09-07-execution-ratings-rollup-design.md §4.1). Two copies of that
// logic is how the backends come to disagree about what "contested" means, and
// the divergence would be invisible in any round-trip test.
//
// Deliberately portable: SUM(CASE WHEN ...) rather than Postgres's
// COUNT(*) FILTER, so the text itself is identical on both. Only the
// placeholder style differs, which is what ToQuestionMarks rewrites.
package ratingrollupsql

import (
	"fmt"
	"strings"
)

// SkillArms returns one row per (project_id, workflow_id, treated) group.
//
// $1 = skill id, $2 = window start, $3 = body sha256 filter (” = every body).
//
// The sha filter is what makes the arms describe the body an operator is
// ACTUALLY approving (2026-09-08-execution-ratings-approval-paths-design.md
// §4). Note it is an equality test, so a row whose body_sha256 is NULL —
// written before migration 180, body unknowable — never satisfies it. That is
// deliberate: an unknowable body must not be counted as the body under review.
// Passing ” disables the filter entirely and gives the whole-history view.
//
// The three stages: TREATED is the executions carrying the skill; CTX is the
// (project, workflow) pairs it appeared in, which is what "comparable" means
// here — a digest and a code review are not comparable outputs; GRADED
// collapses each execution's ratings to one verdict.
//
// An execution counts as rated only when it has ratings AND they agree; as up
// when it has ratings and none is down; as contested when it has both.
// Eligible counts every execution in the context regardless, because coverage
// is rated-over-eligible and the unrated ones are the denominator.
const SkillArms = `
WITH treated AS (
    SELECT DISTINCT execution_id FROM execution_injected_skills
     WHERE skill_id = $1 AND ($3 = '' OR body_sha256 = $3)
),
ctx AS (
    SELECT DISTINCT e.project_id, e.workflow_id
      FROM executions e
      JOIN treated t ON t.execution_id = e.id
),
scoped AS (
    SELECT e.id,
           e.project_id,
           e.workflow_id,
           CASE WHEN t.execution_id IS NOT NULL THEN 1 ELSE 0 END AS treated
      FROM executions e
      JOIN ctx c ON c.project_id = e.project_id AND c.workflow_id = e.workflow_id
      LEFT JOIN treated t ON t.execution_id = e.id
     WHERE e.created_at >= $2
),
graded AS (
    SELECT s.id,
           s.project_id,
           s.workflow_id,
           s.treated,
           COUNT(r.execution_id) AS n_rat,
           SUM(CASE WHEN r.verdict = 'up'   THEN 1 ELSE 0 END) AS n_up,
           SUM(CASE WHEN r.verdict = 'down' THEN 1 ELSE 0 END) AS n_down
      FROM scoped s
      LEFT JOIN execution_ratings r ON r.execution_id = s.id
     GROUP BY s.id, s.project_id, s.workflow_id, s.treated
)
SELECT project_id,
       workflow_id,
       treated,
       COUNT(*) AS eligible_n,
       SUM(CASE WHEN n_rat > 0 AND NOT (n_up > 0 AND n_down > 0) THEN 1 ELSE 0 END) AS rated_n,
       SUM(CASE WHEN n_rat > 0 AND n_down = 0 THEN 1 ELSE 0 END) AS up_n,
       SUM(CASE WHEN n_up > 0 AND n_down > 0 THEN 1 ELSE 0 END) AS contested_n
  FROM graded
 GROUP BY project_id, workflow_id, treated`

// ToQuestionMarks rewrites $N placeholders to the ? style SQLite takes.
//
// Highest-numbered first, so $10 is not mangled by the rewrite of $1 — the
// query has two placeholders today, and that is exactly the kind of thing that
// stops being true quietly.
//
// Only valid when every placeholder appears EXACTLY ONCE, since positional ?
// binds by order of appearance. Use ToNumberedPlaceholders otherwise.
func ToQuestionMarks(query string, n int) string {
	for i := n; i >= 1; i-- {
		query = strings.ReplaceAll(query, fmt.Sprintf("$%d", i), "?")
	}
	return query
}

// ToNumberedPlaceholders rewrites $N to SQLite's ?N form, which binds by
// NUMBER rather than by position.
//
// SkillArms references its sha filter twice — "($3 = ” OR body_sha256 = $3)"
// — and the plain ? rewrite turns that into two positional parameters, so the
// caller's arguments shift by one and the query fails with "missing argument
// with index 4". Numbering the placeholders lets one argument serve both
// references, and keeps the argument list identical to the Postgres side.
func ToNumberedPlaceholders(query string, n int) string {
	for i := n; i >= 1; i-- {
		query = strings.ReplaceAll(query, fmt.Sprintf("$%d", i), fmt.Sprintf("?%d", i))
	}
	return query
}

// InstinctRecoveryArms returns one row per arm for a recovery-domain instinct.
//
// $1 = instinct id, $2 = project id (” = any, a global-scope instinct),
// $3 = role, $4 = error class, $5 = window start.
//
// The context keys are instinct lift's own — (project, role, error_class),
// exactly what RecoveryComplementOutcomes matches on — so the reported verdict
// and the measured verdict on the same retire proposal are computed over the
// same population and differ only in their success predicate: a human up-vote
// on the execution here, a later 'ok' step outcome there
// (2026-09-08-execution-ratings-approval-paths-design.md §3.1).
//
// ONE divergence from lift, deliberate. Lift's treatment arm
// (RecoveryAppliedOutcomes) counts every application of the instinct
// regardless of context, while its baseline is context-restricted. This query
// restricts BOTH arms to the context, because arms drawn from different
// populations are not arms — see the design's as-built note.
//
// Recovery only: instinct_applications.execution_id is populated at the
// lead_recovery surface and nowhere else, so no other domain has an execution
// for a rating to attach to.
const InstinctRecoveryArms = `
WITH ctx_failures AS (
    SELECT DISTINCT o.execution_id
      FROM execution_step_outcomes o
     WHERE ($2 = '' OR o.project_id = $2)
       AND o.role = $3 AND o.error_class = $4
       AND o.outcome <> 'ok' AND o.recorded_at >= $5
),
applied AS (
    SELECT DISTINCT a.execution_id
      FROM instinct_applications a
     WHERE a.instinct_id = $1 AND a.surface = 'lead_recovery'
       AND a.result IN ('succeeded','failed')
       AND a.applied_at >= $5
       AND a.execution_id <> ''
),
scoped AS (
    SELECT f.execution_id,
           CASE WHEN ap.execution_id IS NOT NULL THEN 1 ELSE 0 END AS treated
      FROM ctx_failures f
      LEFT JOIN applied ap ON ap.execution_id = f.execution_id
),
graded AS (
    SELECT s.execution_id,
           s.treated,
           COUNT(r.execution_id) AS n_rat,
           SUM(CASE WHEN r.verdict = 'up'   THEN 1 ELSE 0 END) AS n_up,
           SUM(CASE WHEN r.verdict = 'down' THEN 1 ELSE 0 END) AS n_down
      FROM scoped s
      LEFT JOIN execution_ratings r ON r.execution_id = s.execution_id
     GROUP BY s.execution_id, s.treated
)
SELECT treated,
       COUNT(*) AS eligible_n,
       SUM(CASE WHEN n_rat > 0 AND NOT (n_up > 0 AND n_down > 0) THEN 1 ELSE 0 END) AS rated_n,
       SUM(CASE WHEN n_rat > 0 AND n_down = 0 THEN 1 ELSE 0 END) AS up_n,
       SUM(CASE WHEN n_up > 0 AND n_down > 0 THEN 1 ELSE 0 END) AS contested_n
  FROM graded
 GROUP BY treated`
