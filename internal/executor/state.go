package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// executionState tracks the progress of a workflow execution across steps.
type executionState struct {
	CurrentStepID  string          `json:"currentStepId,omitempty"`
	CompletedSteps []string        `json:"completedSteps,omitempty"`
	LastResult     json.RawMessage `json:"lastResult,omitempty"`
	// StepResults is the per-step result.json body, keyed by
	// step ID. Populated on every successful step termination
	// so downstream steps (call_project + spawn_project payloads
	// in particular) can interpolate ${outputs.<step>.<field>}
	// references. Bounded by workflow length × result size —
	// typically <100 KB total.
	//
	// Inter-project orchestration Phase D (LLD §6.1).
	StepResults         map[string]json.RawMessage `json:"stepResults,omitempty"`
	ApprovalPendingStep string                     `json:"approvalPendingStep,omitempty"`
	ApprovalGrantedStep string                     `json:"approvalGrantedStep,omitempty"`
	VisitCounts         map[string]int             `json:"visitCounts,omitempty"`
	Iterations          int                        `json:"iterations,omitempty"`
	// PlanSteps and PlanIndex track progress within a "plan" step so that
	// execution can resume from the right role after a daemon restart.
	PlanSteps       []string `json:"planSteps,omitempty"`
	PlanIndex       int      `json:"planIndex,omitempty"`
	PlanLeadMessage string   `json:"planLeadMessage,omitempty"` // coordinator message forwarded as context to the first planned role
	// PlanStartHEAD is the git HEAD of the worktree sampled before the
	// lead planning step runs. It bounds the range used for patch
	// generation so commits made during lead planning (the lead can
	// opt to do the whole task itself) and during per-role execution
	// are all captured. Persisted across checkpoints so a resumed
	// execution doesn't re-sample after later commits.
	PlanStartHEAD string `json:"planStartHEAD,omitempty"`
	// PlanLeadStepID is the synthetic step ID of the lead planning
	// row. The lead's pending_validation outcome is held open until
	// the rest of the plan has run so the executor can attribute
	// child failures back to the lead — a child that fails marks
	// the lead's row as `downstream_rejected` with
	// attributed_to_step pointing at the bad child. Persisted so a
	// resume after a daemon restart still finalizes the right row.
	PlanLeadStepID string `json:"planLeadStepID,omitempty"`
	// PausedReason distinguishes WHY an execution was paused so the
	// Recover() path on the next daemon start knows which paused
	// executions should auto-resume vs. which need an external
	// signal.
	//
	//   "shutdown"          — the daemon was stopped cleanly. Auto-
	//                         resume on next start.
	//   "awaiting_children" — delegation pause; resumes when child
	//                         tasks complete (queue / scheduler
	//                         drives this, not Recover).
	//   "operator"          — manual vornikctl Pause; stays paused
	//                         until vornikctl Resume.
	//   ""                  — legacy / unknown; default to NOT
	//                         auto-resuming so we don't surprise an
	//                         operator who paused for a reason we
	//                         can't read.
	PausedReason string `json:"pausedReason,omitempty"`
	// PendingRecovery carries failure context from the previous
	// step to the next (typically a lead 'recover' role) so the
	// recovery step can propose alternative approaches via a
	// `decision` checkpoint instead of just failing the task.
	// Populated by workflow.go's on_fail handler when the failing
	// error has structured signals (RecoverableVerifierError,
	// future failure classes per the swarm-recovery LLD); cleared
	// by the recovery step's normal completion. nil = no recovery
	// context pending for the next step.
	PendingRecovery *RecoveryContext `json:"pendingRecovery,omitempty"`
	// InFlightStepID / InFlightContainerID / InFlightTempRoot record the
	// container running the CURRENT step, persisted right after it starts and
	// before the executor blocks on its exit. If the daemon crashes UNCLEANLY
	// mid-step (a clean shutdown drains the container first), recovery adopts
	// the still-existing container instead of re-spawning — so the step's side
	// effects don't run twice (executor crash-mid-step idempotency). These are
	// cleared implicitly when the workflow loop's next saveCheckpoint
	// overwrites the snapshot from its in-memory state (which carries none).
	InFlightStepID      string `json:"inFlightStepId,omitempty"`
	InFlightContainerID string `json:"inFlightContainerId,omitempty"`
	InFlightTempRoot    string `json:"inFlightTempRoot,omitempty"`
	// ComplexityTier is the planner's complexity verdict for this task
	// (trivial|standard|complex|open_ended). Written by the planning
	// step's outcome handler (lead outcome, or dev-pipeline analyst),
	// read on every subsequent worker spawn to scale the role's
	// tool-iteration budget. Empty = standard (no scaling). Part of the
	// snapshot so it survives resume. See
	// https://docs.vornik.io
	ComplexityTier string `json:"complexityTier,omitempty"`
}

