package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"vornik.io/vornik/internal/counterfactual"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/stepoutcome"
)

// errExecutionPaused is returned when an execution is paused (e.g. awaiting approval
// or waiting for delegated child tasks to complete).
var errExecutionPaused = errors.New("execution paused awaiting approval")

// executeWorkflowAttempt runs through the workflow steps starting from the
// current position until a terminal is reached or the execution is paused.
func (e *Executor) executeWorkflowAttempt(ctx context.Context, task *persistence.Task, execution *persistence.Execution, plan *executionPlan, timeout time.Duration) (string, []byte, []string, error) {
	state := loadExecutionState(execution)
	currentStepID := plan.workflow.Entrypoint
	if execution.CurrentStepID != nil && *execution.CurrentStepID != "" {
		currentStepID = *execution.CurrentStepID
	}

	completedSteps := append([]string{}, execution.CompletedSteps...)

	// Workflow-start artifact cleanup: when the workflow YAML lists
	// cleanup_artifacts, wipe those files from the workspace BEFORE
	// the entrypoint step runs. Defense-in-depth against a producer
	// agent (e.g. researcher) failing to overwrite a canonical file
	// (artifacts/out/research.md) — without this, a downstream step
	// (writer) silently consumes the prior task's content. Only
	// fires at the actual start of execution: a resumed/paused
	// execution that re-enters somewhere mid-workflow keeps its
	// already-written files. Errors NEVER fail the workflow.
	if currentStepID == plan.workflow.Entrypoint && len(completedSteps) == 0 {
		workspaceDir := workflowEffectiveWorkspaceDir(e, plan, task)
		_ = applyWorkflowArtifactCleanup(workspaceDir, plan.workflow, e.logger)
		// One-line announce of the pedantic flag at the start of
		// every fresh execution so operators can confirm at a
		// glance that strict-instruction-following took effect
		// (trading + compliance contexts; see swarm-recovery LLD §6).
		if resolvePedantic(task.Payload, plan.workflow, plan.project) {
			e.logger.Info().
				Str("task_id", task.ID).
				Str("execution_id", execution.ID).
				Str("workflow", plan.workflow.ID).
				Msg("pedantic mode active — recovery routing disabled, on_fail goes straight to terminal")
		}
	}
	var lastContainerID string
	var lastResult []byte
	// stepArtifacts carries output artifacts from the previous agent step
	// so they can be staged as inputs for the next step.
	var stepArtifacts []map[string]string
	var lastResultMessage string
	// lastResultErr carries the original error that produced lastResultMessage
	// so resolveTerminalOutcome can wrap it with %w and preserve the error
	// chain (e.g. context.Canceled) for ClassifyExecutionFailure. Only set
	// when a step fails; nil on success or when the error came from a string
	// source that has no underlying error value (Fix-3, 2026-06-21).
	var lastResultErr error

	// Fork-from-step prompt override (Feature #1 Phase B). Applied
	// at most once per execution — on the first time we hit the
	// forked step. Subsequent retries of the same step run with the
	// workflow's original prompt; the override is operator
	// guidance that's already "in the conversation" once consumed.
	forkOverrideApplied := false

	// Task-level input artifacts from task.Payload.context.inputFiles
	// (user uploads from Telegram, API attachments). Persisted across
	// every step of this workflow: each step's container starts from a
	// fresh tempRoot, so re-staging is mandatory for multi-step
	// workflows where a downstream step (e.g. `write` after
	// `research`) needs access to the original source material the
	// upstream step's output summary alone can't carry.
	//
	// Pre-2026-05-16 this was a one-shot prepend on stepArtifacts —
	// the writer step in a research workflow on an adaptive parent
	// silently lost the user's uploaded PDF because the executor
	// replaced stepArtifacts with the researcher's output artifacts
	// after step 1. Operator-reported on a CV-extraction task:
	// "writer claimed it preserved the CV but the file never
	// reached its container".
	taskInputArtifacts := extractTaskInputArtifacts(task.Payload)
	stepArtifacts = append(stepArtifacts, taskInputArtifacts...)

	// Loop protection: track how many times each step is visited.
	maxVisits := plan.workflow.MaxStepVisits
	if maxVisits <= 0 {
		maxVisits = 3
	}
	// Counterfactual budget override (VariableBudget) tightens
	// the cap when the operator wants to probe a smaller iter
	// budget. Override only LOWERS — a higher value than the
	// workflow's own cap is ignored so the replay doesn't gain
	// more headroom than the original had.
	cfBudget := counterfactual.ExtractPayload(task.Payload).Budget
	if cfBudget.MaxItersPerStep > 0 && cfBudget.MaxItersPerStep < maxVisits {
		maxVisits = cfBudget.MaxItersPerStep
	}
	maxIterations := plan.workflow.MaxIterations
	if maxIterations <= 0 {
		maxIterations = 20
	}
	visitCount := make(map[string]int)
	if state.VisitCounts != nil {
		for k, v := range state.VisitCounts {
			visitCount[k] = v
		}
	}
	iterations := state.Iterations

	// Publish the step we're about to run BEFORE running it, so the
	// execution detail page renders a live "running" Step Progress block
	// for the very first step. The in-loop checkpoints below only ever
	// persist the NEXT step's id (after a step completes), so without this
	// the entrypoint step has no persisted current_step_id for the whole
	// duration of its run and /ui/executions/<id> shows a blank panel
	// until step 1 finishes. Skip for terminal entrypoints (nothing runs)
	// and treat the write as best-effort — losing UI metadata must not
	// abort the execution.
	if _, isTerminal := plan.workflow.Terminals[currentStepID]; !isTerminal {
		if err := e.saveCheckpoint(ctx, execution, currentStepID, completedSteps, state); err != nil {
			e.logger.Warn().
				Str("execution_id", execution.ID).
				Str("step_id", currentStepID).
				Err(err).
				Msg("failed to publish initial current_step_id — detail page may show a blank first step")
		}
	}

	for {
		if terminal, ok := plan.workflow.Terminals[currentStepID]; ok {
			cid, res, termErr := e.resolveTerminalOutcome(ctx, task, execution, terminal,
				currentStepID, completedSteps, lastContainerID, lastResult, lastResultMessage, lastResultErr)
			return cid, res, completedSteps, termErr
		}

		step, ok := plan.workflow.Steps[currentStepID]
		if !ok {
			return "", nil, completedSteps, fmt.Errorf("workflow step %s not found", currentStepID)
		}

		// Per-step dispatch trace (2026-06-13): make the workflow loop's step
		// sequence visible in the journal so a "completed without running the
		// publish step" regression is diagnosable directly, rather than inferred
		// from the absence of a handler log. Cheap (one line per transition).
		e.logger.Info().
			Str("task_id", task.ID).
			Str("execution_id", execution.ID).
			Str("step", currentStepID).
			Str("type", step.Type).
			Str("handler", step.Handler).
			Int("iteration", iterations+1).
			Msg("workflow: dispatching step")

		// Global iteration counter: fail if total step transitions exceed the limit.
		iterations++
		state.Iterations = iterations
		if iterations > maxIterations {
			return "", nil, completedSteps, fmt.Errorf("workflow exceeded %d total iterations — terminating", maxIterations)
		}

		// Per-step loop protection: fail if a step is visited too many times.
		visitCount[currentStepID]++
		state.VisitCounts = visitCount
		if visitCount[currentStepID] > maxVisits {
			return "", nil, completedSteps, fmt.Errorf("step %s visited %d times (max %d) — likely infinite rework loop", currentStepID, visitCount[currentStepID], maxVisits)
		}

		// Resolve per-step timeout from workflow YAML. Fall back to the
		// execution-level timeout (from executor config) if not specified.
		stepTimeout := timeout
		if step.Timeout != "" {
			if d, err := time.ParseDuration(step.Timeout); err == nil && d > 0 {
				stepTimeout = d
			}
		}
		// Dynamic tool budget: scale the step's time budget by the same
		// complexity factor as its iteration budget so the two move
		// together (raise iterations → raise wall-clock; downscale a
		// small task → less of both). Applied BEFORE the counterfactual
		// cap so the "override only LOWERS" invariant holds against the
		// scaled native. Ephemeral roles only — see applyStepTimeoutBudget.
		// See https://docs.vornik.io §6/§7.
		if e.config.ToolBudget.Enabled {
			roleConfig, _ := findSwarmRole(plan.swarm, step.Role)
			autonomous := task.CreationSource != persistence.TaskCreationSourceUser
			stepTimeout = applyStepTimeoutBudget(stepTimeout, roleConfig, state.ComplexityTier, autonomous, e.config.ToolBudget)
		}

		// Counterfactual budget can tighten the timeout. Same
		// "override only LOWERS" rule as max_iters_per_step.
		if cfBudget.StepTimeoutSeconds > 0 {
			cap := time.Duration(cfBudget.StepTimeoutSeconds) * time.Second
			if cap < stepTimeout {
				stepTimeout = cap
			}
		}

		switch step.Type {
		case "agent":
			// Feature #3 live observation — emit step_started at
			// the top of each agent-step iteration. Nil-safe when
			// the publisher isn't wired.
			stepStartedAt := time.Now()
			// step.Model isn't on the workflow definition (roles
			// carry models in the swarm config); leave empty so
			// the consumer pulls from per-step usage rows.
			e.emitStepStarted(ctx, execution.ID, currentStepID, step.Role, "", 1)
			// Assemble the agent input (operator hints, prompt
			// assembly, strategist prewarm, adaptive candidates,
			// recovery forwarding, complexity tier, role resolution).
			// Extracted as Phase 1 of the agent-case decomposition
			// (Track B); behaviour is identical. forkOverrideApplied is
			// threaded by pointer because the helper flips it on the
			// first visit to a forked step.
			opts, roleConfig := e.prepareAgentStepInput(
				ctx, task, execution, plan, currentStepID, step,
				stepArtifacts, lastResultMessage, &state, &forkOverrideApplied,
			)
			// Resume guard (Track-B Phase 2): detect a re-entry after this
			// step's delegated children already ran. routeAlreadyHandled
			// means skip the LLM + spawn and synthesise the children's
			// outcomes; resumeChildFailed routes to on_fail instead of
			// publishing partial work.
			routeAlreadyHandled, resumeChildFailed, resumeChildren := e.resolveResumeGuard(ctx, task, plan, currentStepID)

			// Publish-on-success guard: a delegating parent (e.g. github-router)
			// resuming after a FAILED child must NOT advance to its OnSuccess
			// (publish) step — that would open a change request from rejected or
			// partial work. Route to OnFail so the parent ends cleanly with no PR.
			// (checkParentUnblock re-queues the parent when retry budget remains,
			// so without this the retry would resume straight to publish.)
			if routeAlreadyHandled && resumeChildFailed {
				e.logger.Warn().
					Str("task_id", task.ID).
					Str("execution_id", execution.ID).
					Str("step", currentStepID).
					Str("on_fail", step.OnFail).
					Msg("resume: delegated child failed — routing to on_fail, NOT publishing")
				completedSteps = append(completedSteps, currentStepID)
				if step.OnFail != "" {
					currentStepID = step.OnFail
					continue
				}
				return "", nil, completedSteps, fmt.Errorf("delegated child failed and step %q has no on_fail target", currentStepID)
			}

			// Single-candidate auto-route: when the project's adaptive
			// candidate list has exactly one entry, the lead has nothing
			// to pick — synthesize the routing result directly and skip
			// the LLM call. Saves tokens, and (more importantly) closes
			// off the failure mode where flash-tier models invent a
			// "config missing" refusal instead of emitting the only
			// valid pick. Observed live 2026-05-18 with janka project
			// (candidates: [research], lead model: zai.glm-4.7-flash).
			// Dispatch the agent step (Track-B Phase 3): synthesise the
			// children's outcomes on a resume, auto-route a single
			// candidate without an LLM call, or run the agent with the
			// model-fallback retry layer. Behaviour is identical to the
			// inline three-way branch it replaces.
			containerID, resultBytes, err := e.dispatchAgentStep(
				ctx, task, execution, plan, currentStepID, step, stepTimeout,
				opts, roleConfig, routeAlreadyHandled, resumeChildren,
			)
			if err != nil {
				// Terminal verifier failures short-circuit on_fail
				// routing — see container.go's TerminalVerifierError
				// docstring. The canonical case is rate-limit
				// detection: looping back through "route" three times
				// just hammers an already-blocking portal. Surface
				// the error directly so the operator (and post-
				// mortem) see "the portal blocked us" instead of
				// "the route step failed three times".
				if isTerminalVerifierError(err) {
					e.logger.Warn().
						Str("execution_id", execution.ID).
						Str("step", currentStepID).
						Str("role", step.Role).
						Str("error", truncateStr(err.Error(), 500)).
						Msg("agent step failed with terminal verifier — skipping on_fail retry")
					return "", nil, completedSteps, err
				}

				e.logger.Warn().
					Str("execution_id", execution.ID).
					Str("step", currentStepID).
					Str("role", step.Role).
					Str("on_fail", step.OnFail).
					Str("error", truncateStr(err.Error(), 500)).
					Msg("agent step failed")

				// Shutdown guard (Fix-2): if the daemon is shutting down or
				// the context has been cancelled, do NOT route to on_fail.
				// Routing on_fail during teardown walks the failure graph
				// while the runtime is being torn down — it starts more
				// containers against a closing socket, exhausts retry budget,
				// and overwrites the PAUSED status with FAILED (Signature B,
				// restart-induced in-flight FAILED, 2026-06-21). Return the
				// error upward; the existing isShuttingDown() bail-out arm in
				// runExecution (executor.go ~line 1975) handles it cleanly →
				// PAUSED-for-resume, NOT FAILED.
				if e.isShuttingDown() || ctx.Err() != nil {
					return "", nil, completedSteps, err
				}

				// If the step has an on_fail transition, route there
				// instead of failing the entire execution.
				if step.OnFail != "" {
					completedSteps = append(completedSteps, currentStepID)
					state.LastResult = []byte(fmt.Sprintf(`{"error":%q}`, err.Error()))
					// Capture structured failure signals so the recovery step
					// can propose alternatives. nil in pedantic mode (recovery
					// opted out) — leave any prior PendingRecovery untouched, so
					// on_fail drops straight through to the terminal failure
					// target with no checkpoint surfacing.
					if rc := buildStepFailureRecovery(err, currentStepID, task, plan); rc != nil {
						state.PendingRecovery = rc
					}
					if saveErr := e.saveCheckpoint(ctx, execution, step.OnFail, completedSteps, state); saveErr != nil {
						return "", nil, completedSteps, saveErr
					}
					currentStepID = step.OnFail
					lastResultMessage = err.Error()
					lastResultErr = err
					continue
				}
				return "", nil, completedSteps, err
			}
			lastContainerID = containerID
			lastResult = resultBytes
			completedSteps = append(completedSteps, currentStepID)
			// Dynamic tool budget (agent-step producer path): if this
			// step emitted a complexity verdict (the dev-pipeline analyst
			// emits analysis.complexity), record it on state so later
			// subtask workers scale their tool-iteration budget. The
			// lead-driven path sets this from LeadOutcome.Complexity in
			// runLeadPlanning instead. See
			// https://docs.vornik.io §5.2.
			if tier := extractComplexityFromResult(resultBytes); tier != "" {
				state.ComplexityTier = tier
			}
			// Feature #3 — step_completed event. Cost is sourced
			// from per-step LLM usage later in the loop; v1 emits
			// a 0 cost (the live view's running-total reconciles
			// from the per-step usage rows out-of-band).
			e.emitStepCompleted(ctx, execution.ID, currentStepID, "ok", stepStartedAt, 0)

			// Extract output artifacts, message, and delegated tasks from result.
			// stepArtifacts is rebuilt as (task inputs ⊕ this step's outputs)
			// so the next step's container sees BOTH the original user
			// uploads and the upstream step's products. The step's outputs
			// come from plan.stepOutputArtifacts — the STORE-backed handoff
			// (task e9a5): persistArtifacts harvested them into the durable
			// artifact store and recorded each {name, sourcePath} where
			// sourcePath is the store StoragePath (under an allowed staging
			// root). We tag each with class="output" so the next step stages
			// them into artifacts/out/ (where roles read upstream products),
			// not artifacts/in/. The raw result.OutputArtifacts forwarding is
			// REMOVED: those carried the agent's container-relative path with
			// no host sourcePath and were silently rejected by
			// resolveStagingSrc, so the file never reached the next step.
			// Task uploads (taskInputArtifacts) stay untagged → artifacts/in/.
			stepArtifacts = append([]map[string]string{}, taskInputArtifacts...)
			for _, out := range plan.stepOutputArtifacts {
				// Copy the map and tag it; do not mutate plan's slice entries.
				tagged := make(map[string]string, len(out)+1)
				for k, v := range out {
					tagged[k] = v
				}
				tagged["class"] = "output"
				stepArtifacts = append(stepArtifacts, tagged)
			}
			lastResultMessage = ""
			lastResultErr = nil
			if len(resultBytes) > 0 {
				var result agentStepResult
				if json.Unmarshal(resultBytes, &result) == nil {
					lastResultMessage = stripReasoning(result.Message)

					// Strict adaptive routing (Track-B Phase 4): when the lead
					// emits {"selected_workflow": "<id>"}, validate/repair the
					// pick against the project's candidate list and delegate the
					// real work to a child task, pausing the parent. fired=false
					// means this isn't a strict route with candidates and the
					// loop falls through to the normal post-step flow.
					if fired, pCID, pRes, pCompleted, pErr := e.handleSelectedWorkflowRoute(
						ctx, task, execution, plan, currentStepID, step, stepTimeout,
						opts, roleConfig, &result, routeAlreadyHandled, &state,
						completedSteps, lastContainerID, lastResult,
					); fired {
						if pErr != nil {
							return "", nil, pCompleted, pErr
						}
						return pCID, pRes, pCompleted, errExecutionPaused
					}

					// Delegation (Track-B Phase 4): if the agent requested
					// delegatedTasks, create the child tasks (topology from
					// delegationMode) and pause the parent until they complete.
					if fired, dErr := e.handleDelegatedTasks(
						ctx, task, execution, currentStepID, step, &result, &state, completedSteps,
					); fired {
						if dErr != nil {
							return "", nil, completedSteps, dErr
						}
						return lastContainerID, lastResult, completedSteps, errExecutionPaused
					}
				}
			}

			nextStepID := step.OnSuccess
			// If no on_success but gates are defined, evaluate gates on
			// the agent result. This enables rework patterns where a
			// reviewer agent routes work back to an earlier step.
			if nextStepID == "" && len(step.Gates) > 0 {
				var (
					gateErr   error
					gateTrace GateEvalTrace
				)
				nextStepID, gateTrace, gateErr = evaluateGateStepTraced(step, append(json.RawMessage(nil), resultBytes...))
				e.logGateTrace(ctx, task, execution, currentStepID, gateTrace, gateErr, nextStepID)
				if gateErr != nil {
					// Gate evaluation failed. The failure class depends on
					// the error shape: malformed JSON from the agent is a
					// parse_error attributed to the *producer* (this step's
					// output couldn't be read). "No matching condition" is
					// a semantic rejection — downstream_rejected. Gate spec
					// bugs are gate_failed. Classifying here keeps the
					// outcome row's blame aimed correctly.
					producerOutcome, producerClass := classifyGateEvalError(gateErr)
					e.finalizePendingOutcome(ctx, execution.ID, currentStepID, producerOutcome, producerClass, gateErr.Error(), nil)

					// Fall back to on_fail if available rather than crashing
					// the entire execution — LLMs produce non-conformant
					// JSON frequently enough that this is a major source
					// of random task failures.
					if step.OnFail != "" {
						// Shutdown guard: do NOT route to on_fail during
						// daemon teardown — same invariant as the agent-step
						// guard above (Fix-2, 2026-06-21). Return the error
						// upward so runExecution's shuttingDown arm handles it
						// → PAUSED-for-resume, NOT FAILED.
						if e.shuttingDownOrCancelled(ctx) {
							return "", nil, completedSteps, gateErr
						}
						nextStepID = step.OnFail
						lastResultMessage = gateErr.Error()
						lastResultErr = gateErr
					} else {
						return "", nil, completedSteps, fmt.Errorf("gate evaluation failed for step %s: %w", currentStepID, gateErr)
					}
				}
			}
			if nextStepID == "" {
				// No on_success, no matching gates — try on_fail as last resort.
				if step.OnFail != "" {
					nextStepID = step.OnFail
					lastResultMessage = "no gate condition matched and no on_success defined"
					// The producer ran fine; the workflow author just didn't
					// wire a default. Leave pending for the terminal sweep.
				} else {
					return "", nil, completedSteps, fmt.Errorf("workflow step %s has no on_success transition and no matching gates", currentStepID)
				}
			} else {
				// Happy path: the consumer (gate or on_success target)
				// accepted the producer's output. Finalize now rather than
				// waiting for the terminal sweep — the sweep can't tell
				// which steps' output was actually consumed mid-run.
				e.finalizePendingOutcome(ctx, execution.ID, currentStepID, string(stepoutcome.OK), "", "", nil)
			}
			state.LastResult = append(json.RawMessage(nil), resultBytes...)
			// Phase D — per-step result mirror for the
			// ${outputs.<step>.<field>} interpolator. Stored
			// alongside LastResult so the same checkpoint write
			// covers both (no extra DB round-trip).
			if state.StepResults == nil {
				state.StepResults = map[string]json.RawMessage{}
			}
			state.StepResults[currentStepID] = append(json.RawMessage(nil), resultBytes...)
			state.ApprovalPendingStep = ""
			state.ApprovalGrantedStep = ""
			if err := e.saveCheckpoint(ctx, execution, nextStepID, completedSteps, state); err != nil {
				return "", nil, completedSteps, err
			}
			currentStepID = nextStepID
		case "gate":
			nextStepID, gateErr := e.runGateStep(ctx, task, execution, currentStepID, step, completedSteps, state)
			if gateErr != nil {
				return "", nil, completedSteps, gateErr
			}
			completedSteps = append(completedSteps, currentStepID)
			if err := e.saveCheckpoint(ctx, execution, nextStepID, completedSteps, state); err != nil {
				return "", nil, completedSteps, err
			}
			currentStepID = nextStepID
		case "approval":
			nextStepID, paused, apErr := e.runApprovalStep(ctx, task, execution, currentStepID, step, completedSteps, &state)
			if apErr != nil {
				return "", nil, completedSteps, apErr
			}
			if paused {
				return "", nil, completedSteps, errExecutionPaused
			}
			completedSteps = append(completedSteps, currentStepID)
			if err := e.saveCheckpoint(ctx, execution, nextStepID, completedSteps, state); err != nil {
				return "", nil, completedSteps, err
			}
			currentStepID = nextStepID
			continue
		case "plan":
			// Plan steps use PlanIndex for progress; undo the generic visit
			// increment so checkpoint resumes don't trigger the loop guard.
			visitCount[currentStepID]--
			state.VisitCounts = visitCount

			containerID, result, nextStepID, planCompletedSteps, err := e.executePlanStep(
				ctx, task, execution, plan, currentStepID, step, stepTimeout, &state, completedSteps, stepArtifacts,
			)
			// Adopt the sub-step-augmented slice returned by executePlanStep so
			// each plan role (plan_0_analyst, plan_1_coder, …) is visible in
			// the UI's step-progress panel instead of a single opaque "plan".
			completedSteps = planCompletedSteps
			if err != nil {
				// Phase 25 — lead handoff is NOT a failure: the
				// lead emitted a checkpoint / external_wait /
				// closure_request and the executor already
				// transitioned the task. Propagate the sentinel
				// straight up so the retry loop's IsLeadHandoff
				// guard can finalize cleanly. Without this,
				// plan-step OnFail would route handoff to the
				// fail-step and the retry loop would re-run the
				// lead, emitting duplicate checkpoints until
				// max_attempts.
				if IsLeadHandoff(err) {
					return "", nil, completedSteps, err
				}
				e.logger.Warn().
					Str("execution_id", execution.ID).
					Str("step", currentStepID).
					Str("on_fail", step.OnFail).
					Str("error", truncateStr(err.Error(), 500)).
					Msg("plan step failed")
				if step.OnFail != "" {
					// Shutdown guard (Fix-2, 2026-06-21): abort on_fail routing
					// during daemon teardown → PAUSED-for-resume, NOT FAILED.
					if e.shuttingDownOrCancelled(ctx) {
						return "", nil, completedSteps, err
					}
					completedSteps = append(completedSteps, currentStepID)
					state.LastResult = []byte(fmt.Sprintf(`{"error":%q}`, err.Error()))
					state.PlanSteps = nil
					state.PlanIndex = 0
					if saveErr := e.saveCheckpoint(ctx, execution, step.OnFail, completedSteps, state); saveErr != nil {
						return "", nil, completedSteps, saveErr
					}
					currentStepID = step.OnFail
					lastResultMessage = err.Error()
					lastResultErr = err
					continue
				}
				return "", nil, completedSteps, err
			}
			lastContainerID = containerID
			lastResult = result
			completedSteps = append(completedSteps, currentStepID)
			state.PlanSteps = nil
			state.PlanIndex = 0
			state.LastResult = append(json.RawMessage(nil), result...)
			state.ApprovalPendingStep = ""
			state.ApprovalGrantedStep = ""
			if err := e.saveCheckpoint(ctx, execution, nextStepID, completedSteps, state); err != nil {
				return "", nil, completedSteps, err
			}
			currentStepID = nextStepID
		case "spawn_project":
			// Inter-project orchestration Phase B (LLD §6.2).
			// Fire-and-forget — handler materialises the new
			// project + lineage row + optional initial_task,
			// then proceeds to OnSuccess immediately (no pause,
			// no CPC wait). To synchronously wait on the
			// spawned project's first task, the workflow author
			// pairs spawn_project with a follow-on call_project
			// step.
			result, err := e.handleSpawnProjectStep(ctx, task, execution, currentStepID, &step, state.StepResults)
			if err != nil {
				e.logger.Warn().
					Str("execution_id", execution.ID).
					Str("step", currentStepID).
					Str("on_fail", step.OnFail).
					Err(err).
					Msg("spawn_project step failed")
				if step.OnFail != "" {
					// Shutdown guard (Fix-2, 2026-06-21): abort on_fail routing
					// during daemon teardown → PAUSED-for-resume, NOT FAILED.
					if e.shuttingDownOrCancelled(ctx) {
						return "", nil, completedSteps, err
					}
					completedSteps = append(completedSteps, currentStepID)
					state.LastResult = []byte(fmt.Sprintf(`{"error":%q}`, err.Error()))
					if saveErr := e.saveCheckpoint(ctx, execution, step.OnFail, completedSteps, state); saveErr != nil {
						return "", nil, completedSteps, saveErr
					}
					currentStepID = step.OnFail
					lastResultMessage = err.Error()
					lastResultErr = err
					continue
				}
				return "", nil, completedSteps, err
			}
			completedSteps = append(completedSteps, currentStepID)
			state.LastResult = []byte(fmt.Sprintf(
				`{"spawned_project":%q,"spawn_id":%q,"initial_task_id":%q,"skipped":%t}`,
				result.SpawnedProject, result.SpawnID, result.InitialTaskID, result.Skipped,
			))
			if err := e.saveCheckpoint(ctx, execution, step.OnSuccess, completedSteps, state); err != nil {
				return "", nil, completedSteps, err
			}
			currentStepID = step.OnSuccess
		case "call_project":
			// Inter-project orchestration Phase A (LLD: docs/
			// low-level-design/inter-project-orchestration-design.md).
			// Handler creates the CPC ledger row + callee task and
			// transitions THIS task to WAITING_FOR_CHILDREN. The
			// caller is woken when the callee terminates via the
			// existing checkParentUnblock pathway — same primitive
			// as in-project delegation.
			result, err := e.handleCallProjectStep(ctx, task, execution, currentStepID, &step, state.StepResults)
			if err != nil {
				e.logger.Warn().
					Str("execution_id", execution.ID).
					Str("step", currentStepID).
					Str("on_fail", step.OnFail).
					Err(err).
					Msg("call_project step failed")
				if step.OnFail != "" {
					// Shutdown guard (Fix-2, 2026-06-21): abort on_fail routing
					// during daemon teardown → PAUSED-for-resume, NOT FAILED.
					if e.shuttingDownOrCancelled(ctx) {
						return "", nil, completedSteps, err
					}
					completedSteps = append(completedSteps, currentStepID)
					state.LastResult = []byte(fmt.Sprintf(`{"error":%q}`, err.Error()))
					if saveErr := e.saveCheckpoint(ctx, execution, step.OnFail, completedSteps, state); saveErr != nil {
						return "", nil, completedSteps, saveErr
					}
					currentStepID = step.OnFail
					lastResultMessage = err.Error()
					lastResultErr = err
					continue
				}
				return "", nil, completedSteps, err
			}
			// Mark the step done in the visit + completed lists,
			// pin the next step to OnSuccess so the resume re-
			// enters the workflow at the right place, then pause
			// the execution under PauseReasonAwaitingChildren —
			// mirrors the in-project delegation pause shape.
			completedSteps = append(completedSteps, currentStepID)
			nextStep := step.OnSuccess
			state.LastResult = []byte(fmt.Sprintf(`{"cross_project_call_id":%q,"callee_task_id":%q}`, result.CPCId, result.CalleeTaskID))
			state.PausedReason = result.PauseReason
			if err := e.saveCheckpoint(ctx, execution, nextStep, completedSteps, state); err != nil {
				return "", nil, completedSteps, err
			}
			_ = e.taskRepo.UpdateStatus(ctx, task.ID, persistence.TaskStatusWaitingForChildren)
			_ = e.execRepo.UpdateStatus(ctx, execution.ID, persistence.ExecutionStatusPaused)
			// Self-heal the create→flip window for any in-tree child
			// that terminated before the flip above (bug-sweep
			// follow-up 2026-06-04). A cross-project callee that isn't
			// visible via GetChildren makes this a no-op — the CPC
			// completion scanner owns that wake.
			e.unblockParentIfChildrenDone(ctx, task.ID)
			return "", nil, completedSteps, errExecutionPaused
		case "a2a_call":
			// Outbound A2A Phase B (LLD: https://docs.vornik.io
			// a2a-protocol-design.md "Outbound A2A client"). The
			// step POSTs a task to the partner agent's endpoint
			// and consumes the SSE stream synchronously within
			// this goroutine. No CPC ledger, no
			// WAITING_FOR_CHILDREN pause — A2A's wire is
			// stateless from vornik's persistence model. The
			// step's existing Timeout caps the call.
			result, callErr := e.handleA2ACallStep(ctx, currentStepID, &step)
			if callErr != nil {
				e.logger.Warn().
					Str("execution_id", execution.ID).
					Str("step", currentStepID).
					Str("on_fail", step.OnFail).
					Err(callErr).
					Msg("a2a_call step failed")
				if step.OnFail != "" {
					// Shutdown guard (Fix-2, 2026-06-21): abort on_fail routing
					// during daemon teardown → PAUSED-for-resume, NOT FAILED.
					if e.shuttingDownOrCancelled(ctx) {
						return "", nil, completedSteps, callErr
					}
					completedSteps = append(completedSteps, currentStepID)
					if result != nil {
						if buf, _ := json.Marshal(result); len(buf) > 0 {
							state.LastResult = buf
						} else {
							state.LastResult = []byte(fmt.Sprintf(`{"error":%q}`, callErr.Error()))
						}
					} else {
						state.LastResult = []byte(fmt.Sprintf(`{"error":%q}`, callErr.Error()))
					}
					if saveErr := e.saveCheckpoint(ctx, execution, step.OnFail, completedSteps, state); saveErr != nil {
						return "", nil, completedSteps, saveErr
					}
					currentStepID = step.OnFail
					lastResultMessage = callErr.Error()
					lastResultErr = callErr
					continue
				}
				return "", nil, completedSteps, callErr
			}
			completedSteps = append(completedSteps, currentStepID)
			if buf, mErr := json.Marshal(result); mErr == nil {
				state.LastResult = buf
			} else {
				state.LastResult = []byte(`{}`)
			}
			if err := e.saveCheckpoint(ctx, execution, step.OnSuccess, completedSteps, state); err != nil {
				return "", nil, completedSteps, err
			}
			currentStepID = step.OnSuccess
		case "system":
			// B-7: deterministic step backed by a registered Go
			// handler (no agent container, no LLM). The handler
			// is looked up by step.Handler from the executor's
			// SystemHandlerRegistry. Errors take the OnFail branch
			// when set; otherwise abort the execution.
			handler, ok := e.systemHandlers.Get(step.Handler)
			if !ok {
				err := fmt.Errorf("system step %s: no handler registered for %q (registered: %v)",
					currentStepID, step.Handler, e.systemHandlers.Names())
				e.recordStepOutcome(ctx, task, execution, currentStepID, "system", step.Handler,
					string(stepoutcome.GateFailed), "unknown_handler", err.Error(), nil, nil)
				if step.OnFail != "" {
					// Shutdown guard (Fix-2, 2026-06-21): abort on_fail routing
					// during daemon teardown → PAUSED-for-resume, NOT FAILED.
					if e.shuttingDownOrCancelled(ctx) {
						return "", nil, completedSteps, err
					}
					completedSteps = append(completedSteps, currentStepID)
					state.LastResult = []byte(fmt.Sprintf(`{"error":%q}`, err.Error()))
					if saveErr := e.saveCheckpoint(ctx, execution, step.OnFail, completedSteps, state); saveErr != nil {
						return "", nil, completedSteps, saveErr
					}
					currentStepID = step.OnFail
					continue
				}
				return "", nil, completedSteps, err
			}
			sysResult, sysErr := handler.Execute(ctx, SystemStepInput{
				Task:       task,
				Execution:  execution,
				StepID:     currentStepID,
				Step:       &step,
				PrevResult: state.LastResult,
			})
			if sysErr != nil {
				e.recordStepOutcome(ctx, task, execution, currentStepID, "system", step.Handler,
					string(stepoutcome.GateFailed), "handler_failed", sysErr.Error(), nil, nil)
				if step.OnFail != "" {
					// Shutdown guard (Fix-2, 2026-06-21): abort on_fail routing
					// during daemon teardown → PAUSED-for-resume, NOT FAILED.
					if e.shuttingDownOrCancelled(ctx) {
						return "", nil, completedSteps, sysErr
					}
					completedSteps = append(completedSteps, currentStepID)
					state.LastResult = []byte(fmt.Sprintf(`{"error":%q}`, sysErr.Error()))
					if saveErr := e.saveCheckpoint(ctx, execution, step.OnFail, completedSteps, state); saveErr != nil {
						return "", nil, completedSteps, saveErr
					}
					currentStepID = step.OnFail
					continue
				}
				return "", nil, completedSteps, sysErr
			}
			e.recordStepOutcome(ctx, task, execution, currentStepID, "system", step.Handler,
				string(stepoutcome.OK), "", "", nil, nil)
			// 2026-06-13 diagnostic: surface the system handler's result envelope
			// (e.g. forge.open_change_request's {cr_url, branch, state}) so a
			// "publish dispatched but opened no PR" outcome (state=no_change) is
			// visible in the journal instead of inferred from GitHub's absence.
			e.logger.Info().
				Str("task_id", task.ID).
				Str("execution_id", execution.ID).
				Str("step", currentStepID).
				Str("handler", step.Handler).
				Str("result", truncateStr(string(sysResult.Result), 400)).
				Msg("workflow: system step succeeded")
			completedSteps = append(completedSteps, currentStepID)
			if len(sysResult.Result) > 0 {
				state.LastResult = append(json.RawMessage(nil), sysResult.Result...)
			} else {
				state.LastResult = []byte(`{}`)
			}
			if err := e.saveCheckpoint(ctx, execution, step.OnSuccess, completedSteps, state); err != nil {
				return "", nil, completedSteps, err
			}
			currentStepID = step.OnSuccess
		default:
			return "", nil, completedSteps, fmt.Errorf("workflow step type %s is not implemented yet", step.Type)
		}
	}
}

