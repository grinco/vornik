package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
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
	// ParallelJoin is the DESCRIPTOR written once at the pause checkpoint of
	// a declarative `parallel` fan-out step (parallel-fanout LLD §4.5). It
	// carries the join step id, the join policy, and the branch→child
	// mapping so the CROSS-GOROUTINE wake path
	// (unblockParentIfChildrenDone) can evaluate the join policy without
	// loading the workflow definition — and so a legacy delegation join,
	// which leaves this nil, keeps its byte-identical all-or-nothing
	// behaviour. NO verdict is persisted here: succeeded/total are derived
	// at resume from GetChildren. Written once per WAITING_FOR_CHILDREN
	// window by the paused parent and read-only for the rest of that
	// window (the wake path reads but never mutates it). Nil for every
	// non-parallel execution.
	ParallelJoin *ParallelJoinState `json:"parallelJoin,omitempty"`
}

// ParallelJoinState is the descriptor-only record of an in-flight parallel
// fan-out's join (parallel-fanout LLD §4.5). It is written once at the pause
// checkpoint and never carries a verdict — the join policy is re-evaluated
// from the authoritative child statuses (GetChildren) both by the wake path
// and at resume.
type ParallelJoinState struct {
	// JoinStepID is the non-parallel consumer step the parent resumes at.
	JoinStepID string `json:"joinStepId"`
	// Policy is the raw join_policy string (all | best_effort | quorum:<n>).
	// Carried so the wake path evaluates the policy without the workflow def.
	Policy string `json:"policy"`
	// ChildTaskIDs are the delegated leg task ids (descriptor / telemetry).
	ChildTaskIDs []string `json:"childTaskIds,omitempty"`
	// BranchIDs are the declared branch ids, in workflow order (lets the
	// resume-time telemetry name legs).
	BranchIDs []string `json:"branchIds,omitempty"`
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

// sanitizeCheckpointBytes replaces any step result that is not valid JSON with a
// diagnostic that is, and reports which slots it repaired.
//
// THE GUARD BELONGS HERE. Step results are opaque bytes the executor did not
// encode — an agent's result.json, a plan step's output, a system handler's
// envelope — stored into json.RawMessage fields and marshalled later. Invalid
// bytes therefore do not fail where they were produced; they fail at the next
// checkpoint and take the whole execution with them, several steps from the
// cause. That was fixed once at ONE producer (70aafb58, step error text) and
// went on killing executions through the pass-through sites — 7 of them in the
// 36 hours journald still held on 2026-08-22 (see stepResultJSON for the
// measurement).
// Asking at each producer means asking again for every producer added later, so
// the last thing before the marshal asks too.
//
// StepResults is a map and so is repaired in the CALLER's state as well as in
// this copy — deliberately: the workflow loop keeps interpolating
// ${outputs.<step>.<field>} from it for the rest of the run, and repairing only
// what is written would leave the live copy unusable. LastResult is a value
// field and is repaired for the write only; the loop reassigns it every step.
func sanitizeCheckpointBytes(state *executionState) []string {
	if state == nil {
		return nil
	}
	var repaired []string
	if len(state.LastResult) > 0 && !json.Valid(state.LastResult) {
		state.LastResult = stepResultJSON(state.LastResult)
		repaired = append(repaired, "lastResult")
	}
	for id, raw := range state.StepResults {
		if len(raw) == 0 || json.Valid(raw) {
			continue
		}
		state.StepResults[id] = stepResultJSON(raw)
		repaired = append(repaired, "stepResults."+id)
	}
	sort.Strings(repaired) // map order is random; the log line should not be
	return repaired
}

// saveExecutionState marshals the state and writes it to the repository.
//
// It is the single funnel every snapshot write in this package passes through,
// which is why the pause claim is applied HERE (pause_claim.go, design §5.4):
// the workflow loop holds an executionState loaded before a pause may have
// stamped its reason, and writing that copy back would erase it — leaving a
// PAUSED execution Recover() skips, stuck across a restart with nothing
// recording why.
func (e *Executor) saveExecutionState(ctx context.Context, execution *persistence.Execution, state executionState) error {
	if execution != nil {
		e.stampPauseClaim(execution.TaskID, &state)
	}
	if repaired := sanitizeCheckpointBytes(&state); len(repaired) > 0 {
		// Warn, not fail: the execution is recoverable and the alternative is
		// the fatal marshal this guard exists to prevent. Named slots so the
		// producer is identifiable without reading the snapshot.
		e.logger.Warn().
			Str("execution_id", execution.ID).
			Strs("repaired", repaired).
			Msg("workflow: step result was not valid JSON; stored it as text so the checkpoint survives")
	}
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
	if exists {
		// Claim the pause in the SAME critical section that resolved the
		// handle, so the goroutine cannot exit between the two and leave
		// the write unauthorised (pause_claim.go). A claim already
		// standing (Shutdown got here first) keeps its reason.
		e.claimPause(taskID, reason)
	}
	e.mu.Unlock()

	if !exists {
		// Same sentinel Cancel uses: callers distinguish "nothing to act on"
		// (benign) from a real failure with errors.Is rather than by matching
		// the message.
		//
		// Shutdown does NOT come through here — it claims its handles up
		// front and calls pauseClaimedHandle directly, because for a daemon
		// that is going away "nothing is running" is a race, not an answer.
		// The operator path keeps this behaviour deliberately: see the
		// pause-write-ownership design §5.2 and the separately filed P3
		// "Pause is gated on a stale task status too".
		return nil, fmt.Errorf("%w: task %s", ErrNoActiveExecution, taskID)
	}

	return e.pauseClaimedHandle(handle, reason)
}

// pauseClaimedHandle performs a pause whose claim has already been taken. It
// never consults activeExecutions: the handle it was given IS its authority,
// which is what lets a shutdown pause an execution whose goroutine has already
// exited (design §5.2). Because the map lookup no longer implies "this
// execution has not finished", both status writes are conditional (§5.3).
func (e *Executor) pauseClaimedHandle(handle *executionHandle, reason string) (*PauseStatus, error) {
	if handle == nil {
		return nil, fmt.Errorf("%w: nil execution handle", ErrNoActiveExecution)
	}
	taskID := handle.taskID

	// Snapshot the handle's mutable fields under the lock. containerID is
	// written by the runExecution goroutine under e.mu each time a step's
	// container starts (executor.go); reading it after Unlock is a data race,
	// and a lost race reads "" and skips StopContainer, orphaning a still-
	// billing container (B1, audit 2026-07-03). Mirrors Cancel.
	e.mu.Lock()
	containerID := handle.containerID
	e.mu.Unlock()

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
	if containerID != "" {
		if err := e.runtime.StopContainer(context.Background(), containerID, false); err != nil {
			// Log but continue - container might already be stopped
			// Use force stop as fallback
			_ = e.runtime.StopContainer(context.Background(), containerID, true)
		}
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 30*time.Second)
		if _, err := e.runtime.WaitForExit(waitCtx, containerID, 30*time.Second); err != nil {
			e.logger.Warn().
				Err(err).
				Str("task_id", taskID).
				Str("container_id", containerID).
				Msg("pause: container did not exit within 30s of SIGTERM — orphan window risk on next daemon start")
		}
		waitCancel()
	}

	// Get execution record for the execution ID
	exec, err := e.execRepo.GetByTaskID(context.Background(), taskID)
	if err != nil {
		e.releasePauseClaim(taskID)
		return nil, fmt.Errorf("failed to get execution record: %w", err)
	}

	// Stamp the pause reason into the state snapshot BEFORE marking
	// the execution PAUSED. The DB transition is what Recover() reads
	// on the next start; the state.PausedReason in the snapshot is
	// what tells Recover() how to dispatch. Writing the reason first
	// avoids a race where a daemon crash between the two writes
	// leaves the row PAUSED with no reason.
	state := loadExecutionState(exec)
	// Whether WE put the reason there. The stamp is speculative — it happens
	// before the conditional status write that decides whether the pause
	// takes — so a refused pause has to take it back (see below).
	stampedReason := false
	// Never overwrite a reason already on the row. A shutdown arriving on an
	// execution the operator paused must not re-stamp it as shutdown-paused:
	// Recover() auto-resumes that reason, so the daemon would overrule the
	// operator's intent to keep it stopped (design §5.3, review round 1 F2).
	// The same protection the conditional status write gives the status.
	if state.PausedReason == "" {
		state.PausedReason = reason
		stampedReason = true
	}
	if err := e.saveExecutionState(context.Background(), exec, state); err != nil {
		// Log but continue — the worst case is Recover() sees a
		// PAUSED execution without a reason and skips it (caller
		// can still Resume manually).
		e.logger.Warn().Err(err).Str("execution_id", exec.ID).
			Msg("pause: failed to stamp PausedReason in state snapshot")
	}

	// Update execution status to paused — conditionally. Dropping the
	// activeExecutions presence check (§5.2) also dropped the accidental
	// guarantee it carried: that the execution had not already finished. A
	// bare UpdateStatus here would reopen a row the goroutine just marked
	// COMPLETED, and Recover() would re-run a finished workflow on the next
	// start. PAUSED is absent from the `from` set on purpose — a pause must
	// not overwrite a pause.
	applied, err := e.execRepo.TransitionStatusConditional(context.Background(), exec.ID,
		[]persistence.ExecutionStatus{
			persistence.ExecutionStatusRunning,
			persistence.ExecutionStatusPending,
		}, persistence.ExecutionStatusPaused)
	if err != nil {
		e.releasePauseClaim(taskID)
		return nil, fmt.Errorf("failed to update execution status: %w", err)
	}
	if !applied {
		e.abandonRefusedPause(taskID, exec.ID, reason, stampedReason)
		handle.cancel()
		return nil, fmt.Errorf("%w: execution %s is not in a pausable state", ErrNoActiveExecution, exec.ID)
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
	if ok, terr := e.taskRepo.TransitionConditional(context.Background(), taskID,
		[]persistence.TaskStatus{
			persistence.TaskStatusRunning,
			persistence.TaskStatusLeased,
			persistence.TaskStatusPending,
		}, persistence.TaskStatusPaused, persistence.TransitionOpts{}); terr != nil {
		// Soft failure — the execution-side PAUSED already
		// landed and handleFailure's defensive check still
		// guards against terminal overwrite. Logging is enough.
		e.logger.Warn().Err(terr).Str("task_id", taskID).
			Msg("pause: failed to flip task.Status to PAUSED; relying on handleFailure guard")
	} else if !ok {
		// The task reached a terminal (or otherwise non-pausable) status
		// while we were pausing. The execution row is PAUSED and the task
		// is not; that mismatch is the honest record of what happened and
		// Recover() reads the task status before resuming, so it will not
		// re-drive a finished task.
		e.logger.Info().Str("task_id", taskID).
			Msg("pause: task was no longer in a pausable status — left as it stands")
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

// abandonRefusedPause undoes a pause whose conditional status write was
// refused. That is a legitimate outcome of a graceful shutdown, not a failure:
// the execution reached a terminal state, was cancelled, or was already paused
// while this pause was being decided.
//
// Two things have to be undone. The claim, so no later checkpoint re-stamps a
// pause that never happened. And the reason itself, which by this point has
// already been written: the stamp goes in BEFORE the status on purpose — a
// crash between the two must leave a recoverable row rather than a PAUSED one
// Recover() skips — so a pause that then does not take has already put its
// reason on the row, and a cancelled execution reading "paused because
// shutdown" is a lie about why it ended.
//
// Order matters: the claim is released FIRST, or saveExecutionState would
// stamp the reason straight back on (pause_claim.go). Only a reason this pause
// itself wrote is cleared — one that was already there belongs to someone else.
func (e *Executor) abandonRefusedPause(taskID, executionID, reason string, stampedReason bool) {
	// Why the read-modify-write below is safe without a lock, spelled out
	// because it does not look it: a reason is only ever stamped while a claim
	// is held (pause_claim.go's invariant), a second pause cannot take a claim
	// while the first stands, and the `== reason` test means the only thing
	// this can clear is a reason this pause itself wrote. A reason belonging
	// to anyone else is left exactly where it is.
	e.releasePauseClaim(taskID)
	if stampedReason {
		cur, gerr := e.execRepo.GetByTaskID(context.Background(), taskID)
		if gerr == nil && cur != nil {
			curState := loadExecutionState(cur)
			if curState.PausedReason == reason {
				curState.PausedReason = ""
				if serr := e.saveExecutionState(context.Background(), cur, curState); serr != nil {
					e.logger.Warn().Err(serr).Str("execution_id", executionID).
						Msg("pause: could not clear the speculative pause reason on a refused pause")
				}
			}
		}
	}
	e.logger.Info().
		Str("task_id", taskID).
		Str("execution_id", executionID).
		Str("reason", reason).
		Msg("pause: execution was no longer pausable — leaving its status as it stands")
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
//
// It holds e.mu for its whole body, database writes included. That is
// deliberate and predates the claim work: the function decides on the strength
// of activeExecutions not containing this task and then INSERTS into it, so
// dropping the lock in between would let a concurrent Execute or Resume
// dispatch the same task twice. The cost is that e.mu is held across two
// status writes and a snapshot write; the alternative is a second dispatch of
// a live execution, which is worse.
//
// This is also why claims have their own mutex: saveExecutionState consults
// the claim registry, and it is reached from here with e.mu already held (see
// pause_claim.go's locking note).
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

	// Release the claim BEFORE any state write below: while it stands,
	// saveExecutionState re-stamps its reason onto every snapshot, which
	// would put back the very field this path clears (design §5.5).
	// Deliberately not deferred — the writes are in this function. Safe to
	// call with e.mu held: claims take claimMu, and the order is e.mu →
	// claimMu everywhere.
	e.releasePauseClaim(taskID)

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
