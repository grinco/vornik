package workflowhealing

// Production ReplayEngine adapter (LLD § "Next slice — ReplayEngine
// adapter"). Bridges the trial runner's narrow ReplayEngine seam to
// the contracts.HealingApplier CE/EE boundary — zero imports of
// internal/blackbox.
//
// Control flow:
//
//   - BaselineTrace  → applier.BaselineTrace(taskID) after optional
//                      exec→task id resolution via the executions repo.
//   - ReplayEvidence → applier.ApplyPlan(CounterfactualPlan) → get a
//                      trace whose TaskID is the spawned replay task,
//                      then waitForSettle, then applier.BaselineTrace(
//                      replayTaskID) to obtain the fully settled trace.
//
// Non-production guarantees (LLD § safety):
//   - ApplyPlan ALWAYS creates a NEW task, so replayTaskID is never
//     the original evidence task id (the runner double-checks this).
//   - The CounterfactualVariable = "workflow" mutation marks the
//     payload a counterfactual replay, which engages the MCP side-
//     effect gate (internal/blackbox/sideeffects.go) via the EE
//     adapter — the CE layer never needs to know the details.
//
// Replay is a promotion SIGNAL, not a proof: the assembled candidate
// trace feeds the runner's aggregator + scorecard, which already
// surfaces low-fidelity / inconclusive outcomes via the
// ExecutionTrace.Inconclusive field set by the EE adapter.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/contracts"
	"vornik.io/vornik/internal/persistence"
)

// CounterfactualVariableWorkflow is the counterfactual variable used for
// self-healing trial replays (routes the replay at an alternate workflow
// genome). Mirrors blackbox.VariableWorkflow without importing
// internal/blackbox.
const CounterfactualVariableWorkflow = "workflow"

// TaskStatusReader reads a task by id so the adapter can block until a
// spawned replay settles. *postgres.TaskRepository (and the storage
// Repositories.Tasks) satisfies it.
type TaskStatusReader interface {
	Get(ctx context.Context, id string) (*persistence.Task, error)
}

// ExecutionReader resolves an evidence id to its execution row. The
// healing trigger stores evidence as EXECUTION ids
// (evidence_execution_ids, exec_…), but the HealingApplier and trace
// assembler are keyed by TASK id — without this resolution every replay
// was skipped with "blackbox: task not found" and the trial came back
// inconclusive (2026-06-06 incident).
// persistence.ExecutionRepository satisfies it. Optional: nil → ids
// pass through as task ids (pre-resolution behaviour).
type ExecutionReader interface {
	Get(ctx context.Context, id string) (*persistence.Execution, error)
}

// ReplayAdapterOptions tunes the settle-poll loop. Zero values fall
// back to sensible defaults.
type ReplayAdapterOptions struct {
	// PollInterval between task-status reads. Default 2s.
	PollInterval time.Duration
	// PollTimeout is the max time to wait for a spawned replay to
	// reach a terminal status before giving up (returns an error, so
	// the runner skips that evidence rather than hanging). Default
	// 10m — healing replays run the full workflow.
	PollTimeout time.Duration
}

func (o ReplayAdapterOptions) withDefaults() ReplayAdapterOptions {
	if o.PollInterval <= 0 {
		o.PollInterval = 2 * time.Second
	}
	if o.PollTimeout <= 0 {
		o.PollTimeout = 10 * time.Minute
	}
	return o
}

// replayEngineAdapter is the production ReplayEngine, bridging the
// trial runner to the contracts.HealingApplier EE seam.
type replayEngineAdapter struct {
	applier contracts.HealingApplier
	tasks   TaskStatusReader
	execs   ExecutionReader
	opts    ReplayAdapterOptions
	log     zerolog.Logger
}

// NewReplayEngineAdapter wires the production ReplayEngine. applier and
// tasks are required; a nil one yields a nil engine (the runner then
// treats replay as not-wired and errors cleanly, never panics). execs is
// the evidence-id → task resolution source — pass the executions
// repository; nil tolerated (ids then pass through as task ids, which
// silently breaks exec_…-keyed evidence — only omit in tests).
func NewReplayEngineAdapter(
	applier contracts.HealingApplier,
	tasks TaskStatusReader,
	execs ExecutionReader,
	opts ReplayAdapterOptions,
	log zerolog.Logger,
) ReplayEngine {
	if applier == nil || tasks == nil {
		return nil
	}
	return &replayEngineAdapter{
		applier: applier,
		tasks:   tasks,
		execs:   execs,
		opts:    opts.withDefaults(),
		log:     log,
	}
}

