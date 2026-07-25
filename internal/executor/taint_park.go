package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/taintlineage"
)

// TaintReviewState is the sentinel `state` value the forge taint gate sets in a
// system-step result to request an untrusted-review PARK before an autonomous
// forge write proceeds (taint-lineage-tracking §4.5). Distinct from
// SystemStepBlockedState (a push rejection) — this park is driven by the
// task-lineage taint rollup under enforce mode, and resumes by RE-RUNNING the
// task, with a source-set latch (D7) governing whether the re-run re-parks.
const TaintReviewState = "taint_review_awaiting_operator"

// TaintReviewSignal is the courier the forge handler returns (as a system-step
// result) when a tainted forge write must park. The executor's workflow loop
// detects it via AsTaintReview and drives parkForTaintReview. It carries the
// already-resolved checkpoint payload so the park does not re-query.
type TaintReviewSignal struct {
	State         string                `json:"state"` // must equal TaintReviewState
	SourceSetHash string                `json:"source_set_hash"`
	SourceCount   int                   `json:"source_count"` // FULL pre-cap distinct count (F-cap)
	ShownCount    int                   `json:"shown_count"`  // len(Sources) — surfaced truncation
	Sources       []taintlineage.Source `json:"sources,omitempty"`
	Mode          string                `json:"mode,omitempty"`
	// LineageUnavailable marks a fail-closed park: enforce mode is active but
	// the lineage repositories or the task identity were missing, so NO source
	// walk happened. The counts and hash are therefore zero/empty and must not
	// be presented to the operator as "0 external sources" — that reads as
	// "nothing untrusted was found", the opposite of what happened.
	LineageUnavailable bool `json:"lineage_unavailable,omitempty"`
}

// AsTaintReview reports whether a system-step result requests the untrusted-
// review park, returning the parsed signal when so.
func AsTaintReview(result json.RawMessage) (*TaintReviewSignal, bool) {
	if len(result) == 0 {
		return nil, false
	}
	var s TaintReviewSignal
	if err := json.Unmarshal(result, &s); err != nil || s.State != TaintReviewState {
		return nil, false
	}
	return &s, true
}

// TaintReviewer resolves the task-lineage taint rollup for a forge write and,
// under enforce mode, decides whether the write must park for operator review.
// It is a standalone struct (NOT the Executor) so it can be constructed BEFORE
// the executor and injected into the forge system handler without a wiring-order
// cycle. It shares the pure decision logic (Rollup/Decide/latch) with the api
// query_api gate (internal/api/taint_review.go) via internal/taintlineage.
type TaintReviewer struct {
	outcomeRepo persistence.ExecutionStepOutcomeRepository
	taskLister  persistence.TaskLister
	msgRepo     persistence.TaskMessageRepository
	// modeFn resolves the effective enforcement mode for a project (project
	// override → daemon default). Nil ⇒ feature off.
	modeFn func(projectID string) taintlineage.Mode
	// recordOutcome is an optional metric hook: mode, outcome ∈ {flagged,parked}.
	recordOutcome func(mode taintlineage.Mode, outcome string)
}

// NewTaintReviewer builds a reviewer. A nil modeFn leaves the feature inert;
// once mode resolution is wired, missing repositories park in enforce mode.
func NewTaintReviewer(
	outcomeRepo persistence.ExecutionStepOutcomeRepository,
	taskLister persistence.TaskLister,
	msgRepo persistence.TaskMessageRepository,
	modeFn func(projectID string) taintlineage.Mode,
	recordOutcome func(mode taintlineage.Mode, outcome string),
) *TaintReviewer {
	return &TaintReviewer{
		outcomeRepo:   outcomeRepo,
		taskLister:    taskLister,
		msgRepo:       msgRepo,
		modeFn:        modeFn,
		recordOutcome: recordOutcome,
	}
}