// Constants for the executionState.PausedReason field.
const (
	PauseReasonShutdown         = "shutdown"
	PauseReasonAwaitingChildren = "awaiting_children"
	PauseReasonOperator         = "operator"
	// PauseReasonRetryFromStep — operator clicked "Retry from
	// step…" on a FAILED execution. The retry-from-step handler
	// rewinds state.CurrentStepID + trims state.CompletedSteps to
	// the chosen step, marks downstream outcomes as superseded,
	// and parks the execution as Paused. The recover loop picks
	// the row up the same way it does PauseReasonShutdown — flips
	// to RUNNING and resumes via recoverExecution, which starts at
	// state.CurrentStepID. Added 2026.6.0 (SaaS-readiness arc).
	PauseReasonRetryFromStep = "retry_from_step"
)

// ExecutionCheckpoint represents the state stored when pausing an execution.
type ExecutionCheckpoint struct {
	TaskID        string    `json:"taskId"`
	ProjectID     string    `json:"projectId"`
	ContainerID   string    `json:"containerId,omitempty"`
	StartedAt     time.Time `json:"startedAt"`
	PausedAt      time.Time `json:"pausedAt"`
	CurrentStepID string    `json:"currentStepId,omitempty"`
}

// PauseStatus represents the paused state of an execution.
type PauseStatus struct {
	TaskID      string
	ExecutionID string
	PausedAt    time.Time
}

// ResumeStatus represents the resumed state of an execution.
type ResumeStatus struct {
	TaskID      string
	ExecutionID string
	ResumedAt   time.Time
}

// saveCheckpoint persists the current workflow position and state.
func (e *Executor) saveCheckpoint(ctx context.Context, execution *persistence.Execution, nextStepID string, completedSteps []string, state executionState) error {
	state.CurrentStepID = nextStepID
	state.CompletedSteps = append([]string{}, completedSteps...)
	return e.saveExecutionState(ctx, execution, state)
}

// saveExecutionState marshals the state and writes it to the repository.
func (e *Executor) saveExecutionState(ctx context.Context, execution *persistence.Execution, state executionState) error {
	snapshot, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to marshal execution checkpoint: %w", err)
	}
	execution.CompletedSteps = append([]string{}, state.CompletedSteps...)
	if state.CurrentStepID != "" {
		execution.CurrentStepID = &state.CurrentStepID
	}
	execution.StateSnapshot = snapshot
	return e.execRepo.SaveStateSnapshot(ctx, execution.ID, snapshot, state.CurrentStepID, state.CompletedSteps)
}

// markStepInFlight records the running container + its temp root for the
// current step so a crash-recovery can adopt it (see executionState's
// InFlight* fields). Best-effort: a save failure just means recovery
// re-spawns (the safe default), so we log and continue rather than failing
// the step. Loads the current snapshot (which the workflow loop checkpointed
// with CurrentStepID=stepID just before dispatch) so the other state fields
// are preserved.
func (e *Executor) markStepInFlight(ctx context.Context, execution *persistence.Execution, stepID, containerID, tempRoot string) {
	if e == nil || execution == nil || containerID == "" {
		return
	}
	st := loadExecutionState(execution)
	st.InFlightStepID = stepID
	st.InFlightContainerID = containerID
	st.InFlightTempRoot = tempRoot
	if err := e.saveExecutionState(ctx, execution, st); err != nil {
		e.logger.Warn().Err(err).Str("execution_id", execution.ID).Str("step", stepID).
			Msg("re-attach: failed to record in-flight container; a crash here would re-spawn the step")
	}
}

