package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// interProjectEnabledEnv is the feature-flag env name. When unset
// or false, every call_project step fails with
// CROSS_PROJECT_DISABLED — operators land the migration + code
// without exposing the surface until they're ready. Default off
// for the first release (LLD §11).
const interProjectEnabledEnv = "VORNIK_INTER_PROJECT_ENABLED"

// callProjectResult is the small structured shape the handler
// returns to executeWorkflowAttempt. Mirrors the executor's
// other step-handler returns enough that the dispatch site can
// thread the values into the existing checkpoint + transition
// machinery.
type callProjectResult struct {
	// CPCId is the cross_project_calls row id. Stashed on the
	// execution's state snapshot so the resume path can map
	// "child terminated" → "which CPC to resolve" without an
	// extra DB lookup.
	CPCId string
	// CalleeTaskID is the newly-created task in the callee
	// project. Same task that, when leased, runs the callee
	// workflow and produces the result envelope.
	CalleeTaskID string
	// PauseReason gets stamped on the execution state so the
	// pause sequence (existing WAITING_FOR_CHILDREN code path
	// in handleSyncJoin) treats this exactly like an in-
	// project delegation pause.
	PauseReason string
}

// errCrossProjectDisabled is returned by handleCallProjectStep
// when the feature flag is off OR the CPC repo isn't wired.
// The dispatch site routes it through the workflow's on_fail
// branch so deployments can keep an unfinished YAML in place
// without breaking the rest of the project.
var errCrossProjectDisabled = errors.New("call_project: inter-project orchestration disabled (set VORNIK_INTER_PROJECT_ENABLED=true and wire CrossProjectCallRepository)")

// interProjectEnabled reads the env flag. Centralised so tests
// can flip it via t.Setenv without each handler call site
// repeating the parse.
func interProjectEnabled() bool {
	v := os.Getenv(interProjectEnabledEnv)
	return v == "1" || v == "true" || v == "TRUE" || v == "yes"
}

