package executor

import (
	"context"
	"strings"
	"time"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

// emitLive publishes a live event when the executor has a live
// publisher wired. Nil-safe so every tap call site stays terse
// at the boundary — no e.livePub != nil dance scattered through
// the workflow loop.
func (e *Executor) emitLive(ctx context.Context, executionID, kind string, payload any) {
	if e == nil || e.livePub == nil {
		return
	}
	if executionID == "" || kind == "" {
		return
	}
	e.livePub.Publish(ctx, executionID, kind, payload)
}

// emitStepStarted is the canonical wrapper for the step_started
// tap so the workflow loop can call it without remembering the
// payload shape.
func (e *Executor) emitStepStarted(ctx context.Context, executionID, stepID, role, model string, iteration int) {
	e.emitLive(ctx, executionID, livepubsub.KindStepStarted,
		livepubsub.StepStartedPayload{
			StepID:    stepID,
			Role:      role,
			Model:     model,
			Iteration: iteration,
		})
}

// emitStepCompleted publishes the step_completed event at step
// boundary close. Duration is computed by the caller (the loop
// holds the stepStart timestamp); cost comes from the per-step
// usage rows if any.
func (e *Executor) emitStepCompleted(ctx context.Context, executionID, stepID, outcome string, started time.Time, costUSD float64) {
	durationMs := int64(0)
	if !started.IsZero() {
		durationMs = time.Since(started).Milliseconds()
	}
	e.emitLive(ctx, executionID, livepubsub.KindStepCompleted,
		livepubsub.StepCompletedPayload{
			StepID:     stepID,
			Outcome:    outcome,
			DurationMs: durationMs,
			CostUSD:    costUSD,
		})
}

// emitPaused broadcasts that an execution paused awaiting the operator. The
// A2A SSE bridge maps KindPaused → the "input-required" task state, so a
// streaming A2A caller (which can't receive a chat/DM steering push — A2A
// isn't a conversation channel) sees the task is waiting on a human. The UI
// live view consumes the same event. pauseKind names why (e.g.
// "awaiting_input").
func (e *Executor) emitPaused(ctx context.Context, executionID, pauseKind string) {
	e.emitLive(ctx, executionID, livepubsub.KindPaused,
		livepubsub.PausedPayload{PauseKind: pauseKind})
}

// emitFileEdit broadcasts a workspace mutation observed via the
// produced_files collector. Op is one of "create" / "modify" /
// "delete" matching the executor's existing classification.
func (e *Executor) emitFileEdit(ctx context.Context, executionID, stepID, path, op, hash string, sizeBytes int64) {
	e.emitLive(ctx, executionID, livepubsub.KindFileEdit,
		livepubsub.FileEditPayload{
			StepID:    stepID,
			Path:      path,
			Op:        op,
			Hash:      hash,
			SizeBytes: sizeBytes,
		})
}

// consumeHintsForStep pulls any pending operator hints for the
// (taskID, executionID, stepID) target, marks them applied,
// publishes `hint_applied` for each, and returns the concatenated
// content as a prompt prefix. Nil-safe — when the repo isn't wired
// or no hints are pending, returns "".
//
// The returned prefix is wrapped in <operator-hint>...</operator-hint>
// tags so the LLM treats it as instruction-shaped operator
// context, not as part of the original prompt. Multiple hints
// concatenate top-to-bottom in insertion order.
//
// taskID is the new (2026-05-26) scope axis: hints posted to the
// task carry across retries (new execution_id). The repo's
// ConsumePending OR's the two predicates so this one call drains
// both task-level and execution-level pending hints for the step.
func (e *Executor) consumeHintsForStep(ctx context.Context, taskID, executionID, stepID string) string {
	if e == nil || e.hintRepo == nil || executionID == "" {
		return ""
	}
	hints, err := e.hintRepo.ConsumePending(ctx, taskID, executionID, stepID)
	if err != nil || len(hints) == 0 {
		return ""
	}
	var b strings.Builder
	for _, h := range hints {
		if h == nil || strings.TrimSpace(h.Content) == "" {
			continue
		}
		b.WriteString("<operator-hint")
		if h.CreatedBy != "" {
			b.WriteString(` from="`)
			b.WriteString(escapeAttr(h.CreatedBy))
			b.WriteString(`"`)
		}
		b.WriteString(">\n")
		b.WriteString(h.Content)
		b.WriteString("\n</operator-hint>\n\n")
		e.emitHintApplied(ctx, executionID, h)
	}
	return b.String()
}

// emitHintApplied publishes the hint_applied event so the live
// view's hint-history pane can flip the hint's "applied" badge
// without re-polling.
func (e *Executor) emitHintApplied(ctx context.Context, executionID string, h *persistence.ExecutionHint) {
	if h == nil {
		return
	}
	e.emitLive(ctx, executionID, livepubsub.KindHintApplied,
		livepubsub.HintAppliedPayload{
			HintID:    h.ID,
			StepID:    h.StepID,
			CreatedBy: h.CreatedBy,
		})
}

// escapeAttr is a minimal HTML/XML attribute escaper. Only
// double-quotes and ampersands matter for the `from="..."`
// emission — the operator-supplied CreatedBy comes from the API
// key id or X-Operator-Id header and is unlikely to contain
// markup, but we defend at the boundary anyway.
func escapeAttr(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return s
}