// ReviewForgeWrite resolves taint for an autonomous forge write and returns a
// non-nil TaintReviewSignal when the write must PARK (enforce mode only). It
// returns (nil, nil) when the write may proceed (off/advisory, or enforce with
// no tainted lineage / a matching latch). Errors are folded into a fail-closed
// park under enforce (D6) rather than returned — a resolution failure must not
// silently let a tainted write through.
//
// Implements forge.TaintGate.
func (tr *TaintReviewer) ReviewForgeWrite(ctx context.Context, projectID, taskID string) (*TaintReviewSignal, error) {
	if tr == nil || tr.modeFn == nil {
		return nil, nil
	}
	mode := tr.modeFn(projectID)
	if mode == taintlineage.ModeOff {
		return nil, nil
	}
	if tr.outcomeRepo == nil || tr.taskLister == nil || taskID == "" {
		if mode != taintlineage.ModeEnforce {
			return nil, nil
		}
		return &TaintReviewSignal{
			State:              TaintReviewState,
			Mode:               string(mode),
			LineageUnavailable: true,
		}, nil
	}

	dec, err := resolveTaintDecision(ctx, mode, taskID, tr.taskLister, tr.outcomeRepo, tr.msgRepo)
	if err != nil {
		// enforce fails closed on a resolution error (D6); advisory never parks.
		if mode != taintlineage.ModeEnforce {
			return nil, nil
		}
		dec = taintlineage.Decision{Mode: mode, Park: true, RequiresReview: true, WalkComplete: false}
	}

	if mode == taintlineage.ModeAdvisory {
		if tr.recordOutcome != nil {
			if dec.Tainted {
				tr.recordOutcome(mode, "flagged")
			} else {
				// Allowed + untainted → permitted, the §14 calibration denominator (M6).
				tr.recordOutcome(mode, "permitted")
			}
		}
		return nil, nil
	}
	// enforce
	if !dec.Park {
		// Allowed under enforce (untainted / matching latch) → permitted (M6).
		if tr.recordOutcome != nil {
			tr.recordOutcome(mode, "permitted")
		}
		return nil, nil
	}
	if tr.recordOutcome != nil {
		tr.recordOutcome(mode, "parked")
	}
	return &TaintReviewSignal{
		State:         TaintReviewState,
		SourceSetHash: dec.SourceSetHash,
		SourceCount:   dec.SourceCount,
		ShownCount:    dec.ShownCount,
		Sources:       dec.Sources,
		Mode:          string(mode),
	}, nil
}

// resolveTaintDecision is the shared orchestration (walk → tainted-steps query →
// rollup → latch read → Decide) used by BOTH the forge gate here and the api
// query_api gate. The pure decision (D8 formula + latch match) lives in
// taintlineage.Decide; this glue is the only I/O. Returns an error only on a
// repo failure — the caller decides fail-open (advisory) vs fail-closed
// (enforce, D6).
func resolveTaintDecision(
	ctx context.Context,
	mode taintlineage.Mode,
	taskID string,
	taskLister persistence.TaskLister,
	outcomeRepo persistence.ExecutionStepOutcomeRepository,
	msgRepo persistence.TaskMessageRepository,
) (taintlineage.Decision, error) {
	// 1. Walk the request-root lineage → full task-ID set + completeness.
	lineageIDs, outcome, err := persistence.ResolveLineageWithCompleteness(
		ctx, taskLister, taskID, persistence.MaxRequestRootWalkDepth)
	if err != nil {
		return taintlineage.Decision{}, err
	}
	walkComplete := outcome == persistence.WalkOutcomeCleanRoot
	if len(lineageIDs) == 0 {
		lineageIDs = []string{taskID}
	}

	// 2. One batched tainted-steps query over the whole lineage (I7).
	rows, err := outcomeRepo.TaintedStepsForTasks(ctx, lineageIDs)
	if err != nil {
		return taintlineage.Decision{}, err
	}

	// 3. Fold rows into own vs ancestor step taints.
	var own, ancestor []taintlineage.StepTaint
	for _, r := range rows {
		st := taintlineage.StepTaintFromBlob(r.UntrustedSources, r.RequiresReview)
		if r.TaskID == taskID {
			own = append(own, st)
		} else {
			ancestor = append(ancestor, st)
		}
	}
	roll := taintlineage.Rollup(own, ancestor, walkComplete)

	// 4. Read recorded latches for THIS task and decide.
	latches := readLatchHashes(ctx, msgRepo, taskID)
	return taintlineage.Decide(mode, roll, latches), nil
}

// readLatchHashes lists the task's messages and collects every recorded
// taint_latch source-set hash (D7). Best-effort: a repo error yields no
// latches, so the gate stays fail-closed (an unreadable latch never suppresses
// a park). Nil msgRepo ⇒ no latches.
func readLatchHashes(ctx context.Context, msgRepo persistence.TaskMessageRepository, taskID string) []string {
	if msgRepo == nil || taskID == "" {
		return nil
	}
	msgs, err := msgRepo.List(ctx, persistence.TaskMessageFilter{
		TaskID:       taskID,
		MessageKinds: []string{persistence.TaskMessageKindSystem},
	})
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range msgs {
		if h, ok := taintlineage.ParseLatchHash(m.Metadata); ok {
			out = append(out, h)
		}
	}
	return out
}