// handleCallProjectStep is the executor handler for the
// `call_project` step type. See:
//   - https://docs.vornik.io §6.1
//   - workflow.go dispatch site (case "call_project")
//
// Lifecycle (LLD §3.2 worked example):
//  1. Validate feature flag + repo wired.
//  2. Resolve callee project via the WorkflowResolver. Missing
//     project → step fails with PROJECT_NOT_FOUND, on_fail fires.
//  3. Check acceptCallsFrom. Denial → step fails with
//     CROSS_PROJECT_REJECTED.
//  4. Create the CPC row (status=pending).
//  5. Create the callee task (status=QUEUED) carrying the CPC id
//     so the resolve hook can map back.
//  6. Stamp callee_task_id on the CPC row.
//
// The caller transitions to WAITING_FOR_CHILDREN at the dispatch
// site (so the pause sequence stays uniform with in-project
// delegation; we don't duplicate the checkpoint write here).
//
// Errors returned:
//   - errCrossProjectDisabled (feature flag off / repo unwired)
//   - "PROJECT_NOT_FOUND: <id>" (callee project not registered)
//   - "CROSS_PROJECT_REJECTED: <reason>" (acceptCallsFrom denial)
//   - any DB error from the repo writes (passed through)
func (e *Executor) handleCallProjectStep(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	currentStepID string,
	step *registry.WorkflowStep,
	stepResults map[string]json.RawMessage,
) (callProjectResult, error) {
	if !interProjectEnabled() || e.cpcRepo == nil {
		return callProjectResult{}, errCrossProjectDisabled
	}

	// Resolve callee project. The workflows resolver knows
	// about every loaded project; an unknown ID is a workflow-
	// authoring bug we surface immediately.
	calleeProj := e.workflows.GetProject(step.TargetProject)
	if calleeProj == nil {
		return callProjectResult{}, fmt.Errorf("PROJECT_NOT_FOUND: callee project %q is not registered", step.TargetProject)
	}

	// Caller-side outbound allowlist (canCallProjects). The complement
	// to acceptCallsFrom: an operator can lock down which projects THIS
	// caller may call, so a hallucinating planner can't fan calls out to
	// arbitrary projects that happen to accept from a wildcard. Empty =
	// allow-all (back-compatible), so unrestricted callers are unaffected.
	if callerProj := e.workflows.GetProject(task.ProjectID); callerProj != nil && !callerProj.CanCall(step.TargetProject) {
		return callProjectResult{}, fmt.Errorf(
			"CROSS_PROJECT_REJECTED: caller project %q is not allowed to call %q (configure canCallProjects)",
			task.ProjectID, step.TargetProject,
		)
	}

	// acceptCallsFrom enforcement. The check runs against the
	// caller's project ID (sourced from the parent task row);
	// a closed allowlist refuses everything by default.
	if !calleeProj.AcceptsCallsFrom(task.ProjectID) {
		return callProjectResult{}, fmt.Errorf(
			"CROSS_PROJECT_REJECTED: callee project %q does not accept calls from %q (configure acceptCallsFrom)",
			step.TargetProject, task.ProjectID,
		)
	}

	// Depth + cycle guards (LLD §"Later — Inter-project cycle
	// detection + depth limit"). Walks the parent_task_id chain
	// once and applies both invariants against the result:
	//
	//   - depth+1 must not exceed the originating project's
	//     EffectiveMaxCallDepth (default 8). Defends against a
	//     hallucinating LLM fanning out N levels deep.
	//   - The proposed callee must not already be an ancestor.
	//     Defends against trust-each-other pairs that
	//     mutual-allow but would otherwise loop until timeout.
	//
	// Both refusals write an audit row before returning so
	// operators can spot recurring patterns. taskRepo is required;
	// a nil-repo deployment would have failed earlier (callee
	// task create needs it anyway), but the nil-check keeps the
	// lineage walker honest.
	originatingProj := e.workflows.GetProject(task.ProjectID)
	maxDepth := originatingProj.EffectiveMaxCallDepth()
	var lineage lineageInfo
	if e.taskRepo != nil {
		var lineageErr error
		lineage, lineageErr = walkCallerLineage(ctx, e.taskRepo, task)
		if lineageErr != nil {
			// Walking failed mid-chain. Be defensive — refuse
			// the call rather than risk an unbounded chain.
			e.logger.Warn().Err(lineageErr).Str("caller_task_id", task.ID).
				Msg("call_project: lineage walk failed; refusing call as defensive measure")
			return callProjectResult{}, fmt.Errorf(
				"LINEAGE_WALK_FAILED: could not verify call depth + cycle invariants: %v",
				lineageErr,
			)
		}
	} else {
		lineage = lineageInfo{
			AncestorProjects: map[string]struct{}{task.ProjectID: {}},
			LineagePath:      []string{task.ProjectID},
		}
	}
	// Chain-depth header backstop. walkCallerLineage derives depth
	// from the stored parent_task_id chain, but that chain can be
	// truncated (lineageWalkHardLimit) or broken (an ancestor row
	// deleted), under-counting depth and weakening the cap. The
	// caller carries its own post-hop depth on context.callDepth
	// (stamped by buildCalleePayload); take the max so a broken
	// lineage can't reset the meter. readCarriedCallDepth is
	// conservative — a missing/corrupt header resolves to 0 and
	// never lowers the walked depth.
	callerDepth := lineage.Depth
	if carried := readCarriedCallDepth(task); carried > callerDepth {
		if lineage.Truncated || lineage.Depth == 0 {
			e.logger.Debug().
				Str("caller_task_id", task.ID).
				Int("walked_depth", lineage.Depth).
				Int("carried_depth", carried).
				Bool("lineage_truncated", lineage.Truncated).
				Msg("call_project: carried call-depth header exceeds walked lineage depth; using carried value (lineage incomplete)")
		}
		callerDepth = carried
	}
	proposedDepth := callerDepth + 1
	if proposedDepth > maxDepth {
		e.recordCPCRefusal(ctx, task, currentStepID, step.TargetProject,
			auditActionCPCDepthExceeded,
			fmt.Sprintf("would push chain to depth %d; cap=%d", proposedDepth, maxDepth),
			proposedDepth, lineage.LineagePath)
		return callProjectResult{}, fmt.Errorf(
			"DEPTH_EXCEEDED: call_project would push chain to depth %d (caller=%s, cap=%d for project %q) — set maxCallDepth on project %q to allow deeper chains",
			proposedDepth, task.ProjectID, maxDepth, task.ProjectID, task.ProjectID,
		)
	}
	if _, hit := lineage.AncestorProjects[step.TargetProject]; hit {
		e.recordCPCRefusal(ctx, task, currentStepID, step.TargetProject,
			auditActionCPCCycleDetected,
			fmt.Sprintf("callee %q is an ancestor in the lineage chain", step.TargetProject),
			proposedDepth, lineage.LineagePath)
		return callProjectResult{}, fmt.Errorf(
			"CYCLE_DETECTED: callee project %q is already an ancestor of caller task %s (lineage=%v) — break the cycle in the workflow design or split the recursive step into a non-recursive one",
			step.TargetProject, task.ID, lineage.LineagePath,
		)
	}

	// Build the call payload. Phase D adds ${outputs.<step>.
	// <field>} interpolation against the executor's per-step
	// result mirror (state.StepResults). An absent reference
	// resolves to empty string — the workflow author sees the
	// broken value in the callee's payload and can fix the
	// reference. Phase A/B passed the payload verbatim; the
	// interpolator is a no-op for payloads with no references.
	resolvedPayload := interpolateOutputs(step.Payload, stepResults)
	payloadBytes, err := json.Marshal(resolvedPayload)
	if err != nil {
		return callProjectResult{}, fmt.Errorf("call_project: marshal payload: %w", err)
	}

	// Create the CPC row first. If the next step (task create)
	// fails, we'd be left with an orphan pending CPC — Phase A
	// tolerates this (the timeout scanner sweeps it out
	// eventually); Phase D will wrap both in a single tx via
	// the persistence layer.
	cpc := &persistence.CrossProjectCall{
		CallerTaskID:    task.ID,
		CallerStepID:    currentStepID,
		CallerProject:   task.ProjectID,
		CalleeProject:   step.TargetProject,
		CalleeWorkflow:  step.TargetWorkflow,
		Payload:         payloadBytes,
		ExpectedSchema:  step.Expect.Schema,
		Status:          persistence.CPCStatusPending,
		CancelOnTimeout: step.CancelOnTimeout,
	}
	if step.Timeout != "" {
		if d, perr := time.ParseDuration(step.Timeout); perr == nil && d > 0 {
			at := time.Now().Add(d)
			cpc.TimeoutAt = &at
		}
	}
	if err := e.cpcRepo.Create(ctx, cpc); err != nil {
		return callProjectResult{}, fmt.Errorf("call_project: create CPC row: %w", err)
	}

	// Create the callee task. Wire ParentTaskID to the caller
	// so the existing in-project delegation resume primitives
	// (checkParentUnblock) drive the wake — no new scheduler
	// code path. CrossProjectCallID closes the loop for the
	// resolve hook on the callee's terminal status.
	calleeWorkflowID := step.TargetWorkflow
	calleeTaskID := persistence.GenerateID("task")
	cpcID := cpc.ID
	calleePayload := buildCalleePayload(payloadBytes, proposedDepth)
	now := time.Now()
	callee := &persistence.Task{
		ID:                 calleeTaskID,
		ProjectID:          step.TargetProject,
		WorkflowID:         &calleeWorkflowID,
		ParentTaskID:       &task.ID,
		CreationSource:     persistence.TaskCreationSourceDelegation,
		Status:             persistence.TaskStatusQueued,
		Priority:           50,
		Payload:            calleePayload,
		Attempt:            1,
		MaxAttempts:        3,
		CreatedAt:          now,
		UpdatedAt:          now,
		CrossProjectCallID: &cpcID,
	}
	if err := e.taskRepo.Create(ctx, callee); err != nil {
		// Mark the CPC rejected so the caller's on_fail
		// branch surfaces a clean reason instead of a stale
		// pending row.
		_ = e.cpcRepo.MarkRejected(ctx, cpc.ID, "callee task creation failed: "+err.Error())
		return callProjectResult{}, fmt.Errorf("call_project: create callee task: %w", err)
	}

	// Stamp the callee_task_id on the CPC so resolve-hook
	// lookups via GetByCalleeTaskID succeed.
	if err := e.cpcRepo.SetCalleeTaskID(ctx, cpc.ID, calleeTaskID); err != nil {
		e.logger.Warn().Err(err).Str("cpc_id", cpc.ID).
			Str("callee_task_id", calleeTaskID).
			Msg("call_project: failed to stamp callee_task_id (resolve hook may need GetByCalleeTaskID retry)")
		// Don't fail the step — the row exists, the callee
		// task exists, the worst case is the resolve hook
		// does an extra lookup. Continue.
	}

	e.logger.Info().
		Str("caller_task_id", task.ID).
		Str("caller_project", task.ProjectID).
		Str("callee_project", step.TargetProject).
		Str("callee_workflow", step.TargetWorkflow).
		Str("cpc_id", cpc.ID).
		Str("callee_task_id", calleeTaskID).
		Msg("call_project: dispatched cross-project call")

	// Phase C observability: emit live event + bump metrics +
	// write audit row. All best-effort and nil-safe.
	e.emitLive(ctx, execution.ID, livepubsub.KindCrossProjectCallStarted, livepubsub.CrossProjectCallStartedPayload{
		CPCId:          cpc.ID,
		CalleeProject:  step.TargetProject,
		CalleeWorkflow: step.TargetWorkflow,
		CalleeTaskID:   calleeTaskID,
		ExpectedSchema: step.Expect.Schema,
		StepID:         currentStepID,
	})
	if e.metrics != nil {
		e.metrics.RecordCrossProjectCallStarted(task.ProjectID, step.TargetProject)
	}
	e.recordCPCAuditCreate(ctx, task, currentStepID, cpc, calleeTaskID)

	return callProjectResult{
		CPCId:        cpc.ID,
		CalleeTaskID: calleeTaskID,
		PauseReason:  PauseReasonAwaitingChildren,
	}, nil
}