// resolveTerminalOutcome maps a reached workflow terminal to the
// (containerID, result, error) tuple executeWorkflowAttempt returns.
// Extracted from the attempt loop (Track-B decomposition); behaviour is
// identical. The COMPLETED+Recovery case records a best-effort recovery
// marker so a graceful on_fail recovery exit stays observable.
// agentStepResult is the subset of an agent step's result.json the workflow
// loop interprets: the human message, output artifacts to forward to the next
// step, and the delegation/routing directives (delegatedTasks /
// selected_workflow). Named (was an inline anonymous struct repeated three
// times in the agent case) for the Track-B Phase 4 extraction.
type agentStepResult struct {
	Message          string              `json:"message"`
	OutputArtifacts  []map[string]string `json:"outputArtifacts"`
	DelegatedTasks   []delegatedTaskSpec `json:"delegatedTasks"`
	DelegationMode   string              `json:"delegationMode"`
	SelectedWorkflow string              `json:"selected_workflow"`
}

// pauseAwaitingChildren flips the task + execution to the awaiting-children
// pause state and checkpoints the resume step, then self-heals the
// create→flip window. Shared pause tail for both delegation branches
// (selected_workflow route + delegatedTasks); extracted in Track-B Phase 4.
//
// PauseReasonAwaitingChildren marks the pause so Recover() on the next daemon
// start does NOT auto-resume this row — awaiting-children resumes are driven
// by the scheduler when the last child completes. The unblockParentIfChildrenDone
// re-check closes the create→flip race (bug-sweep follow-up 2026-06-04): a
// child can be leasable the instant it is created and finish BEFORE the
// WAITING_FOR_CHILDREN flip lands, in which case its terminal finaliser saw a
// RUNNING parent and skipped the wake — leaving the parent hung until a restart
// sweep. Runs after the checkpoint save so a re-queued parent resumes with its
// state (incl. the resume guard's view) intact.
func (e *Executor) pauseAwaitingChildren(ctx context.Context, task *persistence.Task, execution *persistence.Execution, resumeStepID string, completedSteps []string, state *executionState) error {
	_ = e.taskRepo.UpdateStatus(ctx, task.ID, persistence.TaskStatusWaitingForChildren)
	state.PausedReason = PauseReasonAwaitingChildren
	_ = e.execRepo.UpdateStatus(ctx, execution.ID, persistence.ExecutionStatusPaused)
	if err := e.saveCheckpoint(ctx, execution, resumeStepID, completedSteps, *state); err != nil {
		return err
	}
	e.unblockParentIfChildrenDone(ctx, task.ID)
	return nil
}