// reattachInFlightContainer reports whether stepID has an in-flight container
// from a prior (crashed) run that can be ADOPTED instead of re-spawned, and
// if so returns its id + the output dir to read result.json from. Returns
// ok=false (→ run the step fresh) on any uncertainty: no record, a record for
// a different step, or a container the runtime can no longer find (e.g. pruned
// or lost to a host reboot — in which case the temp root is gone too, so
// re-spawn is correct). This is the ONLY place re-attach changes behavior, and
// it changes it only on the recovery path; a fresh run never has a matching
// record at handler entry.
func (e *Executor) reattachInFlightContainer(ctx context.Context, execution *persistence.Execution, stepID string) (containerID, outputDir string, ok bool) {
	if e == nil || execution == nil || e.runtime == nil {
		return "", "", false
	}
	st := loadExecutionState(execution)
	if st.InFlightStepID != stepID || st.InFlightContainerID == "" || st.InFlightTempRoot == "" {
		return "", "", false
	}
	c, err := e.runtime.InspectContainer(ctx, st.InFlightContainerID)
	if err != nil || c == nil {
		e.logger.Info().Str("execution_id", execution.ID).Str("step", stepID).
			Str("container_id", st.InFlightContainerID).
			Msg("re-attach: in-flight container not found — running the step fresh")
		return "", "", false
	}
	// Container still exists (running or exited). waitForCompletion handles
	// both: it blocks on a running one and returns immediately for an exited
	// one. result.json sits in the original run's output dir.
	e.logger.Info().Str("execution_id", execution.ID).Str("step", stepID).
		Str("container_id", st.InFlightContainerID).Str("status", string(c.Status)).
		Msg("re-attach: adopting in-flight container after recovery (not re-spawning)")
	return st.InFlightContainerID, filepath.Join(st.InFlightTempRoot, "output"), true
}

// loadExecutionState reconstructs the execution state from a persisted execution record.
func loadExecutionState(execution *persistence.Execution) executionState {
	state := executionState{}
	if execution == nil {
		return state
	}
	if len(execution.StateSnapshot) > 0 {
		_ = json.Unmarshal(execution.StateSnapshot, &state)
	}
	if state.CurrentStepID == "" && execution.CurrentStepID != nil {
		state.CurrentStepID = *execution.CurrentStepID
	}
	if len(state.CompletedSteps) == 0 && len(execution.CompletedSteps) > 0 {
		state.CompletedSteps = append([]string{}, execution.CompletedSteps...)
	}
	return state
}

// Pause stops a running execution temporarily, preserving its state for
// resume. Reason is stored in the state snapshot so Recover() can tell
// shutdown-paused executions (auto-resume) from operator-paused ones
// (stay paused until explicit Resume).
func (e *Executor) Pause(taskID string) (*PauseStatus, error) {
	return e.pauseWithReason(taskID, PauseReasonOperator)
}