// resolveCrossProjectCallForTask is the executor's terminal-
// status hook for callee tasks. When a task carrying
// CrossProjectCallID reaches a terminal state, this method
// looks up the matching CPC row and:
//   - validates the task's result envelope against the CPC's
//     expected_schema (Phase A v1 = JSON shape only — full
//     JSON-Schema validation lands in Phase B)
//   - marks the CPC completed / failed / rejected
//   - writes the validated envelope back to the CPC row
//
// The wake of the caller task is handled by the existing
// checkParentUnblock — the callee's ParentTaskID is the
// caller's task ID, so the in-project delegation primitive
// drives the resume.
//
// Best-effort: every error is logged but doesn't propagate. A
// missed resolve leaves the CPC in `running` until the
// timeout scanner sweeps it (Phase B work). Same fail-soft
// pattern as cascadeOrphanExecutions.
func (e *Executor) resolveCrossProjectCallForTask(
	ctx context.Context,
	task *persistence.Task,
	succeeded bool,
) {
	if e == nil || e.cpcRepo == nil || task == nil || task.CrossProjectCallID == nil {
		return
	}
	cpcID := *task.CrossProjectCallID

	// Load the CPC row up front — we need caller_task_id +
	// caller_project for the live event + metrics labels.
	cpc, _ := e.cpcRepo.Get(ctx, cpcID)
	finishCPC := func(status, errMsg string, succeed bool) {
		// Emit + metrics + audit on the caller's stream after
		// the repo write succeeds, so a partial failure doesn't
		// surface "resolved" in observability while the CPC row
		// stays open.
		if cpc == nil {
			return
		}
		dur := time.Duration(0)
		if !cpc.CreatedAt.IsZero() {
			dur = time.Since(cpc.CreatedAt)
		}
		if e.metrics != nil {
			e.metrics.RecordCrossProjectCallResolved(cpc.CallerProject, cpc.CalleeProject, status, dur.Seconds())
		}
		// The live event fires on the CALLER's execution stream
		// — we need the caller's execution id. Best-effort
		// resolution via the execution repo.
		if callerExecID := e.lookupExecutionIDForTask(ctx, cpc.CallerTaskID); callerExecID != "" {
			summary := ""
			if succeed {
				summary = "envelope accepted"
			}
			e.emitLive(ctx, callerExecID, livepubsub.KindCrossProjectCallResolved, livepubsub.CrossProjectCallResolvedPayload{
				CPCId:        cpcID,
				Status:       status,
				Summary:      summary,
				ErrorMessage: errMsg,
				DurationMs:   dur.Milliseconds(),
			})
		}
		e.recordCPCAuditResolve(ctx, cpc, status, errMsg)
	}

	if !succeeded {
		// Callee task ended FAILED or CANCELLED. Resolve as
		// failed; the caller's on_fail branch fires.
		reason := "callee task terminated without success"
		if task.LastError != nil && *task.LastError != "" {
			reason = "callee task failed: " + *task.LastError
		}
		if err := e.cpcRepo.MarkFailed(ctx, cpcID, reason); err != nil {
			e.logger.Warn().Err(err).Str("cpc_id", cpcID).
				Str("callee_task_id", task.ID).
				Msg("cross-project resolve: MarkFailed failed; caller may stay blocked until timeout scanner sweeps")
			return
		}
		finishCPC(string(persistence.CPCStatusFailed), reason, false)
		return
	}

	// Happy path. Pull the envelope from the callee task's
	// result_envelope column (preferred) or fall back to the
	// last execution's result if the column is empty.
	envelope := task.ResultEnvelope
	if len(envelope) == 0 && e.execRepo != nil {
		if exec, err := e.execRepo.GetByTaskID(ctx, task.ID); err == nil && exec != nil {
			envelope = exec.Result
		}
	}

	// Validate envelope shape. Phase A v1 only checks the
	// envelope is non-empty + parses as a JSON object — full
	// JSON-Schema validation (against schema_registry) lands
	// in Phase B. An empty envelope resolves the call as
	// rejected so the caller can take the on_fail branch
	// rather than wait for a timeout.
	if len(envelope) == 0 {
		reason := "callee task produced no result envelope"
		if err := e.cpcRepo.MarkRejected(ctx, cpcID, reason); err != nil {
			e.logger.Warn().Err(err).Str("cpc_id", cpcID).
				Msg("cross-project resolve: MarkRejected (empty envelope) failed")
			return
		}
		finishCPC(string(persistence.CPCStatusRejected), reason, false)
		return
	}
	var probe map[string]any
	if err := json.Unmarshal(envelope, &probe); err != nil {
		reason := "envelope is not a JSON object: " + err.Error()
		if perr := e.cpcRepo.MarkRejected(ctx, cpcID, reason); perr != nil {
			e.logger.Warn().Err(perr).Str("cpc_id", cpcID).
				Msg("cross-project resolve: MarkRejected (parse error) failed")
			return
		}
		finishCPC(string(persistence.CPCStatusRejected), reason, false)
		return
	}

	// Phase D — structural envelope validation. Full JSON-
	// Schema validation against schema_registry rows is a
	// follow-on (needs a real validator dep); v1 of validation
	// checks the LLD-documented envelope shape: `schema` field
	// must equal the expected_schema id, and `status` must be
	// present. Catches the most common failure mode (callee
	// returned the WRONG envelope type) without a new dep.
	if cpc != nil && cpc.ExpectedSchema != "" {
		if reason := validateEnvelopeShape(probe, cpc.ExpectedSchema); reason != "" {
			if perr := e.cpcRepo.MarkRejected(ctx, cpcID, reason); perr != nil {
				e.logger.Warn().Err(perr).Str("cpc_id", cpcID).
					Msg("cross-project resolve: MarkRejected (envelope shape) failed")
				return
			}
			finishCPC(string(persistence.CPCStatusRejected), reason, false)
			return
		}
		// Tier 2 — JSON-Schema validation against the
		// registered schema body. HasSchema differentiates "no
		// schema registered" (pass through, shape-only is
		// enough) from "validation failed" (reject). Lets
		// deployments adopt schemas incrementally without
		// breaking pre-existing CPC flows.
		if e.schemaRegistry != nil && e.schemaRegistry.HasSchema(cpc.ExpectedSchema) {
			if err := e.schemaRegistry.Validate(cpc.ExpectedSchema, probe); err != nil {
				reason := "envelope failed JSON-Schema validation: " + err.Error()
				if perr := e.cpcRepo.MarkRejected(ctx, cpcID, reason); perr != nil {
					e.logger.Warn().Err(perr).Str("cpc_id", cpcID).
						Msg("cross-project resolve: MarkRejected (schema validation) failed")
					return
				}
				finishCPC(string(persistence.CPCStatusRejected), reason, false)
				return
			}
		}
	}

	if err := e.cpcRepo.MarkCompleted(ctx, cpcID, envelope); err != nil {
		e.logger.Warn().Err(err).Str("cpc_id", cpcID).
			Str("callee_task_id", task.ID).
			Msg("cross-project resolve: MarkCompleted failed; caller may stay blocked")
		return
	}
	e.logger.Info().
		Str("cpc_id", cpcID).
		Str("callee_task_id", task.ID).
		Msg("cross-project resolve: completed; envelope persisted to CPC row")
	finishCPC(string(persistence.CPCStatusCompleted), "", true)
}