// handleSelectedWorkflowRoute handles a strict-adaptive route step's
// {"selected_workflow": "<id>"} directive. Extracted from the agent case
// (Track-B Phase 4); behaviour is identical.
//
// fired=false means this step is NOT a strict route with a candidate list (the
// caller falls through to the normal post-step flow — step.OnSuccess, the
// `delegated` terminal, becomes nextStepID). When fired, the parent has
// delegated the real work to a child and is paused: on err==nil the caller
// returns (containerID, result, completedSteps, errExecutionPaused); on err!=nil
// it returns ("", nil, completedSteps, err). The route step is one-shot — its
// only job is dispatching to the chosen workflow; the child carries the real
// output and is observable on its own task ID.
//
// Fix C (2026-05-15 incident) + empty-pick fallback (2026-05-18): the lead's
// pick must be in the candidate list AND non-empty. Bad picks silently created
// unbounded routing loops on the assistant project (default=adaptive); empty
// picks (a refusal — "config missing, add this JSON file" prose instead of a
// JSON pick) fell through to legacy free-form planning and surfaced the refusal
// text to the operator as if it were a deliverable. Both failure modes share
// one corrective-retry path with a one-shot cap and an explicit hint; an empty
// pick on retry fails loud rather than falling through to free-form planning.
//
// Pausing on WAITING_FOR_CHILDREN (2026-05-21) holds the dispatcher's "task
// done" signal until the child (and its descendants) actually produced output —
// pre-fix the parent completed straight to OnSuccess and the dispatcher resumed
// on the parent's "selected_workflow=research" outcome, useless for single-shot
// channels (email, voice). Safe against the historical unbounded-respawn loop:
// the pre-step resume guard detects existing children on resume and bypasses the
// LLM call, so checkParentUnblock → fresh-execution transitions cleanly through
// OnSuccess instead of re-running the lead pick.
func (e *Executor) handleSelectedWorkflowRoute(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	plan *executionPlan,
	currentStepID string,
	step registry.WorkflowStep,
	stepTimeout time.Duration,
	opts *agentInputOpts,
	roleConfig *registry.SwarmRole,
	result *agentStepResult,
	routeAlreadyHandled bool,
	state *executionState,
	completedSteps []string,
	lastContainerID string,
	lastResult []byte,
) (bool, string, []byte, []string, error) {
	if routeAlreadyHandled || !isStrictRouteStep(plan.workflow, currentStepID) || plan.project == nil || len(plan.project.AdaptiveCandidateWorkflows) == 0 {
		return false, "", nil, completedSteps, nil
	}
	badPick := result.SelectedWorkflow != "" && !slices.Contains(plan.project.AdaptiveCandidateWorkflows, result.SelectedWorkflow)
	emptyPick := result.SelectedWorkflow == ""
	if badPick || emptyPick {
		retryStepID := currentStepID + "_route_retry"
		retryReason := "out_of_list"
		if emptyPick {
			retryReason = "empty_selected_workflow"
		}
		e.logger.Warn().
			Str("execution_id", execution.ID).
			Str("step", currentStepID).
			Str("retry_step", retryStepID).
			Str("bad_pick", result.SelectedWorkflow).
			Str("retry_reason", retryReason).
			Strs("candidates", plan.project.AdaptiveCandidateWorkflows).
			Msg("strict adaptive: lead pick invalid; re-running with corrective hint")
		correctedOpts := *opts
		correctedOpts.StepPrompt = opts.StepPrompt + buildRouteCorrectiveHint(result.SelectedWorkflow, plan.project.AdaptiveCandidateWorkflows)
		retryCID, retryBytes, retryErr := e.executeAgentStepWithFallback(ctx, task, execution, plan, retryStepID, step, stepTimeout, &correctedOpts, roleConfig)
		if retryErr != nil {
			return true, "", nil, completedSteps, fmt.Errorf("strict adaptive: corrective re-run failed: %w", retryErr)
		}
		lastContainerID = retryCID
		lastResult = retryBytes
		completedSteps = append(completedSteps, retryStepID)
		*result = agentStepResult{}
		if unmarshalErr := json.Unmarshal(retryBytes, result); unmarshalErr != nil {
			return true, "", nil, completedSteps, fmt.Errorf("strict adaptive: corrective re-run produced unparseable result: %w", unmarshalErr)
		}
		if result.SelectedWorkflow == "" {
			return true, "", nil, completedSteps, fmt.Errorf("strict adaptive: corrective re-run did not emit selected_workflow")
		}
	}

	chosen, err := e.delegateSelectedWorkflow(ctx, task, plan.project, result.SelectedWorkflow)
	if err != nil {
		return true, "", nil, completedSteps, fmt.Errorf("strict adaptive: %w", err)
	}
	e.logger.Info().
		Str("task_id", task.ID).
		Str("execution_id", execution.ID).
		Str("requested_workflow", result.SelectedWorkflow).
		Str("delegated_workflow", chosen).
		Msg("strict adaptive: lead picked workflow; child task spawned — pausing parent")
	completedSteps = append(completedSteps, currentStepID)
	if pErr := e.pauseAwaitingChildren(ctx, task, execution, currentStepID, completedSteps, state); pErr != nil {
		return true, "", nil, completedSteps, pErr
	}
	return true, lastContainerID, lastResult, completedSteps, nil
}

// handleDelegatedTasks creates the child tasks an agent requested via
// delegatedTasks and pauses the parent until they complete. Extracted from the
// agent case (Track-B Phase 4); behaviour is identical. fired=false means the
// step requested no delegation (the loop continues). When fired, the caller
// returns errExecutionPaused on err==nil, or the error otherwise. The requested
// delegationMode (SEQUENTIAL / PARALLEL / FAN_OUT) selects the child execution
// topology (see createDelegatedTasks + https://docs.vornik.io
// 10-delegation-engine.md §3). A step-level DelegatedWorkflow deterministically
// pins the subtask workflow when the LLM didn't set it per-task — so subtasks
// run the intended workflow (e.g. issue-subtask) rather than silently falling
// back to the project default (dev-pipeline).
func (e *Executor) handleDelegatedTasks(ctx context.Context, task *persistence.Task, execution *persistence.Execution, currentStepID string, step registry.WorkflowStep, result *agentStepResult, state *executionState, completedSteps []string) (bool, error) {
	if len(result.DelegatedTasks) == 0 {
		return false, nil
	}
	if step.DelegatedWorkflow != "" {
		for i := range result.DelegatedTasks {
			if strings.TrimSpace(result.DelegatedTasks[i].Workflow) == "" {
				result.DelegatedTasks[i].Workflow = step.DelegatedWorkflow
			}
		}
	}
	if err := e.createDelegatedTasks(ctx, task, result.DelegatedTasks, parseDelegationMode(result.DelegationMode)); err != nil {
		return true, fmt.Errorf("failed to create delegated tasks: %w", err)
	}
	if err := e.pauseAwaitingChildren(ctx, task, execution, currentStepID, completedSteps, state); err != nil {
		return true, err
	}
	return true, nil
}