// resolveTaskID maps an evidence id to the task the HealingApplier
// layers are keyed by. The trigger stores execution ids (exec_…); when
// the executions repo knows the id, its TaskID wins. An unknown id passes
// through unchanged (it may already be a task id). Read errors other
// than not-found surface — they mean we can't TELL what the id is,
// not that it's a task id.
func (a *replayEngineAdapter) resolveTaskID(ctx context.Context, evidenceID string) (string, error) {
	if a.execs == nil {
		return evidenceID, nil
	}
	ex, err := a.execs.Get(ctx, evidenceID)
	switch {
	case errors.Is(err, persistence.ErrNotFound):
		return evidenceID, nil
	case err != nil:
		return "", fmt.Errorf("resolve evidence %s: %w", evidenceID, err)
	case ex == nil || ex.TaskID == "":
		return evidenceID, nil
	}
	return ex.TaskID, nil
}

// BaselineTrace assembles the recorded trace of the original evidence
// execution. No new task is created.
func (a *replayEngineAdapter) BaselineTrace(ctx context.Context, evidenceID string) (*contracts.ExecutionTrace, error) {
	taskID, err := a.resolveTaskID(ctx, evidenceID)
	if err != nil {
		return nil, fmt.Errorf("baseline trace for %s: %w", evidenceID, err)
	}
	tr, err := a.applier.BaselineTrace(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("baseline trace for %s: %w", evidenceID, err)
	}
	return &tr, nil
}

// ReplayEvidence spawns a candidate-genome replay of the evidence
// execution, blocks until it settles, and returns the spawned task id
// + its assembled trace.
func (a *replayEngineAdapter) ReplayEvidence(ctx context.Context, evidenceID, candidateWorkflowID string) (string, *contracts.ExecutionTrace, error) {
	taskID, err := a.resolveTaskID(ctx, evidenceID)
	if err != nil {
		return "", nil, fmt.Errorf("apply candidate replay for %s: %w", evidenceID, err)
	}
	plan := contracts.CounterfactualPlan{
		OriginalTaskID: taskID,
		Variable:       CounterfactualVariableWorkflow,
		Value:          candidateWorkflowID,
		Label:          "self-healing trial: " + candidateWorkflowID,
	}
	spawned, err := a.applier.ApplyPlan(ctx, plan)
	if err != nil {
		// Apply's contract: a non-empty TaskID alongside an error means the
		// task WAS created but the counterfactual provenance stamp failed —
		// typically the scheduler race (no execution row exists while the task
		// is QUEUED, which is the normal case for healing replays). The replay
		// is still routable and side-effect-gated (the payload marker, not the
		// stamp, engages the MCP gate); losing the stamp only degrades the
		// blackbox side-by-side view. Aborting here is worse: it orphans the
		// queued task AND the trial's deferred transient-genome deregistration
		// then strands it ("workflow …-candidate-… not found", 2026-06-06).
		if spawned.TaskID == "" {
			return "", nil, fmt.Errorf("apply candidate replay for %s: %w", evidenceID, err)
		}
		a.log.Warn().Err(err).
			Str("evidence_id", evidenceID).
			Str("replay_task_id", spawned.TaskID).
			Msg("workflowhealing: counterfactual stamp degraded; continuing replay")
	}
	if spawned.TaskID == "" {
		return "", nil, errors.New("replay engine returned no task")
	}
	// Defence in depth for the LLD non-production invariant (the runner also
	// checks): a replay MUST be a fresh task, never the original.
	if spawned.TaskID == taskID {
		return "", nil, fmt.Errorf("replay returned the original task id %s (non-production invariant)", taskID)
	}

	if err := a.waitForSettle(ctx, spawned.TaskID); err != nil {
		return spawned.TaskID, nil, err
	}

	// Fetch the fully-settled trace via BaselineTrace (the EE adapter reads
	// the assembled + cached trace for any task id, including replay tasks).
	fullTrace, err := a.applier.BaselineTrace(ctx, spawned.TaskID)
	if err != nil {
		return spawned.TaskID, nil, fmt.Errorf("assemble candidate trace for %s: %w", spawned.TaskID, err)
	}
	return spawned.TaskID, &fullTrace, nil
}

// waitForSettle polls the task until it reaches a terminal status or
// the timeout/ctx fires. A FAILED replay is NOT an error here — the
// runner's aggregator counts it as a candidate failure, which is a
// legitimate (and important) trial signal. Only an inability to
// OBSERVE the outcome (timeout, repo error) is returned as an error.
func (a *replayEngineAdapter) waitForSettle(ctx context.Context, taskID string) error {
	deadline := time.NewTimer(a.opts.PollTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(a.opts.PollInterval)
	defer ticker.Stop()

	for {
		task, err := a.tasks.Get(ctx, taskID)
		if err != nil {
			return fmt.Errorf("poll replay task %s: %w", taskID, err)
		}
		if task != nil && isTerminalStatus(task.Status) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("replay task %s did not settle within %s", taskID, a.opts.PollTimeout)
		case <-ticker.C:
		}
	}
}

// isTerminalStatus reports whether a task status is terminal (the
// replay has stopped running and a trace can be assembled).
func isTerminalStatus(s persistence.TaskStatus) bool {
	switch s {
	case persistence.TaskStatusCompleted,
		persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled,
		persistence.TaskStatusClosed:
		return true
	default:
		return false
	}
}