// pauseWithReason is the internal pause path. Pause() (operator-driven)
// and Shutdown() (daemon-stopping) share the bulk of the work; only
// the reason recorded in the state snapshot differs. Keeping it as a
// single function means future invariants (lock ordering, container
// stop strategy, error semantics) only have to be maintained once.
func (e *Executor) pauseWithReason(taskID, reason string) (*PauseStatus, error) {
	e.mu.Lock()
	handle, exists := e.activeExecutions[taskID]
	e.mu.Unlock()

	if !exists {
		return nil, fmt.Errorf("no active execution for task %s", taskID)
	}

	// Stop the container gracefully (SIGTERM), then BLOCK on its exit
	// before returning. The agent's defer hooks need a few seconds to
	// flush result.json + tool audit entries; more importantly, the
	// daemon must not exit while the container is still running and
	// holding its worktree bind-mount. If we let pause return early
	// during a daemon shutdown, the next daemon process boots with
	// orphan containers still alive — and its pruneAllWorktrees pass
	// then yanks the worktree directory out from under those orphans,
	// producing the cascading "No such file or directory" failure
	// (regression observed 2026-05-07 after a daemon rebuild).
	//
	// Timeout budget: 30s covers a normal SIGTERM-to-exit cycle (most
	// agents drain in <5s); on miss, we fall through and let the
	// caller proceed rather than stalling shutdown indefinitely.
	if handle.containerID != "" {
		if err := e.runtime.StopContainer(context.Background(), handle.containerID, false); err != nil {
			// Log but continue - container might already be stopped
			// Use force stop as fallback
			_ = e.runtime.StopContainer(context.Background(), handle.containerID, true)
		}
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 30*time.Second)
		if _, err := e.runtime.WaitForExit(waitCtx, handle.containerID, 30*time.Second); err != nil {
			e.logger.Warn().
				Err(err).
				Str("task_id", taskID).
				Str("container_id", handle.containerID).
				Msg("pause: container did not exit within 30s of SIGTERM — orphan window risk on next daemon start")
		}
		waitCancel()
	}

	// Get execution record for the execution ID
	exec, err := e.execRepo.GetByTaskID(context.Background(), taskID)
	if err != nil {
		return nil, fmt.Errorf("failed to get execution record: %w", err)
	}

	// Stamp the pause reason into the state snapshot BEFORE marking
	// the execution PAUSED. The DB transition is what Recover() reads
	// on the next start; the state.PausedReason in the snapshot is
	// what tells Recover() how to dispatch. Writing the reason first
	// avoids a race where a daemon crash between the two writes
	// leaves the row PAUSED with no reason.
	state := loadExecutionState(exec)
	state.PausedReason = reason
	if err := e.saveExecutionState(context.Background(), exec, state); err != nil {
		// Log but continue — the worst case is Recover() sees a
		// PAUSED execution without a reason and skips it (caller
		// can still Resume manually).
		e.logger.Warn().Err(err).Str("execution_id", exec.ID).
			Msg("pause: failed to stamp PausedReason in state snapshot")
	}

	// Update execution status to paused
	if err := e.execRepo.UpdateStatus(context.Background(), exec.ID, persistence.ExecutionStatusPaused); err != nil {
		return nil, fmt.Errorf("failed to update execution status: %w", err)
	}

	// Flip task.Status to PAUSED so the goroutine's subsequent
	// handleFailure/handleSuccess can detect "operator paused me"
	// and skip the terminal-status write. Pre-fix this was missing
	// entirely — operator-initiated pause flipped EXECUTION status
	// to PAUSED but task.Status stayed RUNNING; the in-flight
	// goroutine then finalised as FAILED/COMPLETED and overwrote
	// the operator's intent. Live evidence: T-…1c44 (2026-05-23) —
	// task paused at 17:02:33, merge step ran 24s later, FAILED
	// overwrote PAUSED. The conditional UpdateStatus call here
	// uses the bare UpdateStatus (not TransitionConditional)
	// because the validate-transition check in the caller already
	// gated on RUNNING; if the row drifted between then and here,
	// the race is benign — handleFailure's defensive check below
	// catches it.
	if err := e.taskRepo.UpdateStatus(context.Background(), taskID, persistence.TaskStatusPaused); err != nil {
		// Soft failure — the execution-side PAUSED already
		// landed and handleFailure's defensive check still
		// guards against terminal overwrite. Logging is enough.
		e.logger.Warn().Err(err).Str("task_id", taskID).
			Msg("pause: failed to flip task.Status to PAUSED; relying on handleFailure guard")
	}

	// Cancel the execution context. The runExecution goroutine
	// observes ctx.Done() in its loop and exits via the deferred
	// cleanupExecution, which removes the entry from
	// activeExecutions. We BLOCK here until the entry is gone (or
	// the timeout fires) so callers — primarily the pause API
	// endpoint — can rely on a clean executor state on return.
	//
	// Pre-fix, Pause() returned immediately after cancel(); the
	// goroutine's cleanup happened on a later scheduling pass. A
	// Resume that arrived in the meantime found the orphan entry
	// and the scheduler dispatch path got "task is already being
	// executed" → ReleaseLease(FAILED). Live evidence:
	// exec_8bec1d…5e89 (2026-05-10) — operator paused at 18:47:24
	// then resumed at 18:48:33; the dispatch loop's coarse
	// renewal cadence (~30s) hadn't escalated the running
	// goroutine's lease-renewal failures into a Cancel before
	// Resume tried to re-dispatch. Result: every pause/resume
	// cycle terminal-failed the task.
	handle.cancel()
	e.waitForExecutionCleanup(taskID, 30*time.Second)

	return &PauseStatus{
		TaskID:      taskID,
		ExecutionID: exec.ID,
		PausedAt:    time.Now(),
	}, nil
}