// lookupExecutionIDForTask returns the execution ID for the
// given task (latest non-terminal preferred, falls back to
// the most-recent execution overall). Returns "" on any
// repo error or when the task has no execution yet; callers
// nil-check the result before emitting a live event.
//
// Used by the resolve hook to emit cross_project_call_resolved
// on the CALLER's stream — the CPC row carries only the caller
// task ID, not the execution ID directly (the caller execution
// pauses before it knows the callee's terminal status).
func (e *Executor) lookupExecutionIDForTask(ctx context.Context, taskID string) string {
	if e == nil || e.execRepo == nil || taskID == "" {
		return ""
	}
	exec, err := e.execRepo.GetByTaskID(ctx, taskID)
	if err != nil || exec == nil {
		return ""
	}
	return exec.ID
}

// validateEnvelopeShape performs Phase D's lightweight
// structural validation of the result envelope. Returns ""
// when the shape is acceptable; otherwise a human-readable
// reason the caller routes into MarkRejected.
//
// LLD §4.2 envelope contract:
//
//	{
//	  "schema": "<id>",     // MUST equal expected_schema
//	  "status": "<string>", // MUST be present
//	  "summary": "...",     // optional
//	  "data": {...},        // optional, schema-specific
//	  "artifacts": [...],   // optional
//	  "errors": [...]       // optional
//	}
//
// Full JSON-Schema validation of `data` against a registered
// schema body is deferred to a Phase D follow-on (needs a real
// validator dependency); for now we pin the envelope-shape
// contract so a callee that returns the wrong envelope type
// gets rejected loudly instead of silently feeding garbage to
// the caller's next step.
func validateEnvelopeShape(envelope map[string]any, expectedSchema string) string {
	if envelope == nil {
		return "envelope is not a JSON object"
	}
	schemaVal, ok := envelope["schema"]
	if !ok {
		return "envelope missing required field: schema"
	}
	schemaStr, ok := schemaVal.(string)
	if !ok {
		return "envelope.schema must be a string"
	}
	if schemaStr != expectedSchema {
		return "envelope.schema = " + schemaStr + ", caller expected " + expectedSchema
	}
	if _, ok := envelope["status"]; !ok {
		return "envelope missing required field: status"
	}
	return ""
}

