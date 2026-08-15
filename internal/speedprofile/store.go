package speedprofile

import (
	"context"
	"database/sql"
	"fmt"
)

// SampleQuery is the join that makes this measurable without new
// instrumentation: durations and tool-call counts live on
// execution_step_outcomes, token counts on task_llm_usage, and the two share
// (execution_id, step_id).
//
// Filters that matter:
//   - duration_ms > 0 — a step with no recorded duration measures nothing.
//   - completion_tokens > 0 — a step that generated nothing cannot inform a
//     decode rate, and including them drags the fixed term.
//   - tool_calls_used IS NOT NULL — absent is not zero. A NULL means the step
//     never reported, and treating it as zero would attribute its tool time to
//     the model, which is the confusion this package exists to remove.
const SampleQuery = `
	SELECT u.completion_tokens, o.tool_calls_used, o.duration_ms
	  FROM execution_step_outcomes o
	  JOIN task_llm_usage u
	    ON u.execution_id = o.execution_id AND u.step_id = o.step_id
	 WHERE u.model = $1
	   AND o.duration_ms > 0
	   AND u.completion_tokens > 0
	   AND o.tool_calls_used IS NOT NULL
	   AND o.recorded_at > NOW() - $2::interval`

// LoadSamples reads one model's recent steps.
func LoadSamples(ctx context.Context, db *sql.DB, model, window string) ([]Sample, error) {
	rows, err := db.QueryContext(ctx, SampleQuery, model, window)
	if err != nil {
		return nil, fmt.Errorf("load speed samples for %q: %w", model, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Sample
	for rows.Next() {
		var s Sample
		if err := rows.Scan(&s.CompletionTokens, &s.ToolCalls, &s.DurationMS); err != nil {
			return nil, fmt.Errorf("scan speed sample: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Models lists the models with recent usage, so a profile run does not need to
// be told what the deployment is running.
func Models(ctx context.Context, db *sql.DB, window string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT model FROM task_llm_usage
		 WHERE model <> '' AND recorded_at > NOW() - $1::interval
		 ORDER BY model`, window)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("scan model: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