// dispatchAgentStep runs one agent step and returns its container id, result
// bytes, and error. Extracted from executeWorkflowAttempt's agent case
// (Track-B decomposition, Phase 3); behaviour is identical. Three mutually
// exclusive paths:
//
//   - routeAlreadyHandled (resume): synthesise the delegated children's
//     OUTCOMES as the parent's result and skip the LLM call. The dispatcher
//     (and any single-shot channel) expects the child's work product back
//     from the parent, not an internal "route already executed" note. The
//     synthesised result carries NEITHER selected_workflow NOR
//     delegated_tasks, so both spawn branches in the caller short-circuit (no
//     re-spawn on resume) and the post-step transition advances to OnSuccess.
//   - single-candidate strict route: when the project's adaptive candidate
//     list has exactly one entry the lead has nothing to pick — synthesise the
//     routing result directly and skip the LLM call. Saves tokens and closes
//     the failure mode where flash-tier models invent a "config missing"
//     refusal instead of emitting the only valid pick (observed live
//     2026-05-18 with janka project, lead zai.glm-4.7-flash).
//   - otherwise: run the agent through the model-fallback retry layer.
//
// containerID is empty for the two synthesised paths (no container runs).
func (e *Executor) dispatchAgentStep(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	plan *executionPlan,
	currentStepID string,
	step registry.WorkflowStep,
	stepTimeout time.Duration,
	opts *agentInputOpts,
	roleConfig *registry.SwarmRole,
	routeAlreadyHandled bool,
	resumeChildren []*persistence.Task,
) (string, []byte, error) {
	if routeAlreadyHandled {
		resultBytes := e.synthesizeResumeResult(ctx, resumeChildren)
		e.logger.Info().
			Str("task_id", task.ID).
			Str("execution_id", execution.ID).
			Str("step", currentStepID).
			Int("children", len(resumeChildren)).
			Msg("resume guard: parent resumed after children — surfacing child outcomes, skipping LLM + spawn")
		return "", resultBytes, nil
	}
	if isStrictRouteStep(plan.workflow, currentStepID) && plan.project != nil && len(plan.project.AdaptiveCandidateWorkflows) == 1 {
		only := plan.project.AdaptiveCandidateWorkflows[0]
		e.logger.Info().
			Str("task_id", task.ID).
			Str("execution_id", execution.ID).
			Str("step", currentStepID).
			Str("workflow", only).
			Msg("strict adaptive: single candidate — auto-routing without LLM call")
		synthesized, _ := json.Marshal(struct {
			Message          string `json:"message"`
			SelectedWorkflow string `json:"selected_workflow"`
		}{
			Message:          fmt.Sprintf("auto-routed (single candidate %s)", only),
			SelectedWorkflow: only,
		})
		return "", synthesized, nil
	}
	return e.executeAgentStepWithFallback(ctx, task, execution, plan, currentStepID, step, stepTimeout, opts, roleConfig)
}

// buildStepFailureRecovery maps a failed agent step's error to the
// RecoveryContext the recovery step consumes, or nil when recovery is opted
// out (pedantic mode) — the caller then leaves any prior PendingRecovery
// untouched. Extracted from the agent case (Track-B Phase 3); behaviour is
// identical. Today only *RecoverableVerifierError carries BlockedURLs; all
// other failures route through the canonical classifier and map to the
// lead-facing recovery class so the lead's per-class playbook
// (budget_exhausted / hallucination_flagged / tool_error) is reachable.
// Unclassifiable failures collapse to agent_error, the lead's catch-all.
// (Slice 5 — previously every non-verifier failure hard-coded agent_error,
// leaving the other playbook branches dead.) See
// https://docs.vornik.io
func buildStepFailureRecovery(err error, currentStepID string, task *persistence.Task, plan *executionPlan) *RecoveryContext {
	if resolvePedantic(task.Payload, plan.workflow, plan.project) {
		return nil
	}
	var rve *RecoverableVerifierError
	if errors.As(err, &rve) {
		return &RecoveryContext{
			FailedStep:    currentStepID,
			FailureClass:  "verifier_block",
			FailureReason: truncateStr(err.Error(), 1500),
			BlockedURLs:   rve.BlockedURLs,
		}
	}
	return &RecoveryContext{
		FailedStep:    currentStepID,
		FailureClass:  recoveryFailureClass(ClassifyExecutionFailure(err, "")),
		FailureReason: truncateStr(err.Error(), 1500),
	}
}

// resolveResumeGuard detects whether an agent step is re-entering AFTER its
// delegated children already ran, and reports how the loop should treat it.
// Extracted from executeWorkflowAttempt's agent case (Track-B decomposition,
// Phase 2); behaviour is identical.
//
// 2026-05-21 resume guard: when a strict-adaptive parent pauses on
// WAITING_FOR_CHILDREN and is later requeued via checkParentUnblock, the
// scheduler spawns a fresh execution that restarts at
// workflow.Entrypoint=route. Re-running the route step's LLM would pick a
// workflow + spawn ANOTHER child — the unbounded loop bug observed
// pre-2026-05-15 (20+ czech-news tasks in one autonomy tick). Detect by
// querying for existing children. If any exist, this is a resume run: the
// caller synthesises a no-op result, skips the LLM call, skips the spawn
// branch via the returned handled=true, and lets the post-step transition
// advance the parent to OnSuccess.
//
// The guard applies to the built-in `adaptive` workflow AND to any custom
// workflow that opts in via `resume_after_children: true` (e.g. github-router:
// intake delegates issue-fix, then must advance to its publish step on resume
// instead of re-delegating). Note: no AdaptiveCandidateWorkflows requirement
// here. The resume guard must also cover resume_after_children workflows that
// delegate via `delegatedTasks` rather than `selected_workflow` (e.g.
// issue-fix's decompose step) — those have no candidate list. Re-running such
// a step on resume would re-spawn its subtasks (unbounded). The candidate-list
// checks stay on the auto-route + strict-route paths in the caller, which are
// selected_workflow-specific.
//
// CRITICAL: gate on isStrictRouteStep (entrypoint-confined for
// resume_after_children), NOT a bare ResumeAfterChildren check. A
// resume_after_children workflow has steps AFTER the delegating entrypoint
// that must genuinely run on resume — e.g. issue-fix's `review` agent step. A
// bare ResumeAfterChildren guard fires for the review step too (children
// exist), synthesises a no-op, skips the reviewer LLM, and leaves
// `review.approved` unset → no gate matches → on_success: failed → parent
// FAILS even though every child merged (task_20260613142534_c0ec8045970896cb).
// Confining to the entrypoint (decompose) lets review/publish execute
// normally on resume.
//
// Returns (handled, childFailed, children): handled=true when a resume was
// detected; childFailed=true when any detected child is FAILED; children is
// the detected child set (nil when not handled).
func (e *Executor) resolveResumeGuard(ctx context.Context, task *persistence.Task, plan *executionPlan, currentStepID string) (bool, bool, []*persistence.Task) {
	if !isStrictRouteStep(plan.workflow, currentStepID) || plan.project == nil || e.taskRepo == nil {
		return false, false, nil
	}
	children, gErr := e.taskRepo.GetChildren(ctx, task.ID)
	if gErr != nil || len(children) == 0 {
		return false, false, nil
	}
	for _, ch := range children {
		if ch.Status == persistence.TaskStatusFailed {
			return true, true, children
		}
	}
	return true, false, children
}

// prepareAgentStepInput builds the agentInputOpts for an agent step and
// resolves the step's role config. Extracted from executeWorkflowAttempt's
// agent case (Track-B decomposition, Phase 1 — input-prep); behaviour is
// identical. It consumes operator hints, assembles the step prompt (via
// assembleStepPrompt), prewarms the strategist context, exposes the adaptive
// candidate list, forwards any pending recovery context, carries the
// complexity tier forward, and resolves the role's prompt/permissions/schema
// (via resolveRoleOpts). Mutates *state (recovery forwarding) and flips
// *forkOverrideApplied on the first visit to a forked step.
func (e *Executor) prepareAgentStepInput(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	plan *executionPlan,
	currentStepID string,
	step registry.WorkflowStep,
	stepArtifacts []map[string]string,
	lastResultMessage string,
	state *executionState,
	forkOverrideApplied *bool,
) (*agentInputOpts, *registry.SwarmRole) {
	// Feature #3 Phase C — consume any pending operator hints for this
	// step. Returns the concatenated <operator-hint>...</operator-hint>
	// blocks ready to prepend; empty string when no hints (or repo
	// unwired). Each consumed hint publishes a `hint_applied` live event.
	hintPrefix := e.consumeHintsForStep(ctx, task.ID, execution.ID, currentStepID)
	stepPrompt := e.assembleStepPrompt(execution, currentStepID, step, hintPrefix, forkOverrideApplied)
	opts := &agentInputOpts{
		InputArtifacts: stepArtifacts,
		PreviousResult: lastResultMessage,
		StepPrompt:     stepPrompt,
	}
	// Inject a structured recent-activity block into the strategist's
	// prompt so next-tick reasoning sees last 24h of fills, cancels, and
	// refusals as data rather than depending on memory_search recall.
	// Nil-safe: skips silently when the trading repo isn't wired or the
	// project has no recent rows.
	if step.Role == "strategist" {
		e.prewarmStrategistContext(ctx, plan, task, opts)
	}
	// Strict adaptive routing: when running the adaptive workflow's route
	// step, expose the project's candidate list so the lead's prompt can
	// reference it. Only fires when the project has opted in via
	// AdaptiveCandidateWorkflows; otherwise the field is unset and the
	// lead falls back to its legacy free-form planning behaviour.
	if plan.workflow != nil && plan.workflow.ID == "adaptive" && plan.project != nil && len(plan.project.AdaptiveCandidateWorkflows) > 0 {
		opts.AdaptiveCandidateWorkflows = plan.project.AdaptiveCandidateWorkflows
	}
	// Swarm recovery: a prior step's on_fail handler stashed the structured
	// failure context here. forwardPendingRecovery moves it onto this step's
	// agent input, clears it (so later steps don't see a stale banner), and
	// attaches the advisory learned-remediation overlay (Consumer A) — so an
	// agent-type recover step (e.g. dev-pipeline's analyst recover-checkpoint)
	// reaches the same instinct surface as the plan-step path.
	if e.forwardPendingRecovery(ctx, task, execution, state, opts) {
		e.logger.Info().
			Str("execution_id", execution.ID).
			Str("step", currentStepID).
			Str("failed_step", opts.RecoveryContext.FailedStep).
			Str("failure_class", opts.RecoveryContext.FailureClass).
			Int("blocked_urls", len(opts.RecoveryContext.BlockedURLs)).
			Int("learned_remediations", len(opts.RecoveryContext.LearnedRemediations)).
			Msg("recovery: forwarding failure context to recovery step")
	}
	// Dynamic tool budget: forward the planner's complexity tier (set on
	// state by an earlier planning step) so the worker spawn can scale this
	// role's tool-iteration budget. Empty = standard (no scaling). See
	// https://docs.vornik.io
	opts.ComplexityTier = state.ComplexityTier
	roleConfig := e.resolveRoleOpts(plan, step, task, opts)
	return opts, roleConfig
}

// assembleStepPrompt builds the prompt sent to an agent step: the step's
// base prompt, plus the gate response-format suffix (when the step has
// gates), plus any operator-hint prefix, plus the fork-from-step operator
// override prepended on the FIRST visit to a forked step. Once the override
// is applied it flips *forkOverrideApplied so re-entries (shape retries,
// infra retries) run with the unmodified prompt. Extracted from the agent
// case (Track-B Phase 1) to keep prepareAgentStepInput under the ratchet.
func (e *Executor) assembleStepPrompt(
	execution *persistence.Execution,
	currentStepID string,
	step registry.WorkflowStep,
	hintPrefix string,
	forkOverrideApplied *bool,
) string {
	// When a step has gates, append response format instructions to the
	// prompt so the agent knows what structured JSON to produce.
	stepPrompt := step.Prompt
	if len(step.Gates) > 0 {
		stepPrompt += buildGatePromptSuffix(step.Gates)
	}
	if hintPrefix != "" {
		stepPrompt = hintPrefix + stepPrompt
	}
	if !*forkOverrideApplied &&
		execution.ForkedFromStepID != nil &&
		*execution.ForkedFromStepID == currentStepID &&
		execution.ForkedPromptOverride != nil &&
		*execution.ForkedPromptOverride != "" {
		stepPrompt = *execution.ForkedPromptOverride + "\n\n---\n\n" + stepPrompt
		*forkOverrideApplied = true
		e.logger.Info().
			Str("execution_id", execution.ID).
			Str("step_id", currentStepID).
			Msg("fork: applied prompt override on forked step's first iteration")
	}
	return stepPrompt
}

// resolveRoleOpts resolves the step's role from the swarm config and fills
// the role-specific fields on opts (system prompt, permissions, response
// format, shape-retry hint, output schema). Returns the matched role config
// so the fallback retry layer can see the role's `modelFallback:` field —
// the executor's primary role lookup happens deeper in container.go and isn't
// visible to retry.go. Composition goes through BuildEffectiveRolePrompt so
// every role inherits the builtin safety prelude and any swarm-wide clauses
// without the role YAML having to repeat them. Returns nil when no role
// matches (or the plan carries no swarm). Extracted from the agent case
// (Track-B Phase 1).
func (e *Executor) resolveRoleOpts(plan *executionPlan, step registry.WorkflowStep, task *persistence.Task, opts *agentInputOpts) *registry.SwarmRole {
	if plan.swarm == nil {
		return nil
	}
	for i, r := range plan.swarm.Roles {
		if r.Name != step.Role {
			continue
		}
		if prompt := registry.BuildEffectiveRolePrompt(plan.swarm, r); prompt != "" {
			opts.SystemPrompt = prompt
		}
		applyCounterfactualPromptOverride(opts, task, step.Role)
		perm := r.Permissions
		opts.Permissions = &perm
		opts.ResponseFormat = effectiveResponseFormat(&r)
		opts.ShapeRetryHint = r.ShapeRetryHint
		roleConfig := &plan.swarm.Roles[i]
		applyRoleSchemaOpts(opts, roleConfig)
		return roleConfig
	}
	return nil
}