// emitCrossProjectCallReceivedIfCallee fires the Phase D
// inbound-edge event on the callee execution's stream when
// the executing task is a CPC callee. Resolves the
// CPC row via task.CrossProjectCallID, looks up the
// caller-side metadata (caller_project + step + schema), and
// publishes to the callee's livePub stream.
//
// Per-process dedup via callReceivedDedup so a retry or
// scheduler recovery doesn't re-emit; daemon restart is
// allowed to re-emit once (event is informational, not a
// correctness dependency).
func (e *Executor) emitCrossProjectCallReceivedIfCallee(ctx context.Context, task *persistence.Task, exec *persistence.Execution) {
	if e == nil || task == nil || exec == nil {
		return
	}
	if task.CrossProjectCallID == nil || e.cpcRepo == nil || e.livePub == nil {
		return
	}
	if e.callReceivedDedup != nil && !e.callReceivedDedup.markEmitted(exec.ID) {
		return
	}
	cpc, err := e.cpcRepo.Get(ctx, *task.CrossProjectCallID)
	if err != nil || cpc == nil {
		return
	}
	e.emitLive(ctx, exec.ID, livepubsub.KindCrossProjectCallReceived, livepubsub.CrossProjectCallReceivedPayload{
		CPCId:          cpc.ID,
		CallerProject:  cpc.CallerProject,
		CallerTaskID:   cpc.CallerTaskID,
		CallerStepID:   cpc.CallerStepID,
		ExpectedSchema: cpc.ExpectedSchema,
	})
}

// buildCalleePayload constructs the task.Payload bytes for the
// callee task. The v1 wire shape is the standard task payload
// envelope:
//
//	{"context": {"prompt": "<from caller>", "callDepth": N},
//	 "args":    {"cross_project_payload": <raw>}}
//
// callDepth is the post-hop chain depth (caller depth + 1) so the
// callee's own call_project handler (and any audit row downstream)
// can read it without re-walking the lineage. The depth + cycle
// guards walk the lineage authoritatively at every hop so the
// payload value is informational only — operator inspection,
// debugging, and the in-progress dashboard tile read it.
//
// Phase B will introduce a richer schema with explicit
// inputs.<name> mapping; for v1 the callee workflow reads the
// raw payload from args.cross_project_payload.
func buildCalleePayload(rawPayload []byte, callDepth int) []byte {
	envelope := map[string]any{
		"context": map[string]any{
			"prompt":    "cross-project task — see args.cross_project_payload",
			"callDepth": callDepth,
		},
		"args": map[string]any{
			"cross_project_payload": json.RawMessage(rawPayload),
		},
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		// Should never happen — the input is already valid
		// JSON. Fall back to the raw payload so the callee
		// at least sees the inputs.
		return rawPayload
	}
	return body
}