// waitForExecutionCleanup blocks until activeExecutions[taskID]
// has been removed (the goroutine's deferred cleanupExecution
// fired) or the timeout expires. Polls every 25ms — the goroutine's
// next ctx.Done() observation is bounded by its own select tick,
// so a short poll catches it within one cycle without a heavy
// channel-signal apparatus.
//
// Returns true when the entry was cleared, false on timeout.
// Caller decides whether to log/escalate on timeout — most code
// paths just continue and let the scheduler's recovery sweep
// catch a stuck handle later.
func (e *Executor) waitForExecutionCleanup(taskID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		e.mu.Lock()
		_, present := e.activeExecutions[taskID]
		e.mu.Unlock()
		if !present {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// ResumeTask is the error-only wrapper around Resume that satisfies
// the ui.ExecutorInterface contract. The UI doesn't need the
// ResumeStatus payload — it only checks success/error to decide
// between in-place resume vs fresh dispatch fallback (2026-05-26
// fix — the UI resume button used to flip task→QUEUED creating a
// new execution while the paused one sat parked).
func (e *Executor) ResumeTask(taskID string) error {
	_, err := e.Resume(taskID)
	return err
}

// Resume continues a paused execution.
func (e *Executor) Resume(taskID string) (*ResumeStatus, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Check if task is already being executed
	if _, exists := e.activeExecutions[taskID]; exists {
		return nil, fmt.Errorf("task %s is already being executed", taskID)
	}

	// Get the task
	taskCtx := e.ctx
	if taskCtx == nil {
		taskCtx = context.Background()
	}
	task, err := e.taskRepo.Get(taskCtx, taskID)
	if err != nil {
		return nil, fmt.Errorf("failed to get task %s: %w", taskID, err)
	}

	// Get execution record
	exec, err := e.execRepo.GetByTaskID(context.Background(), taskID)
	if err != nil {
		return nil, fmt.Errorf("failed to get execution record: %w", err)
	}

	// Verify execution is paused
	if exec.Status != persistence.ExecutionStatusPaused {
		return nil, fmt.Errorf("execution is not paused (status: %s)", exec.Status)
	}

	state := loadExecutionState(exec)
	if state.ApprovalPendingStep != "" {
		state.ApprovalGrantedStep = state.ApprovalPendingStep
		state.ApprovalPendingStep = ""
		state.PausedReason = ""
		if err := e.saveExecutionState(context.Background(), exec, state); err != nil {
			return nil, fmt.Errorf("failed to persist approval resume state: %w", err)
		}
	} else if state.PausedReason == PauseReasonOperator {
		state.PausedReason = ""
		if err := e.saveExecutionState(context.Background(), exec, state); err != nil {
			return nil, fmt.Errorf("failed to clear operator pause reason: %w", err)
		}
	}

	// Initialize context if needed
	if e.ctx == nil {
		e.ctx, e.cancel = context.WithCancel(context.Background())
	}

	// Update execution status to running
	if err := e.execRepo.UpdateStatus(context.Background(), exec.ID, persistence.ExecutionStatusRunning); err != nil {
		return nil, fmt.Errorf("failed to update execution status: %w", err)
	}
	if err := e.taskRepo.UpdateStatus(context.Background(), taskID, persistence.TaskStatusRunning); err != nil {
		return nil, fmt.Errorf("failed to update task status: %w", err)
	}

	// Create new execution handle
	execCtx, cancel := context.WithCancel(e.ctx)
	handle := &executionHandle{
		taskID:    taskID,
		projectID: task.ProjectID,
		startedAt: time.Now(),
		cancel:    cancel,
		ctx:       execCtx,
	}
	e.activeExecutions[taskID] = handle
	e.syncActiveGaugeLocked()

	// Restart execution in background
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		e.runExecution(execCtx, task, exec)
	}()

	return &ResumeStatus{
		TaskID:      taskID,
		ExecutionID: exec.ID,
		ResumedAt:   time.Now(),
	}, nil
}