// parkForTaintReview writes an `untrusted_review` decision checkpoint task_message
// and transitions the task RUNNING/LEASED → AWAITING_INPUT, reusing parkForBudget's
// exact seam incl. the guarded conditional-update + steering notify + paused
// event (I6). A sibling of parkForBudget, not a fresh branch. The sources are
// lineage-scoped (I4) and the shown list may be truncated — source_count is the
// FULL pre-cap TotalSources so the operator knows when they haven't seen the
// whole set (F-cap). The latch key (source_set_hash) is over the full pre-cap
// set, so an overflow source still re-parks on a later run.
func (e *Executor) parkForTaintReview(ctx context.Context, task *persistence.Task, execution *persistence.Execution, sig *TaintReviewSignal) error {
	if sig == nil {
		return fmt.Errorf("parkForTaintReview: nil signal")
	}
	if e.taskMessageRepo == nil || e.persistTaskRepo == nil {
		return fmt.Errorf("cannot park for taint review: conversational repos not wired")
	}

	// The fail-closed park has no source walk behind it, so the source-count
	// wording would claim "0 external source(s)" — indistinguishable from a
	// clean result. Say what actually happened instead.
	question := fmt.Sprintf(
		"This task attempted an autonomous forge write derived from untrusted content (%d external source(s); showing the first %d). Review the sources, then resume to allow the write or cancel to block it. Resuming RE-RUNS the task from the start; a re-run that pulls a NEW untrusted source will pause again for review.",
		sig.SourceCount, sig.ShownCount,
	)
	reason := "tainted_write"
	if sig.LineageUnavailable {
		question = "This task attempted an autonomous forge write, but its untrusted-source lineage could NOT be determined (enforce mode is active and the lineage repositories or task identity are unavailable). The write is held rather than allowed unreviewed. Resume only if you are satisfied the write is safe; cancel to block it. If this recurs, the daemon's taint-lineage wiring needs attention."
		reason = "lineage_unavailable"
	}

	meta, _ := json.Marshal(map[string]any{
		"kind": "decision",
		"decision": map[string]any{
			"kind":                taintlineage.CheckpointDecisionKind,
			"reason":              reason,
			"write_surface":       "forge",
			"source_count":        sig.SourceCount,
			"shown_count":         sig.ShownCount,
			"source_set_hash":     sig.SourceSetHash,
			"sources":             sig.Sources,
			"lineage_unavailable": sig.LineageUnavailable,
		},
		"question": question,
		"options": []map[string]any{
			{"id": "allow", "label": "Reviewed — resume & allow"},
			{"id": "cancel", "label": "Block (cancel)"},
		},
	})

	msg := &persistence.TaskMessage{
		TaskID:      task.ID,
		ExecutionID: &execution.ID,
		AuthorKind:  persistence.TaskMessageAuthorLead,
		MessageKind: persistence.TaskMessageKindCheckpoint,
		Content:     question,
		Metadata:    meta,
		CreatedAt:   time.Now().UTC(),
	}
	if err := e.taskMessageRepo.Insert(ctx, msg); err != nil {
		return fmt.Errorf("insert taint-review checkpoint: %w", err)
	}

	ok, err := e.persistTaskRepo.TransitionConditional(ctx, task.ID,
		[]persistence.TaskStatus{persistence.TaskStatusRunning, persistence.TaskStatusLeased},
		persistence.TaskStatusAwaitingInput,
		persistence.TransitionOpts{ClearLease: true},
	)
	if err != nil {
		return fmt.Errorf("transition to AWAITING_INPUT for taint review: %w", err)
	}
	if !ok {
		// Task drifted (cancel race) — the checkpoint is written and visible.
		e.logger.Warn().
			Str("task_id", task.ID).
			Str("execution_id", execution.ID).
			Msg("taint-review checkpoint emitted but task drifted out of RUNNING — transition no-op")
		return nil
	}
	task.Status = persistence.TaskStatusAwaitingInput
	e.notifySteering(ctx, task, string(persistence.TaskStatusAwaitingInput))
	e.emitPaused(ctx, execution.ID, "awaiting_input")
	return nil
}