func (e *Executor) resolveTerminalOutcome(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	terminal registry.WorkflowTerminal,
	currentStepID string,
	completedSteps []string,
	lastContainerID string,
	lastResult []byte,
	lastResultMessage string,
	lastResultErr error,
) (string, []byte, error) {
	switch terminal.Status {
	case "COMPLETED":
		if terminal.Recovery && e.recoveryEvents != nil {
			if recErr := e.recoveryEvents.Record(ctx, &persistence.RecoveryEvent{
				ID:          persistence.GenerateID("rcv"),
				ProjectID:   task.ProjectID,
				TaskID:      task.ID,
				ExecutionID: execution.ID,
				WorkflowID:  execution.WorkflowID,
				TerminalID:  currentStepID,
			}); recErr != nil {
				e.logger.Warn().Err(recErr).Str("execution_id", execution.ID).
					Str("terminal", currentStepID).Msg("failed to record recovery event")
			}
		}
		return lastContainerID, lastResult, nil
	case "FAILED":
		msg := terminal.Message
		if msg == "" {
			msg = "workflow failed"
		}
		lastStep := "unknown"
		if len(completedSteps) > 0 {
			lastStep = completedSteps[len(completedSteps)-1]
		}
		detail := fmt.Sprintf("%s (last step: %s", msg, lastStep)
		if lastResultMessage != "" {
			reason := lastResultMessage
			if len(reason) > 500 {
				reason = reason[:500] + "..."
			}
			detail += fmt.Sprintf(", reason: %s", reason)
		}
		detail += ")"
		// Fix-3 (2026-06-21): wrap the underlying cause with %w so the error
		// chain (e.g. context.Canceled) survives to ClassifyExecutionFailure.
		// Without this, errors.Is(err, context.Canceled) returns false at the
		// classifier and the task lands UNKNOWN instead of CANCELLED.
		// If no underlying error is available (the message came from a string
		// source only), fall back to %s to preserve the existing behavior.
		if lastResultErr != nil {
			return "", nil, fmt.Errorf("%s: %w", detail, lastResultErr)
		}
		return "", nil, fmt.Errorf("%s", detail)
	case "CANCELLED":
		return "", nil, fmt.Errorf("workflow cancelled at step %s", lastStepOrUnknown(completedSteps))
	default:
		return "", nil, fmt.Errorf("workflow reached unsupported terminal status %s", terminal.Status)
	}
}

// runGateStep evaluates a standalone gate step, records its outcome, and
// returns the next step id. Extracted from the attempt loop (Track-B
// decomposition); behaviour is identical. err is non-nil only for a hard
// gate failure with no on_fail target — the caller then aborts the
// attempt; otherwise the caller advances to the returned next step. (The
// completedSteps append + saveCheckpoint stay in the loop, shared with
// the other step types.)
func (e *Executor) runGateStep(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	currentStepID string,
	step registry.WorkflowStep,
	completedSteps []string,
	state executionState,
) (string, error) {
	nextStepID, gateTrace, err := evaluateGateStepTraced(step, state.LastResult)
	e.logGateTrace(ctx, task, execution, currentStepID, gateTrace, err, nextStepID)

	if err == nil {
		// Gate produced a routing decision cleanly. Record ok for
		// consistency with agent-step finalization.
		e.recordStepOutcome(ctx, task, execution, currentStepID, "gate", "",
			string(stepoutcome.OK), "", "", nil, nil)
		return nextStepID, nil
	}

	// Standalone gate step failed. Attribute based on the error shape:
	// malformed upstream JSON blames the previous step (via its pending
	// row); gate spec or semantic mismatch blames the gate.
	producerOutcome, producerClass := classifyGateEvalError(err)

	// "No gate condition matched" with an on_success target on the gate
	// step is the explicit default route — a clean fall-through, not a
	// failure (same intuition as a switch's default case).
	if producerOutcome == string(stepoutcome.DownstreamRejected) && step.OnSuccess != "" {
		e.recordStepOutcome(ctx, task, execution, currentStepID, "gate", "",
			string(stepoutcome.OK), "", "", nil, nil)
		return step.OnSuccess, nil
	}

	if producerOutcome == string(stepoutcome.ParseError) && len(completedSteps) > 0 {
		prev := completedSteps[len(completedSteps)-1]
		e.finalizePendingOutcome(ctx, execution.ID, prev, producerOutcome, producerClass, err.Error(), &currentStepID)
	}
	// The gate step itself is recorded as gate_failed (its own evaluation
	// didn't produce a routing decision).
	e.recordStepOutcome(ctx, task, execution, currentStepID, "gate", "",
		string(stepoutcome.GateFailed), producerClass, err.Error(), nil, nil)
	if step.OnFail == "" {
		return "", err
	}
	return step.OnFail, nil
}

// runApprovalStep handles an approval step. When approval was already
// granted, it clears the approval state and returns the next step id with
// paused=false — the caller advances via the shared append + checkpoint.
// Otherwise it persists the operator-pause state, flips task/execution
// status to paused, and returns paused=true — the caller returns
// errExecutionPaused. Extracted from the attempt loop (Track-B
// decomposition); behaviour is identical. Mutates *state.
func (e *Executor) runApprovalStep(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	currentStepID string,
	step registry.WorkflowStep,
	completedSteps []string,
	state *executionState,
) (string, bool, error) {
	if state.ApprovalGrantedStep == currentStepID {
		nextStepID := step.OnSuccess
		if nextStepID == "" {
			return "", false, fmt.Errorf("approval step %s has no on_success transition", currentStepID)
		}
		state.ApprovalPendingStep = ""
		state.ApprovalGrantedStep = ""
		return nextStepID, false, nil
	}
	state.CurrentStepID = currentStepID
	state.CompletedSteps = append([]string{}, completedSteps...)
	state.ApprovalPendingStep = currentStepID
	// Approval pause reuses the operator-pause class so Recover() doesn't
	// auto-resume; an operator must grant approval to move forward.
	state.PausedReason = PauseReasonOperator
	if err := e.saveExecutionState(ctx, execution, *state); err != nil {
		return "", false, err
	}
	_ = e.taskRepo.UpdateStatus(ctx, task.ID, persistence.TaskStatusPending)
	_ = e.execRepo.UpdateStatus(ctx, execution.ID, persistence.ExecutionStatusPaused)
	return "", true, nil
}

// prewarmStrategistContext fills the strategist's agent input with the
// recent-activity block plus pre-warmed watchlist quote + indicator tables
// (fetched in parallel), so next-tick reasoning gets ready-made data
// instead of ~16 sequential get_quote + ~80 bars/TA tool calls. All
// best-effort: the fields stay empty when the trading repo/broker is
// unavailable, and the strategist falls back to the per-symbol path.
// Extracted from executeWorkflowAttempt's agent-step prep (Track-B).
func (e *Executor) prewarmStrategistContext(ctx context.Context, plan *executionPlan, task *persistence.Task, opts *agentInputOpts) {
	opts.RecentActivityBlock = e.buildRecentActivityBlock(ctx, task.ProjectID)
	var prewarmWg sync.WaitGroup
	prewarmWg.Add(2)
	go func() {
		defer prewarmWg.Done()
		opts.WatchlistQuotesBlock = e.buildWatchlistQuotesBlock(ctx, plan.project)
	}()
	go func() {
		defer prewarmWg.Done()
		opts.WatchlistIndicatorsBlock = e.buildWatchlistIndicatorsBlock(ctx, plan.project)
	}()
	prewarmWg.Wait()
}

// resolvePedantic computes whether pedantic mode is active for the
// given task + workflow + project, applying the narrower-wins
// precedence per the swarm-recovery LLD §6:
//
//  1. Task-level (task.payload.context.pedantic) wins if set.
//  2. Else workflow.Pedantic if set.
//  3. Else project.Pedantic if set.
//  4. Else false (recovery flow active — today's "more creative"
//     default for every project that hasn't opted into strict
//     instruction following).
//
// All three scope fields are *bool so an absent field is
// distinguishable from an explicit false. The function never
// dereferences a nil pointer; callers can pass nil plan / project
// safely (returns false — non-pedantic).
func resolvePedantic(payload []byte, workflow *registry.Workflow, project *registry.Project) bool {
	if len(payload) > 0 {
		var parsed struct {
			Context struct {
				Pedantic *bool `json:"pedantic"`
			} `json:"context"`
		}
		if json.Unmarshal(payload, &parsed) == nil && parsed.Context.Pedantic != nil {
			return *parsed.Context.Pedantic
		}
	}
	if workflow != nil && workflow.Pedantic != nil {
		return *workflow.Pedantic
	}
	if project != nil && project.Pedantic != nil {
		return *project.Pedantic
	}
	return false
}

