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
// $1 = skill id, $2 = window start.
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
    SELECT DISTINCT execution_id FROM execution_injected_skills WHERE skill_id = $1
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
func ToQuestionMarks(query string, n int) string {
	for i := n; i >= 1; i-- {
		query = strings.ReplaceAll(query, fmt.Sprintf("$%d", i), "?")
	}
	return query
}
