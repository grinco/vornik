package executor

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// logGateTrace emits structured gate evaluation output so operators can
// see the exact values the gate compared without replaying the LLM
// response. Writes one log line per evaluation:
//
//   - level Info when the gate fired a target (gate_matched)
//   - level Warn when no condition matched (gate_failed)
//
// Per-entry fields carry {condition, observed, wanted, matched} so
// the "why did my reviewer gate route to failed_review" question has
// a one-grep answer instead of a 200-char single-string preview.
func (e *Executor) logGateTrace(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	stepID string,
	trace GateEvalTrace,
	err error,
	nextStepID string,
) {
	_ = ctx
	if len(trace.Entries) == 0 && err == nil {
		return
	}

	entries := make([]map[string]any, 0, len(trace.Entries))
	for _, entry := range trace.Entries {
		e := map[string]any{
			"condition": entry.Condition,
			"target":    entry.Target,
			"matched":   entry.Matched,
		}
		if entry.Found {
			e["observed"] = entry.Observed
			e["wanted"] = entry.Wanted
		} else if entry.Err == "" {
			e["observed_missing"] = true
		}
		if entry.Err != "" {
			e["error"] = entry.Err
		}
		entries = append(entries, e)
	}

	event := e.logger.Info()
	msg := "gate_matched"
	if err != nil {
		event = e.logger.Warn()
		msg = "gate_failed"
	}
	event.
		Str("task_id", task.ID).
		Str("execution_id", execution.ID).
		Str("step", stepID).
		Str("next_step", nextStepID).
		Int("gate_count", len(trace.Entries)).
		Interface("gates", entries).
		Str("raw_preview", fmt.Sprint(trace.RawPreview)).
		Msg(msg)
}