// extractTaskInputArtifacts parses task.Payload.context.inputFiles
// and converts each path into the {name, sourcePath} shape the
// per-step container staging expects. Returns nil for empty or
// unparseable payloads (best-effort; a malformed payload should
// not stop the workflow — only the input files are lost).
//
// SKIPS files that have a matching context.inputExtractions entry:
// when extraction succeeded at create_task time, the worker
// reaches the content via mcp__vornik__document_* (Phase 2 tools).
// Staging the raw binary on top of that has caused operators to
// observe the worker file_read'ing the EPUB and blowing the 32 MB
// chat-proxy cap (2026-05-21 incident, tasks T-fa9e/T-7f98/T-8889).
// Removing the staged copy structurally eliminates the failure.
//
// Extracted into a free function so the contract can be unit-
// tested without booting a full Executor + plan + container
// stack.
func extractTaskInputArtifacts(payload []byte) []map[string]string {
	if len(payload) == 0 {
		return nil
	}
	var parsed struct {
		Context struct {
			InputFiles       []string         `json:"inputFiles"`
			InputExtractions []map[string]any `json:"inputExtractions"`
		} `json:"context"`
	}
	if json.Unmarshal(payload, &parsed) != nil {
		return nil
	}
	// Build a basename → extracted? lookup so positional matches
	// don't drag stale references when the dispatcher records
	// fewer extractions than files (e.g. one EPUB ingested, one
	// .bin pass-through). When extractions equal inputFiles in
	// count we treat all as extracted; the positional join is
	// exactly what buildAttachedFilesBlock uses upstream.
	extractedBasenames := make(map[string]bool, len(parsed.Context.InputExtractions))
	if len(parsed.Context.InputExtractions) > 0 {
		if len(parsed.Context.InputExtractions) == len(parsed.Context.InputFiles) {
			for i, path := range parsed.Context.InputFiles {
				if path == "" {
					continue
				}
				if _, ok := parsed.Context.InputExtractions[i]["extracted_document_id"]; ok {
					extractedBasenames[filepath.Base(path)] = true
				}
			}
		} else {
			// Mismatched counts — flag every basename as extracted
			// to favour memory-only access over a maybe-staged copy.
			// Mirrors buildAttachedFilesBlock's fallback shape.
			for _, path := range parsed.Context.InputFiles {
				if path != "" {
					extractedBasenames[filepath.Base(path)] = true
				}
			}
		}
	}

	out := make([]map[string]string, 0, len(parsed.Context.InputFiles))
	for _, path := range parsed.Context.InputFiles {
		if path == "" {
			continue
		}
		base := filepath.Base(path)
		if extractedBasenames[base] {
			// Skip staging — agent uses document_* tools instead.
			continue
		}
		out = append(out, map[string]string{
			"name":       base,
			"sourcePath": path,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// handleSuccess processes a successful execution.
func (e *Executor) handleSuccess(ctx context.Context, task *persistence.Task, execution *persistence.Execution, containerID string, result []byte) {
	// Operator-pause guard — same reasoning as handleFailure.
	// Writing COMPLETED over PAUSED would silently discard the
	// operator's pause intent and force the task to terminal.
	// The re-fetch catches a pause that arrived between goroutine
	// start and finaliser; the goroutine's `task` argument is a
	// pre-pause snapshot.
	if fresh, ferr := e.taskRepo.Get(context.Background(), task.ID); ferr == nil &&
		fresh != nil && fresh.Status == persistence.TaskStatusPaused {
		e.logger.Info().
			Str("task_id", task.ID).
			Str("execution_id", execution.ID).
			Msg("handleSuccess: task already PAUSED by operator — skipping terminal status write")
		return
	}

	// Calculate duration
	duration := time.Since(e.getStartTime(task.ID))

	// Any remaining pending_validation rows belong to steps whose output
	// was never explicitly finalized as a parse_error / schema_violation /
	// etc. Since the execution as a whole completed, the absence of
	// explicit attribution means the outputs were consumable — sweep to
	// "ok". (Real parse failures would have been finalized at the
	// consumer site in plan_step.go before we got here.)
	e.sweepPendingOutcomes(ctx, execution.ID, string(stepoutcome.OK))

	// Update execution record
	_ = e.execRepo.RecordCompletion(ctx, execution.ID, result)

	// Update task status. Mirror the DB write into the in-memory
	// struct so anything that consumes `task` after this point
	// (notifier, judge, post-mortem) sees a coherent terminal
	// status — pre-fix the executor handed off a stale "LEASED" /
	// "RUNNING" Status to NotifyTaskCompleted, which rendered as
	// "[Task X reached terminal status: LEASED.]" in the synthetic
	// follow-up turn (see 2026-05-21 watchlist incident).
	_ = e.taskRepo.UpdateStatus(ctx, task.ID, persistence.TaskStatusCompleted)
	task.Status = persistence.TaskStatusCompleted
	e.settleBudgetReservation(ctx, task.ID)
	e.cascadeOrphanExecutions(ctx, task.ID)
	// Inter-project orchestration: if this task is a callee
	// of a `call_project` step, resolve the matching CPC row
	// before the parent unblock fires. The wake itself flows
	// through checkParentUnblock just below — same path as
	// in-project delegation.
	e.resolveCrossProjectCallForTask(ctx, task, true)

	// Containers are cleaned up per-step in executeAgentStep after reading
	// result.json + artifacts. No additional cleanup needed here.

	// Record metrics
	if e.metrics != nil {
		e.metrics.RecordCompleted(task.ProjectID, duration.Seconds())
	}

	// Check if this task is a child — unblock parent if all children are done.
	e.checkParentUnblock(ctx, task)

	// Ingest OUTPUT artifacts into project memory (async-safe — errors are logged).
	e.ingestOutputArtifacts(ctx, task, execution)

	// Deterministic bulk-ingest: for workflows that opt in via
	// ingest_input_artifacts, deposit the task's STAGED INPUT artifacts
	// directly into RAG — no agent copy loop (the hardened
	// companion-rag-ingest path). Gated on the workflow flag so ordinary
	// tasks with uploaded attachments are NOT auto-ingested.
	if e.workflows != nil && execution.WorkflowID != "" {
		if wf := e.workflows.GetWorkflow(execution.WorkflowID); wf != nil && wf.IngestInputArtifacts {
			e.ingestInputArtifacts(ctx, task, execution)
		}
	}

	// Ingest structured trading activity into project memory so the
	// next tick's strategist memory_search has rich context (fills,
	// skips, cancels) without depending on the LLM correctly
	// summarising its own placed/skipped arrays. Only fires when the
	// task ran a trading workflow that produced an executor result.
	e.ingestTradingActivity(ctx, task, execution, result)

	// Extract message from result for a meaningful notification.
	notifyMsg := "Task completed successfully"
	if len(result) > 0 {
		var r struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(result, &r) == nil && r.Message != "" {
			notifyMsg = r.Message
		}
	}

	// Notify watchers
	if e.notifier != nil {
		e.notifier.NotifyTaskCompleted(ctx, task, true, notifyMsg)
	} else {
		e.logger.Warn().Str("task_id", task.ID).Msg("no completion notifier configured — skipping notification")
	}

	// Phase 3: fire the LLM-as-judge async if the project opts
	// in. Done after notify so the judge's latency doesn't
	// delay user-facing telegram messages. Detached context
	// because the parent ctx may already be in shutdown by the
	// time the judge LLM call returns.
	e.fireJudgeIfEnabled(task)
}

// fireJudgeIfEnabled launches the Phase 3 LLM-as-judge runner
// for this task in a goroutine when the project opts in. No-op
// when no runner is wired or the project hasn't enabled it.
// Detached context with a 90s budget — judges are interactive,
// not background work, but we cap so a stuck LLM doesn't pin
// the goroutine forever.
//
// Every early-exit logs the reason at debug level (and skip
// reason as a structured field) so operators can see whether
// the judge is silent because it ran-and-passed vs. silent
// because something is misconfigured. Pre-2026-05-04 the
// function was completely silent on every skip path, which
// made "no verdicts in the panel" indistinguishable from
// "the judge code path was never entered."
func (e *Executor) fireJudgeIfEnabled(task *persistence.Task) {
	if task == nil {
		return
	}
	if e.judgeRunner == nil {
		e.logger.Debug().Str("task_id", task.ID).Str("skip_reason", "no_runner").
			Msg("judge: skipped — no runner wired (chat client unavailable at executor build time?)")
		return
	}
	if e.workflows == nil {
		e.logger.Debug().Str("task_id", task.ID).Str("skip_reason", "no_workflow_resolver").
			Msg("judge: skipped — workflow resolver not wired")
		return
	}
	getter, ok := e.workflows.(interface {
		GetProject(string) *registry.Project
	})
	if !ok {
		e.logger.Debug().Str("task_id", task.ID).Str("skip_reason", "resolver_no_GetProject").
			Msg("judge: skipped — workflow resolver does not implement GetProject")
		return
	}
	p := getter.GetProject(task.ProjectID)
	if p == nil {
		e.logger.Debug().Str("task_id", task.ID).Str("project_id", task.ProjectID).
			Str("skip_reason", "project_not_found").
			Msg("judge: skipped — project not in registry (deleted? config reload race?)")
		return
	}
	if !p.HallucinationJudge.Enabled {
		e.logger.Debug().Str("task_id", task.ID).Str("project_id", task.ProjectID).
			Str("skip_reason", "judge_not_enabled").
			Msg("judge: skipped — project's HallucinationJudge.Enabled is false")
		return
	}
	e.logger.Info().Str("task_id", task.ID).Str("project_id", task.ProjectID).
		Str("status", string(task.Status)).
		Msg("judge: firing async runner for terminal task")
	go func() {
		jctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if err := e.judgeRunner.Run(jctx, task); err != nil {
			e.logger.Warn().Err(err).Str("task_id", task.ID).Msg("judge: runner returned error")
		}
	}()
}

// ingestOutputArtifacts reads OUTPUT-class artifacts for this execution and
// ingests their content into project memory. Skips *-response.md files (execution
// transcripts). Errors are logged but do not affect task completion.
func (e *Executor) ingestOutputArtifacts(ctx context.Context, task *persistence.Task, execution *persistence.Execution) {
	if e.memoryIndexer == nil {
		return
	}

	execID := execution.ID
	artifacts, err := e.artifactRepo.List(ctx, persistence.ArtifactFilter{
		ExecutionID: &execID,
		PageSize:    200,
	})
	if err != nil {
		e.logger.Warn().Err(err).
			Str("task_id", task.ID).
			Msg("memory: failed to list artifacts for ingestion")
		return
	}

	// Migration 75/76: extract repo_scope from the task payload once.
	// The companion delegate() stamps it into task.payload["repo_scope"]
	// when the host LLM (or its plugin) passed it; non-companion
	// workflows leave it absent → empty string → no-op downstream.
	repoScope := extractRepoScopeFromPayload(task.Payload)

	for _, a := range artifacts {
		if a == nil {
			continue
		}
		if a.ArtifactClass != persistence.ArtifactClassOutput {
			continue
		}
		// Skip execution transcript artifacts (e.g. plan-response.md
		// or its post-2026-05-15 disambiguated form
		// route-response-20260515-0f96.md). See isTranscriptArtifact.
		if isTranscriptArtifact(a.Name) {
			continue
		}
		// Only ingest markdown files.
		if !strings.HasSuffix(a.Name, ".md") {
			continue
		}
		if a.StoragePath == "" {
			continue
		}

		// Route through artifactStore.Retrieve so the read works
		// under both the LocalBackend (filesystem) and S3 Backend.
		// Falling back to artifactStore == nil keeps the legacy
		// direct-disk path alive for tests that don't wire a Store.
		var content []byte
		if e.artifactStore != nil {
			content, err = e.artifactStore.Retrieve(ctx, a.ID)
		} else {
			content, err = os.ReadFile(a.StoragePath)
		}
		if err != nil {
			e.logger.Warn().Err(err).
				Str("artifact_id", a.ID).
				Str("path", a.StoragePath).
				Msg("memory: failed to read artifact for ingestion")
			continue
		}
		if len(content) == 0 {
			continue
		}

		// Phase 1 (memory hardening): route through the ingest
		// queue when wired. The IngestWorker drains and calls
		// IngestText asynchronously. Nil-queue deployments keep
		// the legacy synchronous path so old configs Just Work.
		if e.ingestQueue != nil {
			execID := execution.ID
			// Resolve the workflow once per execution so the role
			// helper can look up step.Role for workflows that don't
			// use the dev-pipeline `plan_<n>_<role>` step-ID
			// convention (research, plan-and-write, adaptive,
			// trading, …). Nil resolver / unknown workflow falls
			// back to the old stepIDToRole-only behaviour.
			var wf *registry.Workflow
			if e.workflows != nil && execution.WorkflowID != "" {
				wf = e.workflows.GetWorkflow(execution.WorkflowID)
			}
			var scopePtr *string
			if repoScope != "" {
				s := repoScope
				scopePtr = &s
			}
			item := &persistence.IngestQueueItem{
				ProjectID:          task.ProjectID,
				SourceArtifactID:   a.ID,
				ProducerRole:       producerRoleForExecution(execution, wf),
				IngestExecutionID:  &execID,
				Priority:           50, // default; Phase 2 derives from class
				ProposedConfidence: 0.5,
				RepoScope:          scopePtr,
			}
			if err := e.ingestQueue.Enqueue(ctx, item); err != nil {
				e.logger.Warn().Err(err).
					Str("artifact_id", a.ID).
					Str("source_name", a.Name).
					Msg("memory: failed to enqueue artifact for ingest — falling back to synchronous indexer")
				// Surface the fallback as a metric so dashboards
				// see what was previously only in the daemon log.
				// The sync path bypasses Phase 2 pipeline gates
				// and quarantine routing — operators need to know
				// when it kicks in so a transient outage doesn't
				// quietly become a permanent regression.
				if e.ingestEnqueueFallbackRecorder != nil {
					e.ingestEnqueueFallbackRecorder(task.ProjectID)
				}
				// Fall through to the synchronous path so the
				// chunk still lands; better-than-nothing on a
				// transient enqueue error.
			} else {
				continue
			}
		}

		artifactID := a.ID
		if err := e.memoryIndexer.IngestText(ctx, task.ProjectID, task.ID, artifactID, a.Name, string(content)); err != nil {
			e.logger.Warn().Err(err).
				Str("artifact_id", a.ID).
				Str("source_name", a.Name).
				Msg("memory: failed to ingest artifact")
			continue
		}
		// Migration 75: stamp repo_scope on the just-landed chunks so
		// scoped recall finds them under the right partition. Empty
		// scope = no-op at the indexer layer.
		if repoScope != "" {
			if err := e.memoryIndexer.PatchScopeByArtifact(ctx, task.ProjectID, artifactID, repoScope); err != nil {
				e.logger.Warn().Err(err).
					Str("artifact_id", a.ID).
					Str("repo_scope", repoScope).
					Msg("memory: failed to stamp repo_scope on chunks (ingest succeeded)")
			}
		}
	}
}

// inputArtifactRef pairs a staged input artifact's store ID with its
// best-effort display name (storage-path basename) for the sync ingest
// fallback. The enqueue path needs only the ID — the worker reads the
// name from the artifact row.
type inputArtifactRef struct {
	ID   string
	Name string
}

// inputArtifactRefsFromPayload reads the API-folded
// context.inputArtifactIDs (and the parallel context.inputFiles storage
// paths) out of the task payload. Returns nil when no input artifacts
// were staged.
func inputArtifactRefsFromPayload(payload []byte) []inputArtifactRef {
	if len(payload) == 0 {
		return nil
	}
	var p struct {
		Context struct {
			InputArtifactIDs []string `json:"inputArtifactIDs"`
			InputFiles       []string `json:"inputFiles"`
		} `json:"context"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil
	}
	ids := p.Context.InputArtifactIDs
	if len(ids) == 0 {
		return nil
	}
	refs := make([]inputArtifactRef, 0, len(ids))
	for i, id := range ids {
		if strings.TrimSpace(id) == "" {
			continue
		}
		name := ""
		if i < len(p.Context.InputFiles) {
			name = filepath.Base(p.Context.InputFiles[i])
		}
		refs = append(refs, inputArtifactRef{ID: id, Name: name})
	}
	return refs
}

// ingestInputArtifacts deterministically deposits the task's STAGED
// INPUT artifacts into project RAG memory — directly, with no agent in
// the copy loop. It runs from handleSuccess only for workflows that set
// ingest_input_artifacts: true (see registry.Workflow); the gate is
// load-bearing — without it, every task carrying uploaded attachments
// (Telegram / email / research inputs) would be dumped into RAG.
//
// This is the hardened bulk-ingest path. The legacy companion-rag-ingest
// workflow had a weak `rag-ingester` LLM `cp` each artifacts/in/<file>
// to artifacts/out/ for ingestOutputArtifacts to commit; the model
// routinely claimed produced_files it never wrote (round-tripping large
// files through its context, or just hallucinating success), failing the
// run and forcing the operator to retry / "try a different tool" (the
// 2026-06 ingest incidents). Here the executor enqueues each input
// artifact by ID — same pipeline, same repo_scope stamping as
// ingestOutputArtifacts — so file size and model quality are irrelevant.
func (e *Executor) ingestInputArtifacts(ctx context.Context, task *persistence.Task, execution *persistence.Execution) {
	if e.memoryIndexer == nil {
		return
	}
	refs := inputArtifactRefsFromPayload(task.Payload)
	if len(refs) == 0 {
		return
	}

	repoScope := extractRepoScopeFromPayload(task.Payload)
	var scopePtr *string
	if repoScope != "" {
		s := repoScope
		scopePtr = &s
	}
	var wf *registry.Workflow
	if e.workflows != nil && execution.WorkflowID != "" {
		wf = e.workflows.GetWorkflow(execution.WorkflowID)
	}
	producer := producerRoleForExecution(execution, wf)
	if producer == "" {
		producer = "rag-ingester"
	}
	execID := execution.ID

	committed, failed := 0, 0
	for _, ref := range refs {
		// Preferred: enqueue by artifact ID. The IngestWorker retrieves
		// the stored bytes and runs IngestText asynchronously, carrying
		// repo_scope through to chunk stamping — identical to the output
		// path. File bytes never touch this process.
		if e.ingestQueue != nil {
			item := &persistence.IngestQueueItem{
				ProjectID:          task.ProjectID,
				SourceArtifactID:   ref.ID,
				ProducerRole:       producer,
				IngestExecutionID:  &execID,
				Priority:           50,
				ProposedConfidence: 0.5,
				RepoScope:          scopePtr,
			}
			if err := e.ingestQueue.Enqueue(ctx, item); err != nil {
				e.logger.Warn().Err(err).Str("artifact_id", ref.ID).
					Msg("memory: failed to enqueue input artifact — falling back to synchronous indexer")
				if e.ingestEnqueueFallbackRecorder != nil {
					e.ingestEnqueueFallbackRecorder(task.ProjectID)
				}
				// fall through to the synchronous path
			} else {
				committed++
				continue
			}
		}

		// Synchronous fallback (no queue wired, or enqueue failed):
		// retrieve the stored bytes by ID and ingest directly.
		if e.artifactStore == nil {
			failed++
			continue
		}
		content, err := e.artifactStore.Retrieve(ctx, ref.ID)
		if err != nil || len(content) == 0 {
			if err != nil {
				e.logger.Warn().Err(err).Str("artifact_id", ref.ID).
					Msg("memory: failed to read input artifact for ingest")
			}
			failed++
			continue
		}
		sourceName := ref.Name
		if sourceName == "" {
			sourceName = ref.ID
		}
		if err := e.memoryIndexer.IngestText(ctx, task.ProjectID, task.ID, ref.ID, sourceName, string(content)); err != nil {
			e.logger.Warn().Err(err).Str("artifact_id", ref.ID).Str("source_name", sourceName).
				Msg("memory: input artifact IngestText failed")
			failed++
			continue
		}
		if repoScope != "" {
			if err := e.memoryIndexer.PatchScopeByArtifact(ctx, task.ProjectID, ref.ID, repoScope); err != nil {
				e.logger.Warn().Err(err).Str("artifact_id", ref.ID).Str("repo_scope", repoScope).
					Msg("memory: failed to stamp repo_scope on input chunks (ingest succeeded)")
			}
		}
		committed++
	}

	e.logger.Info().
		Str("task_id", task.ID).
		Str("workflow_id", execution.WorkflowID).
		Int("committed", committed).
		Int("failed", failed).
		Int("total", len(refs)).
		Str("repo_scope", repoScope).
		Msg("memory: deterministic input-artifact ingest")
}

// extractRepoScopeFromPayload pulls repo_scope out of the task
// payload's nested `context` block. The companion delegate handler
// (companion_mcp.go) stamps it on the payload map under
// payload.context.repo_scope (taskcreate.Creator wraps RawContext
// under the "context" key when building the final payload).
//
// Returns empty string on missing / decode error / whitespace,
// which is the no-op signal at the indexer layer. We also accept
// the legacy unnested shape (`payload.repo_scope`) for forward
// compat with future callers that bypass the context wrapper.
func extractRepoScopeFromPayload(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var p struct {
		RepoScope string `json:"repo_scope"`
		Context   struct {
			RepoScope string `json:"repo_scope"`
		} `json:"context"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	if s := strings.TrimSpace(p.Context.RepoScope); s != "" {
		return s
	}
	return strings.TrimSpace(p.RepoScope)
}

// producerRoleForExecution best-effort extracts the producing role
// from an execution's completed steps. Two recognition strategies,
// tried in order per step:
//
//  1. stepIDToRole — the dev-pipeline convention
//     `plan_<index>_<role>[_retry_suffix]`. Works for dynamic plans.
//
//  2. Workflow-step lookup — `wf.Steps[base].Role`, where `base` is
//     the step ID with known retry suffixes stripped. Catches the
//     workflows that use semantic step names (`research`, `write`,
//     `route`, `plan`, etc.) — research, plan-and-write, adaptive,
//     trading, simple-workflow. Pre-2026-05-15 these all returned
//     empty role → unclassified at ingest; the assistant project
//     accumulated 79 such chunks.
//
// Walks completed steps newest-to-oldest and returns the first hit
// from a non-routing/non-gating role. The earlier "skip lead and
// feasibility" filter was lifted: legitimate lead-produced
// artifacts (autonomy digests, lead-authored summaries) need a role
// stamp too, and the per-artifact transcript filter
// (isTranscriptArtifact in artifacts.go) handles dev-pipeline's
// plan-response.md transcript exclusion at the source-name layer
// where it belongs.
//
// Empty-string return means "could not determine the producing
// role" — caller stamps producer_role=NULL on the chunk, which
// classify_by_role downstream treats as unclassified. The previous
// fallback string ("executor") collided with the ibkr-trader swarm's
// real `executor` role and caused 224 chunks in the assistant
// project to be misclassified as `commit_msg`; returning "" keeps
// the two meanings distinct.
func producerRoleForExecution(execution *persistence.Execution, wf *registry.Workflow) string {
	if execution == nil {
		return ""
	}
	for i := len(execution.CompletedSteps) - 1; i >= 0; i-- {
		stepID := execution.CompletedSteps[i]
		if role := stepIDToRole(stepID); role != "" {
			return role
		}
		if wf == nil {
			continue
		}
		base := stripRetryStepSuffix(stepID)
		if step, ok := wf.Steps[base]; ok && step.Role != "" {
			return step.Role
		}
	}
	return ""
}

// stripRetryStepSuffix removes the known retry-step suffixes the
// executor appends to a base step ID when re-running with different
// parameters. Keeps the canonical step ID so producerRoleForExecution
// can look it up in the workflow definition (which doesn't list the
// synthetic retry IDs). Suffixes covered:
//
//	_shape_retry     — retry.go's shape-violation retry
//	_model_fallback  — retry.go's model-fallback retry
//	_infra_retry<N>  — retry.go's transient-failure retry
//	_refusal_retry   — plan_step.go's refusal retry
//	_route_retry     — workflow.go's strict-adaptive corrective retry
//
// All known suffixes start with `_` and never re-occur in the base
// step IDs we ship, so right-trimming the first matching suffix is
// unambiguous.
func stripRetryStepSuffix(stepID string) string {
	for _, suffix := range []string{"_shape_retry", "_model_fallback", "_refusal_retry", "_route_retry"} {
		if strings.HasSuffix(stepID, suffix) {
			return strings.TrimSuffix(stepID, suffix)
		}
	}
	// _infra_retry<N> has a variable trailing integer — strip by
	// finding the prefix then dropping the rest.
	if idx := strings.Index(stepID, "_infra_retry"); idx > 0 {
		return stepID[:idx]
	}
	return stepID
}

// stepIDToRole extracts the role suffix from a `plan_<n>_<role>`
// step ID. Returns "" when the ID doesn't match the convention.
// Trailing variants (_shape_retry, _model_fallback, _infra_retry1)
// are stripped so the role name is clean.
func stepIDToRole(stepID string) string {
	parts := strings.Split(stepID, "_")
	if len(parts) < 3 {
		return ""
	}
	if parts[0] != "plan" {
		return ""
	}
	// plan_<index>_<role>[...]. The role token is parts[2]; trailing
	// parts are retry/variant suffixes the executor appends.
	role := parts[2]
	// Special cases the executor uses verbatim as step IDs:
	//   plan_lead_lead, plan_lead_lead_shape_retry → role="lead"
	if parts[1] == "lead" {
		return "lead"
	}
	return role
}

// ingestTradingActivity writes a structured memory chunk
// summarising the executor step's placements + skips +
// observed fills, so the next tick's strategist memory_search
// surfaces concrete context like "ibkr-trader recent fills →
// 2026-05-04 13:40 SAP filled 1@$172.05; 2026-05-04 13:42 TSM
// cancelled (cancel_reason: market_moved)" rather than
// returning empty results.
//
// Bridges the Phase-3 fill-ingestion gap: until a real fills
// poller lands, the executor's terminal output is the
// authoritative record of what happened. Best-effort —
// memoryIndexer-nil and parse-failure paths are silent
// no-ops, never failing the task.
//
// Skips when the result doesn't carry the executor's tri-array
// shape (placed / skipped / fills_observed) — non-trading
// workflows produce different result shapes and shouldn't
// land here.
func (e *Executor) ingestTradingActivity(ctx context.Context, task *persistence.Task, execution *persistence.Execution, result []byte) {
	if e.memoryIndexer == nil || len(result) == 0 {
		return
	}
	var r struct {
		Placed []struct {
			Symbol         string `json:"symbol"`
			BrokerOrderID  string `json:"broker_order_id"`
			Status         string `json:"status"`
			IdempotencyKey string `json:"idempotency_key"`
		} `json:"placed"`
		Skipped []struct {
			Symbol       string `json:"symbol"`
			Reason       string `json:"reason"`
			Detail       string `json:"detail"`
			CancelReason string `json:"cancel_reason"`
			CancelDetail string `json:"cancel_detail"`
		} `json:"skipped"`
		FillsObserved []struct {
			Symbol string  `json:"symbol"`
			Qty    float64 `json:"qty"`
			Price  float64 `json:"price"`
		} `json:"fills_observed"`
	}
	if err := json.Unmarshal(result, &r); err != nil {
		return
	}
	// Empty arrays = nothing to record. Quiet when the tick
	// produced no signal-bearing activity (closed-market,
	// no-approvals fast path, etc.).
	if len(r.Placed) == 0 && len(r.Skipped) == 0 && len(r.FillsObserved) == 0 {
		return
	}

	stamp := time.Now().UTC().Format("2006-01-02 15:04 UTC")
	var sb strings.Builder
	sb.WriteString("# Trading activity ")
	sb.WriteString(stamp)
	sb.WriteString("\n\n")

	if len(r.FillsObserved) > 0 {
		sb.WriteString("## Fills observed\n")
		for _, f := range r.FillsObserved {
			fmt.Fprintf(&sb, "- %s: %.4f @ $%.2f\n", f.Symbol, f.Qty, f.Price)
		}
		sb.WriteString("\n")
	}
	if len(r.Placed) > 0 {
		sb.WriteString("## Orders placed\n")
		for _, p := range r.Placed {
			fmt.Fprintf(&sb, "- %s: status=%s broker_id=%s\n", p.Symbol, p.Status, p.BrokerOrderID)
		}
		sb.WriteString("\n")
	}
	if len(r.Skipped) > 0 {
		sb.WriteString("## Skipped / cancelled\n")
		for _, s := range r.Skipped {
			line := fmt.Sprintf("- %s: %s", s.Symbol, s.Reason)
			if s.Detail != "" {
				line += " (" + s.Detail + ")"
			}
			if s.CancelReason != "" {
				line += " · cancel_reason=" + s.CancelReason
				if s.CancelDetail != "" {
					line += " (" + s.CancelDetail + ")"
				}
			}
			sb.WriteString(line)
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}
	sb.WriteString("Source: executor step `")
	sb.WriteString(execution.ID)
	sb.WriteString("`. Strategist's next-tick memory_search for \"ibkr-trader recent fills\" / \"ibkr-trader stopped out\" should find this entry.\n")

	// sourceName carries the execution ID so multiple ticks per
	// project don't collide on the chunks' deterministic ID. The
	// indexer hashes the content body, so identical activity (rare
	// — placement IDs differ per call) lands as a single chunk.
	sourceName := "trading-activity-" + execution.ID + ".md"
	if err := e.memoryIndexer.IngestText(ctx, task.ProjectID, task.ID, "", sourceName, sb.String()); err != nil {
		e.logger.Warn().Err(err).
			Str("task_id", task.ID).
			Str("execution_id", execution.ID).
			Msg("memory: failed to ingest trading activity")
	}
}

// buildRecentActivityBlock returns a compact markdown block
// summarising the last 24h of trading_orders for a project.
// Surfaces filled / submitted / cancelled / refused rows so
// the strategist's next-tick prompt sees concrete history
// without depending on the LLM correctly invoking
// memory_search.
//
// Returns "" when:
//   - the repo isn't wired (executor used in non-trading
//     deployments);
//   - the repo errors (best-effort — strategist still runs);
//   - the project has no rows in the window (typical first
//     tick after a fresh deploy).
//
// Capped at 20 rows so a busy day's output stays under the
// gateway's request size envelope. Newest first.
func (e *Executor) buildRecentActivityBlock(ctx context.Context, projectID string) string {
	if e.tradingOrderRepo == nil || projectID == "" {
		return ""
	}
	since := time.Now().UTC().Add(-24 * time.Hour)
	pid := projectID
	rows, err := e.tradingOrderRepo.List(ctx, persistence.TradingOrderFilter{
		ProjectID: &pid,
		Since:     &since,
		PageSize:  20,
	})
	if err != nil || len(rows) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("Last 24h of broker activity for this project (newest first). Use this to apply cooldown rules and avoid revenge-trading symbols that just stopped out.\n\n")
	for _, o := range rows {
		if o == nil {
			continue
		}
		// One line per row. Keep it terse — the strategist
		// reads this alongside its main prompt and we want it
		// scannable, not narrative.
		ts := o.SubmittedAt.UTC().Format("2006-01-02 15:04Z")
		px := ""
		if o.LimitPrice != nil && *o.LimitPrice > 0 {
			px = fmt.Sprintf(" @ $%.2f", *o.LimitPrice)
		}
		stop := ""
		if o.StopPrice != nil && *o.StopPrice > 0 {
			stop = fmt.Sprintf(" stop $%.2f", *o.StopPrice)
		}
		reason := ""
		if o.LastStatusReason != "" {
			reason = " · " + o.LastStatusReason
		}
		fmt.Fprintf(&sb, "- %s %s %s qty=%.4f%s%s status=%s%s\n",
			ts, o.Symbol, o.Action, o.Qty, px, stop, o.Status, reason)
	}
	return sb.String()
}

// handleFailure processes a failed execution.
func (e *Executor) handleFailure(ctx context.Context, task *persistence.Task, execution *persistence.Execution, err error) {
	// Operator-pause guard. If the task has been flipped to PAUSED
	// between the goroutine starting and reaching the failure
	// finaliser (typical case: pause arrived after agent steps
	// completed but before the merge step ran, then merge failed),
	// honour the operator's intent. Writing FAILED here would
	// overwrite PAUSED in the DB, mask the operator action in the
	// UI, and force the task back to terminal status.
	//
	// Re-fetch the task because the in-memory `task` argument was
	// snapshotted when the goroutine started — pre-pause. A live
	// DB read sees the operator's update.
	//
	// Live evidence: T-…1c44 (2026-05-23) — operator paused at
	// 17:02:33; without this guard the merge step's handleFailure
	// at 17:02:57 silently demoted PAUSED to FAILED.
	if fresh, ferr := e.taskRepo.Get(context.Background(), task.ID); ferr == nil &&
		fresh != nil && fresh.Status == persistence.TaskStatusPaused {
		e.logger.Info().
			Str("task_id", task.ID).
			Str("execution_id", execution.ID).
			Msg("handleFailure: task already PAUSED by operator — skipping terminal status write")
		return
	}

	// Calculate duration
	duration := time.Since(e.getStartTime(task.ID))

	errorMsg := "unknown error"
	if err != nil {
		errorMsg = err.Error()
	}

	// Classify the failure so operators get a typed tag alongside the
	// freeform message. ClassifyExecutionFailure is defensive — returns
	// UNKNOWN rather than guessing when the signal is ambiguous.
	errorClass := ClassifyExecutionFailure(err, errorMsg)

	// Sweep any remaining pending_validation rows to "ok": if a step's
	// output had been bad, the consumer site (plan_step.go, gate eval)
	// would have finalized it explicitly before the failure propagated.
	// Pending rows at this point were produced but never rejected, so
	// marking them "ok" keeps downstream execution failures from
	// contaminating the producer's quality metric.
	e.sweepPendingOutcomes(ctx, execution.ID, string(stepoutcome.OK))

	e.logger.Warn().
		Str("task_id", task.ID).
		Str("execution_id", execution.ID).
		Str("project_id", task.ProjectID).
		Str("workflow_id", execution.WorkflowID).
		Dur("duration", duration).
		Str("error", errorMsg).
		Str("error_class", errorClass).
		Strs("completed_steps", execution.CompletedSteps).
		Msg("execution failed")

	// Update execution record. error_code on executions has been typed
	// since the beginning; reuse the same value so execution-level and
	// task-level classifiers always agree.
	execErrorCode := errorClass
	if execErrorCode == "" {
		execErrorCode = "EXECUTION_ERROR"
	}
	_ = e.execRepo.RecordFailure(ctx, execution.ID, errorMsg, execErrorCode)

	// Persist LastError + LastErrorClass for UI / CLI / `vornikctl task
	// list --class X` filtering — these reflect the LATEST attempt's
	// failure, regardless of whether more retries are coming.
	//
	// Status is conditional. When the task has retry budget remaining
	// (handled by scheduler.TaskCompleted's ReleaseLease → TaskStatusQueued),
	// the executor MUST NOT pre-flip Status to FAILED. Pre-fix the task
	// briefly showed FAILED in the UI between handleFailure and
	// ReleaseLease — confusing operators ("the task failed!") and, more
	// dangerously, stranding the task as terminal-FAILED if anything
	// disrupted the handoff (daemon restart, ReleaseLease DB blip,
	// task lookup failure inside TaskCompleted's defensive branch).
	// Leaving Status alone keeps the task LEASED until the scheduler
	// makes the retry decision; the persistent visible-FAILED window
	// disappears.
	task.LastError = &errorMsg
	if errorClass != "" {
		task.LastErrorClass = &errorClass
	}
	if !e.taskWillRetry(task) {
		// Diagnostic for stability item 3 ("tasks finalize FAILED
		// after one attempt"). Logging which decision path triggered
		// terminal-FAILED — and what the attempt budget looked like
		// — gives operators concrete data when they next see a task
		// land FAILED unexpectedly early.
		e.logger.Warn().
			Str("task_id", task.ID).
			Str("execution_id", execution.ID).
			Int("task_attempt", task.Attempt).
			Int("task_max_attempts", task.MaxAttempts).
			Str("error_class", errorClass).
			Str("decision_path", "handleFailure.terminal").
			Msg("task: terminal FAILED — retry budget exhausted")
		task.Status = persistence.TaskStatusFailed
	}
	_ = e.taskRepo.Update(ctx, task)
	// Settle the budget reservation only on the TERMINAL failure (retry
	// exhausted). A task that will retry keeps its reservation — its spend
	// is still in flight.
	if task.Status == persistence.TaskStatusFailed {
		e.settleBudgetReservation(ctx, task.ID)
	}
	// Inter-project resolve: failed-side. Skips when the task
	// isn't a CPC callee.
	e.resolveCrossProjectCallForTask(ctx, task, false)

	// Record metrics
	if e.metrics != nil {
		e.metrics.RecordFailed(task.ProjectID, duration.Seconds())
	}

	// Check if this task is a child — unblock parent if all children are done.
	e.checkParentUnblock(ctx, task)

	// Per-project circuit breaker: if the project has now accumulated
	// `threshold` failures within `window`, pause autonomy on it and
	// alert the operator. Skipped when the task will retry (the
	// failure isn't terminal yet — flipping the breaker on a
	// transient is the wrong signal). Skipped when the breaker
	// isn't wired (the default for upgrades).
	if e.circuitBreaker != nil && !e.taskWillRetry(task) {
		e.circuitBreaker.Trip(ctx, task, errorClass)
	}

	// Notify watchers — only when the TASK is in its terminal state.
	// task.MaxAttempts > 0 && task.Attempt < task.MaxAttempts means the
	// scheduler will re-queue this task for another execution attempt
	// (see scheduler.TaskCompleted). Firing the Telegram notification
	// from a non-final execution would (a) send a misleading "task
	// failed" message before the retry runs and (b) wipe the watcher
	// list (telegram bot.NotifyTaskCompleted unconditionally calls
	// RemoveWatchers after sending), so the eventual success on retry
	// would have nobody to tell. Defer the announcement until the
	// retry budget is actually exhausted.
	if !e.taskWillRetry(task) {
		if e.notifier != nil {
			e.notifier.NotifyTaskCompleted(ctx, task, false, errorMsg)
		} else {
			e.logger.Warn().Str("task_id", task.ID).Msg("no completion notifier configured — skipping notification")
		}

		// Phase 3 LLM-as-judge — fire on TERMINAL failures too, not
		// just success. Without this, tasks whose Phase 1 detector
		// produced High-severity signals (and therefore failed every
		// retry) never get a Phase 3 verdict — the operator sees
		// many Phase 1 signals on the panel but the verdict tile
		// stays empty, making the layer look broken when in fact
		// the judge was simply never called. The judge tolerates
		// thin evidence (artifacts may be empty, last_result_text
		// may be a stack trace) — abstaining is a valid outcome
		// and is more useful than no row at all.
		// Gated on !taskWillRetry so we don't fire once per attempt,
		// matching the notifier policy above.
		e.fireJudgeIfEnabled(task)
	} else {
		e.logger.Debug().
			Str("task_id", task.ID).
			Int("attempt", task.Attempt).
			Int("max_attempts", task.MaxAttempts).
			Msg("execution failure: deferring task notification — scheduler will retry")
	}
}

// taskWillRetry reports whether the scheduler is going to re-queue this
// task for another execution attempt after the current one fails.
// Mirrors the predicate in scheduler.TaskCompleted (task.Attempt <
// task.MaxAttempts) so the notification gating here stays in sync with
// the actual retry decision. MaxAttempts == 0 means retries are
// disabled — treat as "this is the only attempt".
func (e *Executor) taskWillRetry(task *persistence.Task) bool {
	if task == nil || task.MaxAttempts <= 0 {
		return false
	}
	return task.Attempt < task.MaxAttempts
}

func (e *Executor) handleCancelled(ctx context.Context, task *persistence.Task, execution *persistence.Execution) {
	duration := time.Since(e.getStartTime(task.ID))

	// Cancelled executions still produced output up to the cancel point;
	// sweep with "cancelled" so the producer steps aren't wrongly marked
	// "ok" when the run was actually aborted mid-flight.
	e.sweepPendingOutcomes(ctx, execution.ID, string(stepoutcome.Cancelled))

	_ = e.execRepo.UpdateStatus(ctx, execution.ID, persistence.ExecutionStatusCancelled)
	_ = e.taskRepo.UpdateStatus(ctx, task.ID, persistence.TaskStatusCancelled)
	task.Status = persistence.TaskStatusCancelled
	e.settleBudgetReservation(ctx, task.ID)
	e.cascadeOrphanExecutions(ctx, task.ID)
	// Inter-project resolve: cancelled-side. Same shape as
	// the failed branch — callee cancellation translates to
	// CPC=failed so the caller's on_fail fires.
	e.resolveCrossProjectCallForTask(ctx, task, false)

	if e.notifier != nil {
		e.notifier.NotifyTaskCompleted(ctx, task, false, "Task cancelled")
	} else {
		e.logger.Warn().Str("task_id", task.ID).Msg("no completion notifier configured — skipping notification")
	}

	// Cancellation is a terminal status like success/failure, so it
	// must drive the parent-unblock sweep too. Regression context
	// 2026-06-07: this call was missing (handleSuccess/handleFailure
	// both had it), so a WAITING_FOR_CHILDREN parent whose last child
	// was cancelled waited forever until manually cancelled.
	e.checkParentUnblock(ctx, task)

	e.logger.Info().
		Str("task_id", task.ID).
		Str("execution_id", execution.ID).
		Str("project_id", task.ProjectID).
		Dur("duration", duration).
		Msg("execution cancelled")
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func lastStepOrUnknown(completedSteps []string) string {
	if len(completedSteps) > 0 {
		return completedSteps[len(completedSteps)-1]
	}
	return "unknown"
}

// cascadeOrphanExecutions closes any non-terminal execution rows
// hanging off a task that just reached a terminal status. The
// adaptive-route flow runs two executions per task: the first
// reaches PAUSED with pause_reason=awaiting_children and never
// finalises, even after the second drives the task to COMPLETED.
// Without this sweep those PAUSED rows accumulate (29 on one
// project as of 2026-05-22) and block config reload's safety
// check.
//
// Best-effort: a DB error here is logged but doesn't fail the
// terminal transition. The config-reload safety check already
// has a fallback that skips PAUSED+terminal-task pairs, so a
// missed cascade only means the orphan lingers — not that
// reloads break.
func (e *Executor) cascadeOrphanExecutions(ctx context.Context, taskID string) {
	if e.execRepo == nil || taskID == "" {
		return
	}
	n, err := e.execRepo.SupersedeNonTerminalForTask(ctx, taskID)
	if err != nil {
		e.logger.Warn().
			Err(err).
			Str("task_id", taskID).
			Msg("cascade-orphan-executions: sweep failed; orphan rows may linger until manual cleanup")
		return
	}
	if n > 0 {
		e.logger.Info().
			Str("task_id", taskID).
			Int64("orphans_swept", n).
			Msg("cascade-orphan-executions: superseded non-terminal executions")
	}
}

// shouldRetry determines if an error should trigger a retry.
func (e *Executor) shouldRetry(err error) bool {
	if err == nil {
		return false
	}

	// Don't retry on context cancellation
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var transient retryableError
	return errors.As(err, &transient)
}

// checkParentUnblock checks whether this task's parent can be unblocked.
// If all children of the parent are in a terminal state, the parent is moved
// back to QUEUED so the scheduler can resume it.
//
// Known caveat (stability item 3): this path bypasses the scheduler's
// retry-budget logic. When a child fails, the parent goes to terminal
// FAILED on its first attempt regardless of `parent.Attempt <
// parent.MaxAttempts`. A behavioral fix (re-queue the parent on
// remaining budget) requires understanding the workflow engine's
// re-attempt semantics for delegating tasks — risky without trace data.
// For now this path emits a structured warning when the bypass happens
// against a parent that DID have retry budget remaining, so the
// "tasks finalize FAILED after a single attempt" observation becomes
// diagnosable in production telemetry.
// synthesizeResumeResult builds the result a delegating parent reports when its
// route/decompose step is skipped on resume (children already spawned + done).
// It surfaces the children's OUTCOMES — what the dispatcher expects back from
// the parent — rather than an internal routing note. Each child's final
// message is read from its completed execution (GetByTaskID). A single child's
// message is passed through verbatim as the parent's message (the common
// single-shot route case); multiple children's messages are joined, and every
// child's outcome is listed under `children`. The result deliberately carries
// no `selected_workflow` / `delegated_tasks`, so the caller's spawn branches
// short-circuit and no child is re-spawned.
func (e *Executor) synthesizeResumeResult(ctx context.Context, children []*persistence.Task) []byte {
	type childOutcome struct {
		TaskID  string `json:"task_id"`
		Status  string `json:"status"`
		Message string `json:"message,omitempty"`
	}
	outcomes := make([]childOutcome, 0, len(children))
	var messages []string
	for _, ch := range children {
		co := childOutcome{TaskID: ch.ID, Status: string(ch.Status)}
		if e.execRepo != nil {
			if ce, err := e.execRepo.GetByTaskID(ctx, ch.ID); err == nil && ce != nil && len(ce.Result) > 0 {
				var r struct {
					Message string `json:"message"`
				}
				if json.Unmarshal(ce.Result, &r) == nil && strings.TrimSpace(r.Message) != "" {
					co.Message = r.Message
					messages = append(messages, r.Message)
				}
			}
		}
		outcomes = append(outcomes, co)
	}
	var msg string
	switch len(messages) {
	case 0:
		// Children carried no parseable message — report status instead of an
		// empty outcome (don't strand the dispatcher with nothing).
		msg = fmt.Sprintf("%d delegated subtask(s) completed", len(children))
	case 1:
		msg = messages[0]
	default:
		msg = strings.Join(messages, "\n\n")
	}
	out, _ := json.Marshal(struct {
		Message  string         `json:"message"`
		Children []childOutcome `json:"children,omitempty"`
	}{Message: msg, Children: outcomes})
	return out
}

func (e *Executor) checkParentUnblock(ctx context.Context, task *persistence.Task) {
	if task.ParentTaskID == nil || *task.ParentTaskID == "" {
		return
	}
	e.unblockParentIfChildrenDone(ctx, *task.ParentTaskID)
}

// unblockParentIfChildrenDone is the parent-wake core shared by
// checkParentUnblock (child-terminal events), NotifyChildTerminal
// (UI close path), and the post-delegation self-heal re-check.
//
// Concurrency (bug-sweep follow-up 2026-06-04): this runs concurrently
// from every child's terminal finaliser, so the last two children of a
// fan-out finishing together both observe allDone and both try to wake
// the parent. The status writes therefore go through
// TransitionConditional gated on WAITING_FOR_CHILDREN — exactly one
// concurrent caller wins; the loser's moved==false means "already
// handled" and is a clean no-op. Pre-fix this was an unconditional
// read-modify-write Update/UpdateStatus, so concurrent callers could
// double-increment parent.Attempt (skipping a retry), re-queue the
// parent twice (double execution), or clobber a concurrent QUEUED
// re-queue with FAILED.
func (e *Executor) unblockParentIfChildrenDone(ctx context.Context, parentID string) {
	parent, err := e.taskRepo.Get(ctx, parentID)
	if err != nil || parent.Status != persistence.TaskStatusWaitingForChildren {
		return
	}

	children, err := e.taskRepo.GetChildren(ctx, parent.ID)
	if err != nil {
		return
	}
	if len(children) == 0 {
		// Nothing to conclude from. A WAITING_FOR_CHILDREN parent
		// whose children aren't visible via GetChildren (e.g. a
		// cross-project callee tracked in the CPC ledger) is woken by
		// its own mechanism — concluding "all done" vacuously here
		// would wrongly re-queue it mid-wait.
		return
	}

	allDone := true
	anyFailed := false
	failedChildIDs := make([]string, 0)
	for _, child := range children {
		switch child.Status {
		case persistence.TaskStatusCompleted,
			persistence.TaskStatusFailed,
			persistence.TaskStatusCancelled,
			persistence.TaskStatusClosed:
			// CLOSED is an operator-confirmed terminal status (Phase 23
			// conversational lifecycle). Treat it like COMPLETED — the
			// child is done by operator decision and the parent should
			// resume. Pre-fix the switch only matched the executor-set
			// terminal statuses, so a parent of an operator-closed child
			// sat in WAITING_FOR_CHILDREN forever
			// (task_20260521111852_8016a4a902b4f959, 2026-05-21).
			if child.Status == persistence.TaskStatusFailed {
				anyFailed = true
				failedChildIDs = append(failedChildIDs, child.ID)
			}
		default:
			allDone = false
		}
	}

	if !allDone {
		return
	}

	if anyFailed {
		// Respect the parent's retry budget — pre-fix this path
		// unconditionally marked the parent FAILED via UpdateStatus,
		// ignoring MaxAttempts entirely. A multi-attempt parent (e.g.
		// MaxAttempts=3) was therefore terminally failed on its first
		// child failure, with no last_error and no chance to retry.
		// The fix mirrors handleFailure: stamp last_error +
		// last_error_class on the SAME conditional UPDATE as the
		// status flip.
		retryBudgetRemaining := parent.MaxAttempts > 0 && parent.Attempt < parent.MaxAttempts
		errMsg := fmt.Sprintf("child task(s) failed: %v", failedChildIDs)
		errClass := string(persistence.TaskFailureClassChildFailed)
		evt := e.logger.Warn().
			Str("parent_task_id", parent.ID).
			Str("project_id", parent.ProjectID).
			Strs("failed_child_task_ids", failedChildIDs).
			Int("parent_attempt", parent.Attempt).
			Int("parent_max_attempts", parent.MaxAttempts).
			Bool("retry_budget_remaining", retryBudgetRemaining).
			Str("decision_path", "checkParentUnblock.anyFailed")
		var moved bool
		if retryBudgetRemaining {
			moved, _ = e.taskRepo.TransitionConditional(ctx, parent.ID,
				[]persistence.TaskStatus{persistence.TaskStatusWaitingForChildren},
				persistence.TaskStatusQueued,
				persistence.TransitionOpts{
					LastError:      &errMsg,
					LastErrorClass: &errClass,
					Attempt:        parent.Attempt + 1,
				})
			evt.Bool("transitioned", moved).Msg("task: parent re-queued for retry after child-failure bubble-up")
		} else {
			moved, _ = e.taskRepo.TransitionConditional(ctx, parent.ID,
				[]persistence.TaskStatus{persistence.TaskStatusWaitingForChildren},
				persistence.TaskStatusFailed,
				persistence.TransitionOpts{
					LastError:      &errMsg,
					LastErrorClass: &errClass,
				})
			evt.Bool("transitioned", moved).Msg("task: parent → terminal FAILED via child-failure bubble-up; retry budget exhausted")
		}
	} else {
		moved, _ := e.taskRepo.TransitionConditional(ctx, parent.ID,
			[]persistence.TaskStatus{persistence.TaskStatusWaitingForChildren},
			persistence.TaskStatusQueued,
			persistence.TransitionOpts{})
		if !moved {
			e.logger.Debug().
				Str("parent_task_id", parent.ID).
				Msg("task: parent unblock lost the race — already handled by a concurrent child finaliser")
		}
	}
}

// NotifyChildTerminal lets a non-executor caller (e.g. the UI close
// path) drive the parent-unblock sweep when a child reaches a
// terminal status outside the executor's own flow. Loads the child
// by ID and delegates to checkParentUnblock — same logic, same
// retry-budget handling, same telemetry. No-op when the child has
// no parent or can't be loaded; the executor's auto-resume path
// will sweep on the next opportunity if anything is missed here.
func (e *Executor) NotifyChildTerminal(ctx context.Context, childTaskID string) {
	if e == nil || e.taskRepo == nil || childTaskID == "" {
		return
	}
	child, err := e.taskRepo.Get(ctx, childTaskID)
	if err != nil || child == nil {
		return
	}
	e.checkParentUnblock(ctx, child)
}
