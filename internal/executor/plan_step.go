package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/playbook"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/stepoutcome"
)

// gitHEAD returns the current HEAD commit SHA for a git repository at dir.
// Returns "" when the directory is not a git repo or the command fails —
// callers use the empty string to mean "skip the HEAD-based check".
func gitHEAD(ctx context.Context, dir string) string {
	if dir == "" {
		return ""
	}
	out, err := gitExec.output(ctx, "-C", dir, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// roleResultClaims carries the machine-readable claims an agent made about
// whether it changed files, committed, ran tests, or reviewed a specific
// commit. These come from the result.json the agent wrote; the executor
// uses them to detect fabricated success — see verifyRoleClaims for the
// cross-checks.
type roleResultClaims struct {
	// claimedCommit is true when the agent's JSON asserts committed: true
	// under any top-level key (implementation, architect, review, etc.).
	claimedCommit bool
	// claimedFilesChanged is the files_changed count the agent reported.
	// 0 means "no claim made"; the verifier doesn't fire on absent values.
	claimedFilesChanged int
	// claimedTestingPassed is set when the result asserts testing.passed.
	// Pointer because absent ≠ false: a researcher role's result.json
	// has no testing block at all and we don't want to spuriously assert
	// it lied about not-passing.
	claimedTestingPassed *bool
	// claimedCheckedCommit is the sha the reviewer says it inspected
	// (review.checked_commit). Verifier confirms the sha exists in the
	// project's git repo so a model that hallucinated a plausible-looking
	// hash gets caught.
	claimedCheckedCommit string
}

// parseRoleClaims scans an agent's result.json for commit / file-change /
// testing / review claims across the known JSON shapes. It tolerates
// unknown shapes and returns zero values when no claim is present.
func parseRoleClaims(data []byte) roleResultClaims {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return roleResultClaims{}
	}
	var claims roleResultClaims
	for _, raw := range envelope {
		var obj struct {
			Committed     *bool  `json:"committed"`
			FilesChanged  *int   `json:"files_changed"`
			Passed        *bool  `json:"passed"`
			CheckedCommit string `json:"checked_commit"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			continue
		}
		if obj.Committed != nil && *obj.Committed {
			claims.claimedCommit = true
		}
		if obj.FilesChanged != nil && *obj.FilesChanged > claims.claimedFilesChanged {
			claims.claimedFilesChanged = *obj.FilesChanged
		}
		if obj.Passed != nil {
			// Last-write-wins across envelope keys; in practice the
			// `testing` key is the only one declaring `passed:` so this
			// never collides. Stays defensive for future role shapes.
			v := *obj.Passed
			claims.claimedTestingPassed = &v
		}
		if obj.CheckedCommit != "" && claims.claimedCheckedCommit == "" {
			claims.claimedCheckedCommit = obj.CheckedCommit
		}
	}
	return claims
}

// executePlanStep runs the lead agent to produce a dynamic execution plan, then
// executes each role in the plan sequentially with per-step checkpointing so that
// a daemon restart can resume from the correct role index.
//
// Returns the last container ID, last result bytes, the next workflow step ID
// (the plan step's on_success target), and the updated completedSteps slice
// with each sub-role's synthetic step ID appended so the UI can render plan
// progress role-by-role instead of a single opaque "plan" entry.
func (e *Executor) executePlanStep(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	plan *executionPlan,
	stepID string,
	step registry.WorkflowStep,
	timeout time.Duration,
	state *executionState,
	outerCompletedSteps []string,
	inputArtifacts []map[string]string,
) (string, []byte, string, []string, error) {
	var lastContainerID string
	var lastResult []byte
	var lastMessage string

	// Local copy so synthetic sub-step IDs (plan_0_analyst, plan_1_coder, …)
	// can be appended as each role finishes without clobbering the caller's
	// slice. Returned at end so workflow.go uses this as its own view of
	// completedSteps for subsequent checkpoints.
	completedSteps := append([]string{}, outerCompletedSteps...)

	// Sample the pre-plan git HEAD before the lead planning step runs.
	// The lead can commit during its own planning step when it decides
	// the task is simple enough to handle directly — capturing HEAD here
	// ensures those commits are included in the patch set. Persist the
	// sample in state so resume after a checkpoint doesn't re-sample
	// after later commits have moved HEAD forward.
	if state.PlanStartHEAD == "" {
		state.PlanStartHEAD = gitHEAD(ctx, plan.worktreeDir)
	}

	// Capture + clear PendingRecovery before invoking the lead. The
	// runLeadPlanning code path receives it via opts and enforces
	// the recovery contract (outcome must be non-continue) after
	// parsing the result. Clearing here makes sure subsequent
	// non-recovery plan steps don't reuse a stale banner.
	var planRecoveryContext *RecoveryContext
	if state != nil && state.PendingRecovery != nil {
		planRecoveryContext = state.PendingRecovery
		state.PendingRecovery = nil
		e.logger.Info().
			Str("execution_id", execution.ID).
			Str("step", stepID).
			Str("failed_step", planRecoveryContext.FailedStep).
			Str("failure_class", planRecoveryContext.FailureClass).
			Msg("recovery: forwarding failure context to recovery plan step")
	}

	// Consume any pending operator hints for this plan step
	// (2026-05-26): the `case "agent":` branch in workflow.go already
	// consumes hints, but `case "plan":` routed through here
	// previously skipped them — so steering messages submitted while
	// the recover step was running were stored in the DB and never
	// reached the lead's prompt. Consume once at the parent plan
	// step level and propagate to the lead AND every planned sub-
	// role; that matches operator intuition ("steer this hop")
	// without forcing them to predict synthetic sub-step IDs.
	planHintPrefix := e.consumeHintsForStep(ctx, task.ID, execution.ID, stepID)

	// Resume from checkpoint if we already have a plan.
	if len(state.PlanSteps) == 0 {
		leadCID, leadStepID, planSteps, leadMessage, err := e.runLeadPlanning(ctx, task, execution, plan, stepID, step, timeout, inputArtifacts, planRecoveryContext, planHintPrefix, state)
		if IsLeadHandoff(err) {
			// Phase 25 — lead emitted checkpoint / external_wait /
			// closure_request and the executor already wrote the
			// task_message + flipped the task status. We propagate
			// the sentinel so workflow.go can skip the COMPLETED
			// status flip (the task is now AWAITING_INPUT /
			// AWAITING_EXTERNAL / still COMPLETED for closure).
			//
			// Append the lead step to completedSteps so dashboards
			// render the lead row as the terminating step.
			completedSteps = append(completedSteps, leadStepID)
			return leadCID, nil, "", completedSteps, errLeadHandoff
		}
		if err != nil {
			return "", nil, "", completedSteps, fmt.Errorf("lead planning failed: %w", err)
		}
		lastContainerID = leadCID
		// Persist the lead step ID so the resume path (after a
		// daemon restart mid-plan) still finalizes the right row
		// when the children complete.
		state.PlanLeadStepID = leadStepID

		// Validate planned roles against the swarm catalog and
		// substitute via SwarmRole.Aliases when the lead used a
		// known synonym ("editor" → "writer" in assistant-swarm
		// where the lead's training data biases toward editor;
		// "researcher" → "scout" in dev-swarm). Roles with
		// neither a direct match nor an alias hit are dropped;
		// fall through to "fail when nothing is left".
		if plan.swarm != nil {
			res := resolvePlanRoles(planSteps, plan.swarm.Roles)
			for _, c := range res.collisions {
				e.logger.Warn().
					Str("swarm", plan.swarm.ID).
					Str("alias", c.alias).
					Str("first_role", c.firstRole).
					Str("conflicting_role", c.conflictingRole).
					Msg("swarm role alias collision — first-seen wins; rename one of the aliases")
			}
			if len(res.substituted) > 0 {
				e.logger.Info().
					Str("execution_id", execution.ID).
					Str("swarm", plan.swarm.ID).
					Strs("substituted", res.substituted).
					Msg("lead plan used role aliases — substituted to canonical names")
			}
			if len(res.dropped) > 0 {
				e.logger.Warn().
					Str("execution_id", execution.ID).
					Str("swarm", plan.swarm.ID).
					Strs("dropped_roles", res.dropped).
					Strs("kept_roles", res.valid).
					Msg("lead plan included roles not in the swarm and not in any alias — dropping them and continuing with the valid subset")
			}
			if len(res.valid) == 0 {
				return "", nil, "", completedSteps, fmt.Errorf(
					"lead plan references only unknown roles %v (swarm %s has: %v)",
					res.dropped, plan.swarm.ID, roleNames(plan.swarm.Roles))
			}
			planSteps = res.valid
		}

		state.PlanSteps = planSteps
		state.PlanLeadMessage = leadMessage
		state.PlanIndex = 0
		// Checkpoint so a restart skips lead re-planning and resumes at index 0.
		if err := e.saveCheckpoint(ctx, execution, stepID, completedSteps, *state); err != nil {
			return "", nil, "", completedSteps, err
		}

		e.logger.Info().
			Str("execution_id", execution.ID).
			Str("step", stepID).
			Strs("plan_steps", planSteps).
			Msg("adaptive plan produced")
	}

	planSteps := state.PlanSteps
	// Seed the first planned role with the coordinator's reasoning so it has
	// context about why it was chosen (e.g. feasibility verdict, blocker list).
	lastMessage = state.PlanLeadMessage

	// Git HEAD before any role ran was sampled above (before lead
	// planning) and persisted in state. An empty value means the project
	// is not a git repo; skip all commit-claim checks in that case.
	planStartHEAD := state.PlanStartHEAD
	committedInPlan := planStartHEAD != "" && gitHEAD(ctx, plan.worktreeDir) != planStartHEAD

	// Execute each planned role starting from the last checkpoint index.
	for i := state.PlanIndex; i < len(planSteps); i++ {
		roleName := planSteps[i]
		syntheticStepID := fmt.Sprintf("%s_%d_%s", stepID, i, roleName)

		agentStep := registry.WorkflowStep{
			Type:      "agent",
			Role:      roleName,
			OnSuccess: step.OnSuccess,
			Timeout:   step.Timeout,
		}

		opts := &agentInputOpts{
			PreviousResult: lastMessage,
		}
		if i == 0 {
			opts.InputArtifacts = inputArtifacts
		}
		// Propagate the operator-hint prefix consumed at the parent
		// plan step boundary to each sub-role's prompt. Sub-roles have
		// no step.Prompt of their own (the role's SystemPrompt carries
		// the contract), so we hand the hint blocks through as the
		// StepPrompt — they'll surface as the leading <operator-hint>
		// section the role sees before its task brief.
		if planHintPrefix != "" {
			opts.StepPrompt = planHintPrefix
		}
		var roleConfig *registry.SwarmRole
		if plan.swarm != nil {
			for j, r := range plan.swarm.Roles {
				if r.Name == roleName {
					if prompt := registry.BuildEffectiveRolePrompt(plan.swarm, r); prompt != "" {
						opts.SystemPrompt = prompt
					}
					applyCounterfactualPromptOverride(opts, task, roleName)
					perm := r.Permissions
					opts.Permissions = &perm
					opts.ResponseFormat = effectiveResponseFormat(&r)
					opts.ShapeRetryHint = r.ShapeRetryHint
					roleConfig = &plan.swarm.Roles[j]
					applyRoleSchemaOpts(opts, roleConfig)
					break
				}
			}
		}

		// Capture HEAD immediately before the role runs so per-role claim
		// verification can compare against the pre-role state rather than
		// the plan-start state (which a previous role may have moved).
		preRoleHEAD := gitHEAD(ctx, plan.worktreeDir)

		e.logger.Info().
			Str("execution_id", execution.ID).
			Str("plan_step_id", syntheticStepID).
			Str("role", roleName).
			Int("index", i).
			Int("total", len(planSteps)).
			Msg("executing plan role")

		cid, result, err := e.executeAgentStepWithFallback(ctx, task, execution, plan, syntheticStepID, agentStep, timeout, opts, roleConfig)
		if err != nil {
			// Plan-quality attribution: the spawned child failed.
			// The LEAD chose that role, so the lead's pending row
			// flips to downstream_rejected with attributed_to_step
			// pointing at the failing child. Without this the
			// lead's row would stay OK and the dashboard would say
			// "lead is great" while the plans it produces are
			// failing every run. Skipped on resume paths where the
			// lead row was finalized in a previous attempt — the
			// state.PlanLeadStepID is "" in that case.
			if state.PlanLeadStepID != "" {
				attrID := syntheticStepID
				detail := fmt.Sprintf("plan role %d (%s) failed: %s", i, roleName, truncateStr(err.Error(), 400))
				e.finalizePendingOutcome(ctx, execution.ID, state.PlanLeadStepID,
					string(stepoutcome.DownstreamRejected),
					"plan_child_failed",
					detail,
					&attrID)
			}
			return "", nil, "", completedSteps, fmt.Errorf("plan role %d (%s) failed: %w", i, roleName, err)
		}
		lastContainerID = cid
		lastResult = result
		lastMessage = ""
		if len(result) > 0 {
			var r struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(result, &r) == nil {
				lastMessage = stripReasoning(r.Message)
			}
		}

		// Verify the agent's claims against ground truth. Two layers:
		//
		//   - The HEAD-didn't-advance checks live here (they need the
		//     pre/post HEAD pair this loop captured).
		//   - The cross-cutting claim verifications (testing.passed →
		//     toolAudit, files_changed → real diff count, checked_commit
		//     → object exists) live in verifyRoleClaims so the regular
		//     workflow agent-step path can call them too.
		//
		// Both fail the role with schema_violation so the scheduler's
		// retry path picks them up. Pre-2026.5.4 the file-count check
		// only fired on "HEAD didn't advance"; an agent that committed
		// one file but claimed five slipped through. The new
		// verifyRoleClaims layer catches that.
		if planStartHEAD != "" {
			claims := parseRoleClaims(result)
			postRoleHEAD := gitHEAD(ctx, plan.worktreeDir)
			headAdvanced := postRoleHEAD != "" && postRoleHEAD != preRoleHEAD
			if headAdvanced {
				committedInPlan = true
			}
			if claims.claimedCommit && !headAdvanced {
				// The agent returned well-formed JSON that asserted a
				// commit it didn't actually make. Attribute to the
				// sub-step as schema_violation — the output parsed but
				// failed the "claims match reality" invariant.
				detail := fmt.Sprintf("claimed committed:true but git HEAD did not advance (before=%s after=%s)",
					short(preRoleHEAD), short(postRoleHEAD))
				e.finalizePendingOutcome(ctx, execution.ID, syntheticStepID,
					string(stepoutcome.SchemaViolation), stepoutcome.ClassVerifyFailed, detail, nil)
				return "", nil, "", completedSteps, fmt.Errorf(
					"plan role %d (%s) claimed committed:true but git HEAD did not advance "+
						"(before=%s after=%s) — agent output does not match repository state",
					i, roleName, short(preRoleHEAD), short(postRoleHEAD))
			}
			if claims.claimedFilesChanged > 0 && !headAdvanced {
				detail := fmt.Sprintf("claimed files_changed=%d but git HEAD did not advance (before=%s after=%s)",
					claims.claimedFilesChanged, short(preRoleHEAD), short(postRoleHEAD))
				e.finalizePendingOutcome(ctx, execution.ID, syntheticStepID,
					string(stepoutcome.SchemaViolation), stepoutcome.ClassVerifyFailed, detail, nil)
				return "", nil, "", completedSteps, fmt.Errorf(
					"plan role %d (%s) claimed files_changed=%d but git HEAD did not advance "+
						"(before=%s after=%s) — no commit was created",
					i, roleName, claims.claimedFilesChanged,
					short(preRoleHEAD), short(postRoleHEAD))
			}
			// Cross-cutting deception checks. preRoleHEAD/postRoleHEAD
			// flow into the files_changed accuracy check; projectDir
			// flows into the commit-existence check.
			if err := e.verifyRoleClaims(ctx, result, preRoleHEAD, postRoleHEAD, plan.worktreeDir); err != nil {
				e.finalizePendingOutcome(ctx, execution.ID, syntheticStepID,
					string(stepoutcome.SchemaViolation), stepoutcome.ClassVerifyFailed, err.Error(), nil)
				return "", nil, "", completedSteps, fmt.Errorf(
					"plan role %d (%s) %w", i, roleName, err)
			}
		}

		// Output-artifact verification was attempted here but failed as
		// designed: (a) `/app/workspace/...` paths in result.json map
		// to the per-step temp `workspaceDir`, NOT `plan.worktreeDir`
		// — the two are unrelated dirs, and the temp one gets
		// defer-deleted by the time this code runs; (b) the only
		// artifact the agent currently declares is the synthesized
		// `<step>-response.md` wrapper which always exists, so there
		// was nothing useful to verify even if the paths worked. The
		// real failure mode (researcher claims COMPLETED but doesn't
		// write the scan file the writer expects) can't be caught
		// here because file_write outputs aren't declared in
		// result.outputArtifacts at all. Fixing it properly needs the
		// agent-side wrapper to collect file_write paths AND the
		// verification to live inside executeAgentStep where
		// workspaceDir is still alive. See missingDeclaredOutputs
		// below — helper kept for the eventual refactor.

		// This sub-step's output passed verification and the loop is
		// about to feed it forward. Finalize the pending row now.
		e.finalizePendingOutcome(ctx, execution.ID, syntheticStepID, string(stepoutcome.OK), "", "", nil)

		// Surface this sub-role in the UI as a completed step. Append before
		// the checkpoint so a daemon restart between role i and role i+1 sees
		// role i in the completed list rather than re-running it.
		completedSteps = append(completedSteps, syntheticStepID)

		state.PlanIndex = i + 1
		if err := e.saveCheckpoint(ctx, execution, stepID, completedSteps, *state); err != nil {
			return "", nil, "", completedSteps, err
		}
	}

	// Fix #7: if any role in the plan declared a commit claim (checked
	// per-step above) and HEAD ended where it started, the whole plan
	// was a no-op despite the individual roles claiming success. Treat
	// this as a workflow failure so the execution doesn't silently
	// register as COMPLETED with nothing to show.
	//
	// Plans that *never* made a commit claim (e.g. research → writer
	// workflows, or a pure scout run) are allowed — zero commits is a
	// legitimate outcome for non-code tasks.
	if planStartHEAD != "" && !committedInPlan {
		claimedAnyCommit := false
		if len(lastResult) > 0 {
			if parseRoleClaims(lastResult).claimedCommit {
				claimedAnyCommit = true
			}
		}
		if claimedAnyCommit {
			return "", nil, "", completedSteps, fmt.Errorf(
				"plan completed with no new commits on %s despite roles claiming work "+
					"(HEAD unchanged at %s)",
				plan.worktreeDir, short(planStartHEAD))
		}
	}

	// Mailbox patches + human-readable summary as the task's canonical
	// output. When the plan produced real commits we:
	//   1. generate one .patch per commit via git format-patch,
	//   2. persist each through the artifact store so they surface in
	//      the UI's artifact panel and are forwarded to Telegram
	//      watchers by sendArtifactsToWatchers,
	//   3. persist a CHANGES.md companion that the UI can download and
	//      that provides a readable diff for reviewers,
	//   4. replace lastResult with an envelope whose "message" field is
	//      the commit-derived summary — handleSuccess extracts message
	//      for the completion notification, and the task_detail UI
	//      renders it in the Output panel.
	// When there are no commits we leave lastResult alone so non-code
	// plans (research → writer, scout-only runs) keep whatever the last
	// role produced.
	if planStartHEAD != "" && committedInPlan && plan.worktreeDir != "" && e.artifactStore != nil {
		finalHEAD := gitHEAD(ctx, plan.worktreeDir)
		changes, err := generatePlanChanges(ctx, plan.worktreeDir, planStartHEAD, finalHEAD)
		if err != nil {
			// Patch generation is best-effort — a failure here should
			// not turn a successful plan into a failed execution. Log
			// and fall through with the last role's raw result.
			e.logger.Warn().
				Err(err).
				Str("execution_id", execution.ID).
				Str("worktree", plan.worktreeDir).
				Msg("plan patch generation failed — keeping last-role result as task output")
		} else if changes != nil {
			defer func() { _ = os.RemoveAll(changes.OutputDir) }()
			for _, patchPath := range changes.Patches {
				name := filepath.Base(patchPath)
				if _, storeErr := e.artifactStore.Store(ctx, task.ProjectID, execution.ID, task.ID, name, patchPath); storeErr != nil {
					e.logger.Warn().
						Err(storeErr).
						Str("execution_id", execution.ID).
						Str("patch", name).
						Msg("failed to persist plan patch as artifact")
				}
			}
			summaryPath := filepath.Join(changes.OutputDir, "CHANGES.md")
			if _, storeErr := e.artifactStore.Store(ctx, task.ProjectID, execution.ID, task.ID, "CHANGES.md", summaryPath); storeErr != nil {
				e.logger.Warn().
					Err(storeErr).
					Str("execution_id", execution.ID).
					Msg("failed to persist CHANGES.md as artifact")
			}

			envelope := map[string]any{
				"status":  "COMPLETED",
				"message": changes.Summary,
				"changes": map[string]any{
					"from":        short(changes.FromSHA),
					"to":          short(changes.ToSHA),
					"patchCount":  len(changes.Patches),
					"commitCount": len(changes.Commits),
					"commits":     changes.Commits,
				},
			}
			if data, marshalErr := json.Marshal(envelope); marshalErr == nil {
				lastResult = data
			} else {
				e.logger.Warn().
					Err(marshalErr).
					Msg("failed to marshal plan summary envelope — keeping last-role result")
			}
			e.logger.Info().
				Str("execution_id", execution.ID).
				Int("patches", len(changes.Patches)).
				Int("commits", len(changes.Commits)).
				Str("from", short(changes.FromSHA)).
				Str("to", short(changes.ToSHA)).
				Msg("plan patches and summary persisted")
		}
	}

	// Plan-quality attribution: every spawned child succeeded, so the
	// lead's plan was good. Flip the held-open lead row from
	// pending_validation to OK now that the picture is complete.
	// state.PlanLeadStepID may be empty on a resume path where the
	// lead row was finalized in a prior attempt — that's a no-op.
	if state.PlanLeadStepID != "" {
		e.finalizePendingOutcome(ctx, execution.ID, state.PlanLeadStepID,
			string(stepoutcome.OK), "", "", nil)
	}

	return lastContainerID, lastResult, step.OnSuccess, completedSteps, nil
}

// roleNames returns just the names from a slice of SwarmRoles for use in
// error messages. Keeps the caller's error string short and focused on
// what the operator needs to fix.
func roleNames(roles []registry.SwarmRole) []string {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, r.Name)
	}
	return out
}

// short returns the first 7 characters of a git SHA for log messages.
// Works on any string; shorter inputs are returned unchanged.
func short(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}

// runLeadPlanning runs the lead agent and parses its plan JSON output.
// Returns the container ID, the lead's synthetic step ID, the
// ordered list of role names, and the coordinator's message
// (used as context for the first planned role). The lead step ID
// flows back to the caller so the executor can finalize the lead's
// pending outcome row after the spawned children have run — that's
// what enables plan-quality attribution (lead is downstream_rejected
// when its own plan caused a child to fail).
func (e *Executor) runLeadPlanning(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	plan *executionPlan,
	stepID string,
	step registry.WorkflowStep,
	timeout time.Duration,
	inputArtifacts []map[string]string,
	recoveryContext *RecoveryContext,
	hintPrefix string,
	state *executionState,
) (string, string, []string, string, error) {
	// Phase 25+ — feed the lead the conversation thread + scratchpad
	// when the conversational lifecycle is wired. Without it (legacy
	// deployments) the lead sees only the original step prompt, same
	// as before.
	var convoMsgs []*persistence.TaskMessage
	var scratchpad *persistence.TaskScratchpad
	if e.taskMessageRepo != nil {
		if msgs, mErr := e.taskMessageRepo.List(ctx, persistence.TaskMessageFilter{
			TaskID: task.ID,
			Limit:  50,
		}); mErr == nil {
			convoMsgs = msgs
		}
	}
	if e.taskScratchpadRepo != nil {
		if sp, spErr := e.taskScratchpadRepo.Get(ctx, task.ID); spErr == nil {
			scratchpad = sp
		}
	}

	// Structured recovery-checkpoint actions (LLD §9): on a recovery
	// hop, if the operator approved a decision-checkpoint option carrying
	// a structured action, APPLY it before the lead re-plans. Guardrails
	// preserved — this is only reachable on resume AFTER the operator
	// answered (no auto-pivot); reroute stays AdaptiveCandidateWorkflows-
	// bounded and model_fallback operator-gated; every failure fails-safe
	// to today's prose-hint path (resolveOperatorCheckpointAction returns
	// only normalized/valid actions, applyRecoveryCheckpointAction
	// swallows rejections). nil/retry/skip leave the prose path unchanged.
	if recoveryContext != nil {
		if action := resolveOperatorCheckpointAction(convoMsgs); action != nil {
			if e.applyRecoveryCheckpointAction(ctx, task, plan.project, plan.swarm, action) {
				e.logger.Info().
					Str("execution_id", execution.ID).
					Str("step", stepID).
					Str("action_type", string(action.Type)).
					Msg("recovery: applied operator-approved structured checkpoint action before re-planning")
			}
		}
	}

	// Consumer A (slice 3): when the failure_playbooks gate is on and
	// the instinct repo is wired, augment the recovery context with the
	// worker-mined "similar failures previously resolved here" overlay.
	// Advisory only — it never replaces the lead's judgement or
	// auto-pivots recovery; the operator still approves the checkpoint.
	// No-op (and the prompt stays byte-for-byte identical to today) when
	// the gate is off, the repo is nil, or there's no matching instinct.
	if recoveryContext != nil {
		e.attachLearnedRemediations(ctx, task, execution, recoveryContext)
	}

	leadPrompt := buildPlanningPromptWithContext(step.Prompt, plan.swarm, scratchpad, convoMsgs, recoveryContext)
	if hintPrefix != "" {
		// Prepend the operator-hint blocks consumed at the plan-step
		// boundary so the lead sees them as instruction-shaped context
		// ahead of its planning prompt. Matches the case "agent":
		// behaviour in workflow.go.
		leadPrompt = hintPrefix + leadPrompt
	}
	opts := &agentInputOpts{
		InputArtifacts: inputArtifacts,
		StepPrompt:     leadPrompt,
	}
	// Forward recovery context to the lead so context.recovery
	// renders in its prompt. The caller (executePlanStep) is
	// responsible for clearing state.PendingRecovery on the way in
	// so subsequent steps don't see a stale banner.
	if recoveryContext != nil {
		opts.RecoveryContext = recoveryContext
	}
	var roleConfig *registry.SwarmRole
	if plan.swarm != nil {
		for i, r := range plan.swarm.Roles {
			if r.Name == step.Role {
				if r.SystemPrompt != "" {
					opts.SystemPrompt = r.SystemPrompt
				}
				applyCounterfactualPromptOverride(opts, task, step.Role)
				perm := r.Permissions
				opts.Permissions = &perm
				opts.ResponseFormat = effectiveResponseFormat(&r)
				opts.ShapeRetryHint = r.ShapeRetryHint
				roleConfig = &plan.swarm.Roles[i]
				applyRoleSchemaOpts(opts, roleConfig)
				break
			}
		}
	}

	// Recovery-mode schema override (2026-05-26 — long-term fix):
	// when the lead is running in recovery mode, swap the role's
	// generic ResponseSchema for one that constrains `outcome` to
	// the enum [checkpoint, external_wait, closure_request]. This
	// is model-level enforcement: providers that honor json_schema
	// (OpenAI, Bedrock Converse) will reject continue at the
	// structured-output decoder before the daemon ever sees it.
	// Providers that don't enforce schemas fall through to the
	// prompt-level safeguards (recovery banner + pruned menu +
	// corrective-hint retry). Each layer catches what the layer
	// below misses.
	if recoveryContext != nil {
		opts.ResponseSchema = recoveryModeResponseSchema()
		opts.ResponseFormat = "json_schema"
		// Drop the result-emission tool variant — its schema would
		// also need a recovery-mode flavour, and tool-call schemas
		// land via a different gateway path. Forcing json_schema for
		// recovery hops keeps the enforcement surface to one path
		// the test pins.
		opts.ResultEmissionTool = nil
	}

	leadStep := registry.WorkflowStep{
		Type:      "agent",
		Role:      step.Role,
		OnSuccess: step.OnSuccess,
		Timeout:   step.Timeout,
	}

	// Keep the lead step ID descriptive for execution/outcome detail pages.
	// Role quality is keyed by the explicit role field, but including the
	// role in the synthetic step ID makes logs and persisted rows easier to
	// inspect.
	leadStepID := stepID + "_lead_" + step.Role
	cid, resultBytes, err := e.executeAgentStepWithFallback(ctx, task, execution, plan, leadStepID, leadStep, timeout, opts, roleConfig)
	if err != nil {
		// Container-level failure already wrote a terminal outcome
		// (failed/timeout/cancelled) in executeAgentStep's defer.
		return "", "", nil, "", err
	}

	// Phase 25: try the new lead-outcome envelope first. When the
	// lead emits a non-continue outcome AND the conversational
	// lifecycle is wired (taskMessageRepo + persistTaskRepo set
	// by service.container), we write the task_message + flip the
	// task status to AWAITING_INPUT / AWAITING_EXTERNAL and signal
	// hand-off via errLeadHandoff. The legacy parsePlanSteps path
	// runs only when the outcome is continue (or absent).
	parsedOutcome, parsedOK, _ := ParseLeadOutcome(resultBytes)

	// In recovery mode a wrong-kind checkpoint (or continue+plan) is a
	// contract violation handled below by a corrective-hint retry. It must
	// NOT be handed off to the operator first — a kind=review/action_required
	// checkpoint carries its alternatives as prose, so the operator would see
	// a checkpoint with no selectable options (task …a917b9aa76e81a8b).
	recoveryViolation := recoveryContext != nil && recoveryContractViolated(parsedOK, parsedOutcome)

	if !recoveryViolation && parsedOK && parsedOutcome != nil && parsedOutcome.Outcome != LeadOutcomeContinue {
		if e.taskMessageRepo != nil && e.persistTaskRepo != nil {
			if hErr := e.handleLeadHandoff(ctx, task, execution, leadStepID, parsedOutcome); hErr != nil {
				return cid, leadStepID, nil, "", fmt.Errorf("lead handoff failed: %w", hErr)
			}
			// Mark the lead step OK — its job (emitting a valid
			// outcome) is done. Caller propagates errLeadHandoff
			// to skip the executePlanStep child-spawn path and
			// the workflow.go COMPLETED status flip.
			e.finalizePendingOutcome(ctx, execution.ID, leadStepID, "ok", "", "", nil)
			return cid, leadStepID, nil, "", errLeadHandoff
		}
		// Conversational lifecycle not wired but the lead emitted a
		// non-continue outcome — fall through to the legacy parse
		// failure path so the operator sees an explicit error.
		e.logger.Warn().
			Str("execution_id", execution.ID).
			Str("outcome", string(parsedOutcome.Outcome)).
			Msg("lead emitted non-continue outcome but conversational lifecycle is not wired; treating as parse failure")
	}

	// Recovery-mode contract enforcement (2026-05-18, extended
	// 2026-05-26): when the step received a RecoveryContext, the
	// lead's ONE allowed move is to emit a non-continue outcome
	// (typically checkpoint=decision proposing alternatives).
	// Emitting continue + a plan with role steps is a contract
	// violation — re-spawning the same role that just failed produces
	// the same failure (observed live on T-0833 / janka CV cascade).
	//
	// 2026-05-26 — corrective-hint retry: previously this path failed
	// the step terminally on first violation. Mirrors the
	// planRefusalCorrectiveHint pattern: re-run the lead ONCE with an
	// explicit corrective hint that names what was wrong and what the
	// fix is. Models often slip past prose instructions but pay
	// attention to "your previous attempt did X — do Y instead."
	// Only fails terminally on the second violation.
	//
	// This is the first concrete slice of the "Workflow-execution
	// self-healing" roadmap item: a bounded corrective-hint retry
	// loop driven by a structured failure signal. Subsequent classes
	// (verifier failures, schema violations) plug into the same
	// pattern.
	if recoveryContext != nil {
		if recoveryViolation {
			outcomeLabel := "missing"
			if parsedOK && parsedOutcome != nil {
				outcomeLabel = string(parsedOutcome.Outcome)
				if parsedOutcome.Outcome == LeadOutcomeCheckpoint && parsedOutcome.Checkpoint != nil {
					// e.g. "checkpoint:review" — the corrective hint
					// branches on the "checkpoint" prefix to explain that
					// recovery checkpoints must be kind=decision.
					outcomeLabel = "checkpoint:" + string(parsedOutcome.Checkpoint.Kind)
				}
			}
			retryCID, retryStepID, retryCompletedOK, retryHandoffErr := e.retryRecoveryContractViolation(
				ctx, task, execution, plan, stepID, leadStep, timeout, opts, recoveryContext, outcomeLabel,
			)
			if retryHandoffErr != nil {
				// Retry succeeded — the lead emitted a valid non-
				// continue outcome on the second attempt and the
				// handoff path already wrote the task_message and
				// flipped task status. Propagate errLeadHandoff.
				return retryCID, retryStepID, nil, "", retryHandoffErr
			}
			if retryCompletedOK {
				// Retry was attempted but reported "no second
				// violation worth surfacing" — fall through to
				// continue-flow parsing as if the lead had emitted a
				// plan all along (rare; only when the conversational
				// lifecycle isn't wired). Use the retry's step ID so
				// outcome rows land correctly.
				leadStepID = retryStepID
				cid = retryCID
				// fall through to parsePlanSteps below
			} else {
				// Retry attempt failed validation too — fail loud.
				e.logger.Warn().
					Str("execution_id", execution.ID).
					Str("step", leadStepID).
					Str("outcome", outcomeLabel).
					Str("failed_step", recoveryContext.FailedStep).
					Str("failure_class", recoveryContext.FailureClass).
					Msg("recovery contract violation: lead must emit a decision checkpoint (or external_wait/closure_request), not continue+plan or a non-decision checkpoint (corrective-hint retry also failed)")
				return cid, leadStepID, nil, "", fmt.Errorf(
					"recovery contract violation: step %q was a recovery hop (failed_step=%q, class=%q) but the lead emitted outcome=%q instead of a decision `checkpoint` — corrective-hint retry also failed the contract, so re-spawning the failed role would just repeat the failure",
					stepID, recoveryContext.FailedStep, recoveryContext.FailureClass, outcomeLabel,
				)
			}
		}
	}

	steps, message, err := parsePlanSteps(resultBytes)
	if err == nil {
		// Dynamic tool budget: capture the lead's complexity verdict
		// (continue-outcome path; parsedOutcome was parsed above) onto
		// execution state so subsequent worker spawns scale their
		// tool-iteration budget. Validated — a garbage value never
		// persists. See https://docs.vornik.io
		if state != nil && parsedOutcome != nil {
			if tier := validComplexityTier(parsedOutcome.Complexity); tier != "" {
				state.ComplexityTier = tier
			}
		}
		// Parse succeeded — the lead's output was consumable. Hold
		// the lead step's pending row open until the rest of the
		// plan has executed so the caller (executePlanStep) can
		// flip it to OK only when every spawned child succeeds, or
		// to downstream_rejected (attributed_to_step = failing
		// child) when one of them tripped over the lead's plan.
		// Without this, the lead's row was always OK regardless of
		// whether 4 of 5 children failed — quality metrics
		// double-counted bad plans as good leads.
		return cid, leadStepID, steps, message, nil
	}

	// Refusal-specific retry: when the lead emits valid JSON but it's a
	// refusal ("I cannot plan this", "out of scope", etc.), re-run with
	// a corrective hint that nudges the model toward a research-only
	// plan instead of refusing outright. parse_plan_refused was 85
	// cases over the most-recent 7 days — almost all of them benign
	// requests the lead misclassified as too risky to plan. Retrying
	// once with explicit guidance reclaims most of those.
	//
	// Other parse failures (invalid JSON, no steps, empty result) keep
	// the existing fail-fast behaviour: the executeAgentStepWithShape
	// Retry wrapper above already gave them a corrective re-run, so a
	// failure here means the second attempt also went sideways.
	if isPlanRefusal(err) {
		correctedOpts := *opts
		correctedOpts.StepPrompt = opts.StepPrompt + planRefusalCorrectiveHint(err)
		retryStepID := leadStepID + "_refusal_retry"
		e.logger.Warn().
			Str("execution_id", execution.ID).
			Str("step", leadStepID).
			Str("retry_step", retryStepID).
			Str("error", truncateForPrompt(err.Error(), 200)).
			Msg("plan refusal: re-running lead with corrective prompt")

		cid2, resultBytes2, err2 := e.executeAgentStep(ctx, task, execution, plan, retryStepID, leadStep, timeout, &correctedOpts)
		if err2 == nil {
			steps2, message2, parseErr2 := parsePlanSteps(resultBytes2)
			if parseErr2 == nil {
				// Refusal-retry succeeded. Hold the lead's pending
				// row open exactly like the happy path so the
				// caller can finalize it after the spawned
				// children run. Using the retry's stepID so the
				// finalization lands on the right row.
				return cid2, retryStepID, steps2, message2, nil
			}
			// Second attempt parsed but is still unusable — fall
			// through to the original failure attribution. Use the
			// FIRST attempt's error so the operator sees what
			// triggered the retry, not the (possibly different)
			// second-attempt failure.
		}
	}

	// The container exited cleanly but its output is unusable.
	// Attribute the failure to the lead step (the producer), not
	// to the plan step (the consumer) — this is exactly the
	// attribution the old metric got wrong.
	outcome, class := classifyLeadParseError(err)
	e.finalizePendingOutcome(ctx, execution.ID, leadStepID, outcome, class, err.Error(), nil)
	return cid, "", nil, "", fmt.Errorf("could not parse plan from lead output: %w", err)
}

// isPlanRefusal returns whether err looks like a plan-refusal
// failure (lead emitted valid JSON that explicitly declined the
// task) as opposed to a parse failure or no-steps failure. Only
// refusals get the corrective re-run — re-prompting on a parse
// failure already happens at the shape-retry layer.
func isPlanRefusal(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "lead agent refused to plan")
}

// retryRecoveryContractViolation re-runs the lead ONCE with a
// corrective hint appended to the step prompt, after the first
// attempt emitted continue/missing in recovery mode. Returns:
//
//   - containerID and stepID of the retry attempt (used by caller
//     so finalization lands on the retry's row).
//   - completedOK=true when the retry produced a valid non-continue
//     outcome AND handoff succeeded — in that case handoffErr is
//     errLeadHandoff. The caller propagates it.
//   - completedOK=true with handoffErr=nil only happens when the
//     conversational lifecycle isn't wired (rare) — the caller
//     falls through to continue-shape parsing with the retry's
//     stepID. This matches the legacy fall-through behaviour above.
//   - completedOK=false, handoffErr=nil means the retry also
//     emitted continue/missing — caller fails the step terminally.
//
// We use executeAgentStep (single attempt, no model fallback) to
// match the planRefusalCorrectiveHint pattern; the corrective hint
// is the variable being tested, not the model.
func (e *Executor) retryRecoveryContractViolation(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	plan *executionPlan,
	stepID string,
	leadStep registry.WorkflowStep,
	timeout time.Duration,
	opts *agentInputOpts,
	recoveryCtx *RecoveryContext,
	firstAttemptOutcome string,
) (cid string, retryStepID string, completedOK bool, handoffErr error) {
	correctedOpts := *opts
	correctedOpts.StepPrompt = opts.StepPrompt + recoveryContractCorrectiveHint(recoveryCtx, firstAttemptOutcome)
	retryStepID = stepID + "_lead_" + leadStep.Role + "_recovery_retry"
	e.logger.Warn().
		Str("execution_id", execution.ID).
		Str("step", stepID).
		Str("retry_step", retryStepID).
		Str("first_outcome", firstAttemptOutcome).
		Str("failure_class", recoveryCtx.FailureClass).
		Msg("recovery contract violation: re-running lead with corrective hint")

	cid2, resultBytes2, err2 := e.executeAgentStep(ctx, task, execution, plan, retryStepID, leadStep, timeout, &correctedOpts)
	if err2 != nil {
		// Container-level retry failure — the executeAgentStep defer
		// already finalized the row. Surface as "retry didn't help."
		return cid2, retryStepID, false, nil
	}
	parsedOutcome2, parsedOK2, _ := ParseLeadOutcome(resultBytes2)
	if parsedOK2 && parsedOutcome2 != nil && parsedOutcome2.Outcome != LeadOutcomeContinue {
		if e.taskMessageRepo != nil && e.persistTaskRepo != nil {
			if hErr := e.handleLeadHandoff(ctx, task, execution, retryStepID, parsedOutcome2); hErr != nil {
				// Handoff side-effect failed — surface as a real
				// error (not a violation).
				return cid2, retryStepID, false, fmt.Errorf("recovery retry handoff failed: %w", hErr)
			}
			e.finalizePendingOutcome(ctx, execution.ID, retryStepID, "ok", "", "", nil)
			return cid2, retryStepID, true, errLeadHandoff
		}
		// Conversational lifecycle not wired — fall through.
		return cid2, retryStepID, true, nil
	}
	// Retry STILL emitted continue or missing — caller will fail loud.
	return cid2, retryStepID, false, nil
}

// recoveryModeResponseSchema returns the JSON Schema constraining
// the lead's response_format in recovery mode. The schema:
//
//   - REQUIRES the `outcome` field (no missing-outcome fallback to
//     continue — which the validator otherwise treats as a contract
//     violation).
//   - Enumerates `outcome` as [checkpoint, external_wait,
//     closure_request] — `continue` is structurally absent.
//
// Providers that honour response_format=json_schema (OpenAI's
// strict mode, Bedrock Converse's constrained generation) refuse
// to emit a value outside the enum at the decoder layer. Providers
// that don't enforce strictly still receive the schema in their
// prompt envelope, which the agent runtime adapts into a tool-call
// schema (Anthropic) or json_object nudge.
//
// The nested checkpoint/external_wait/closure_request objects are
// kept permissive (additionalProperties: true) so the schema
// rejects only the forbidden outcome, not the body shape variants
// each kind permits. Body validation lives in the lead-outcome
// parser (lead_outcome.go) where it can produce specific error
// messages.
func recoveryModeResponseSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"outcome"},
		"properties": map[string]any{
			"outcome": map[string]any{
				"type": "string",
				"enum": []string{
					string(LeadOutcomeCheckpoint),
					string(LeadOutcomeExternalWait),
					string(LeadOutcomeClosureRequest),
				},
				"description": "MUST be checkpoint, external_wait, or closure_request. The `continue` outcome is forbidden in recovery mode.",
			},
			"checkpoint": map[string]any{
				"type":                 "object",
				"additionalProperties": true,
			},
			"external_wait": map[string]any{
				"type":                 "object",
				"additionalProperties": true,
			},
			"closure_request": map[string]any{
				"type":                 "object",
				"additionalProperties": true,
			},
			"message":           map[string]any{"type": "string"},
			"scratchpad_update": map[string]any{"type": "object", "additionalProperties": true},
		},
		"additionalProperties": true,
	}
}

// recoveryContractViolated reports whether a parsed lead outcome breaks
// the recovery contract. In recovery mode the lead's job is to propose
// selectable alternatives for the operator, so the ONLY acceptable moves
// are:
//
//   - checkpoint with kind=decision (structured options[] the UI/Telegram
//     render as choices),
//   - external_wait or closure_request (legitimate recovery terminals).
//
// Everything else is a violation:
//
//   - continue+plan re-spawns the role that just failed (the original
//     2026-05-18 contract),
//   - a checkpoint of kind=review/action_required carries its alternatives
//     as prose inside draft/task_for_human, which the operator surfaces
//     can't render as selectable options — exactly the
//     task …a917b9aa76e81a8b (2026-06-18) bug where the operator saw the
//     proposal text but no buttons to pick from,
//   - an unparseable/missing outcome.
func recoveryContractViolated(parsedOK bool, o *LeadOutcome) bool {
	if !parsedOK || o == nil {
		return true
	}
	switch o.Outcome {
	case LeadOutcomeCheckpoint:
		return o.Checkpoint == nil || o.Checkpoint.Kind != CheckpointKindDecision
	case LeadOutcomeExternalWait, LeadOutcomeClosureRequest:
		return false
	default:
		// continue (or any unexpected outcome) — re-spawning the failed
		// role just repeats the failure.
		return true
	}
}

// recoveryContractCorrectiveHint builds the corrective text appended
// to the lead's step prompt on the recovery-violation retry attempt.
// Mirrors planRefusalCorrectiveHint's shape and tone: name what was
// wrong, then say exactly what the fix is.
//
// firstOutcome is the violation label from runLeadPlanning: "continue"
// (continue+plan), "missing" (unparseable), or "checkpoint:<kind>" when
// the lead emitted a checkpoint of the wrong kind. The hint branches on
// the latter so it doesn't mis-describe a checkpoint as a plan to spawn
// role steps (task …a917b9aa76e81a8b).
func recoveryContractCorrectiveHint(rc *RecoveryContext, firstOutcome string) string {
	fix := "You MUST emit outcome=checkpoint with kind=decision: " +
		`{"outcome":"checkpoint","checkpoint":{"kind":"decision","question":"<one-sentence summary of what failed and what to do>","options":[{"id":"a","label":"<alternative 1>"},{"id":"b","label":"<alternative 2>"},{"id":"abort","label":"abort with explanation"}],"default_if_no_response":"abort"},"message":"..."}` +
		". Read your role's recovery-mode playbook for per-failure-class proposals. " +
		"If no viable alternative exists, still emit a decision whose options include \"abort with explanation\". " +
		"Respond ONLY with the JSON envelope.]"

	if strings.HasPrefix(firstOutcome, "checkpoint") {
		return "\n\n[CORRECTION: your previous response emitted a " + firstOutcome +
			" — its alternatives were prose, not selectable options. This is INVALID in recovery mode: " +
			"the operator must be able to PICK an alternative, so a recovery checkpoint MUST be kind=decision " +
			"with a structured options[] array, not free text inside draft/task_for_human. " +
			"The prior step (" + rc.FailedStep + ", class=" + rc.FailureClass + ") already failed. " +
			fix
	}
	return "\n\n[CORRECTION: your previous response emitted outcome=" + firstOutcome +
		" with a plan to spawn role steps. This is INVALID in recovery mode — " +
		"the prior step (" + rc.FailedStep + ", class=" + rc.FailureClass + ") already failed, " +
		"and re-spawning the same role will just repeat the failure. " +
		fix
}

// attachLearnedRemediations populates rc.LearnedRemediations from the
// (advisory) continuous-learning instinct layer — Consumer A, slice 3.
//
// It is a strict opt-in, fail-soft overlay:
//   - With the instinct.consumers.failure_playbooks gate off (or no
//     instinct repo wired), it returns immediately and rc is untouched,
//     so the recovery prompt is byte-for-byte identical to today.
//   - It is read-mostly: the only write is one InstinctApplication
//     (feedback) row per surfaced instinct, on the lead_recovery
//     surface. It NEVER mutates the audit spine and NEVER auto-pivots
//     recovery — the lead still proposes alternatives and the operator
//     still approves the checkpoint.
//   - Any repo error is logged and swallowed: a degraded instinct store
//     must never block recovery planning.
//
// The match key is the FAILED step's recorded stepoutcome ErrorClass
// (and role) — the same value recovery instincts key their trigger on —
// looked up from the step-outcome row for (execution, failed_step). When
// no outcome row / error class is available (pre-outcome executions, or
// the failed step recorded no class), there is nothing to match on and
// the overlay stays empty.
func (e *Executor) attachLearnedRemediations(ctx context.Context, task *persistence.Task, execution *persistence.Execution, rc *RecoveryContext) {
	if !e.instinctPlaybooks || e.instinctRepo == nil || rc == nil {
		return
	}
	if task == nil || task.ProjectID == "" || rc.FailedStep == "" || execution == nil {
		return
	}

	// Resolve the failed step's recorded error class + role from its
	// step-outcome row — that is the value recovery instincts key on
	// (RecoveryContext.FailureClass is the coarser executor taxonomy and
	// won't match the trigger). Newest row wins.
	class, role := e.failedStepErrorClass(ctx, execution.ID, rc.FailedStep)
	if class == "" {
		return
	}

	rems, err := playbook.LearnedRemediations(ctx, e.instinctRepo, class, task.ProjectID, role, 3)
	if err != nil {
		e.logger.Debug().Err(err).
			Str("task_id", task.ID).
			Str("error_class", class).
			Msg("instinct learned-remediation lookup failed; recovery proceeds without overlay")
		return
	}
	if len(rems) == 0 {
		return
	}
	// v2 auto-apply: promote eligible remediations (confidence floor +
	// optional error-class allowlist) from advisory to a prompt-level
	// directive. The mark drives both the recovery-prompt rendering
	// (learnedRemediationsBlock) and the recorded application result.
	autoApplied := 0
	for i := range rems {
		elig := e.instinctAutoApply.eligible(
			rems[i].Confidence, rems[i].SupportCount, rems[i].ContradictCount, rems[i].ErrorClass)
		if elig {
			rems[i].AutoApplied = true
			autoApplied++
		}
		// Pipeline visibility: surface where each remediation sits relative
		// to the clean-support bar so an operator can watch instincts
		// approach auto-apply eligibility before they qualify (the consumer
		// is otherwise silent until it fires). See the supply design.
		e.logger.Debug().
			Str("instinct_id", rems[i].InstinctID).
			Str("error_class", rems[i].ErrorClass).
			Float64("confidence", rems[i].Confidence).
			Int("support", rems[i].SupportCount).
			Int("contradict", rems[i].ContradictCount).
			Int("min_clean_support", e.instinctAutoApply.minCleanSupport).
			Bool("eligible", elig).
			Msg("instinct auto-apply eligibility")
	}
	rc.LearnedRemediations = rems

	// Record one application/feedback row per surfaced instinct so the
	// feedback loop can correlate surfacing with the eventual outcome. An
	// advisory surfacing is "ignored" (shown, not acted on); an auto-applied
	// one is "auto_applied" (surfaced as a directive). Both are PENDING —
	// the RecoveryResolver later flips them to succeeded/failed from the
	// step outcome. Auto-apply still goes through the operator's recovery
	// approval gate; it changes emphasis, not authority.
	for _, r := range rems {
		result := persistence.InstinctResultIgnored
		if r.AutoApplied {
			result = persistence.InstinctResultAutoApplied
		}
		appErr := e.instinctRepo.RecordApplication(ctx, &persistence.InstinctApplication{
			InstinctID:  r.InstinctID,
			TaskID:      task.ID,
			Surface:     persistence.InstinctSurfaceLeadRecovery,
			Result:      result,
			ExecutionID: execution.ID,
			StepID:      rc.FailedStep,
		})
		if appErr != nil {
			e.logger.Debug().Err(appErr).
				Str("instinct_id", r.InstinctID).
				Msg("recording instinct application (lead_recovery) failed; non-fatal")
		}
		// Bump the surfacing counter regardless of the RecordApplication
		// outcome: the metric tracks that the remediation was SHOWN to the
		// lead, which happened whether or not the feedback row persisted.
		// Nil-safe — an executor without a metrics sink simply doesn't emit.
		if e.instinctMetrics != nil && e.instinctMetrics.ApplicationsTotal != nil {
			e.instinctMetrics.ApplicationsTotal.WithLabelValues(
				persistence.InstinctSurfaceLeadRecovery, result).Inc()
		}
	}
	e.logger.Debug().
		Str("task_id", task.ID).
		Str("error_class", class).
		Int("learned_remediations", len(rems)).
		Int("auto_applied", autoApplied).
		Msg("surfaced learned recovery remediations to lead")
}

// failedStepErrorClass returns the recorded stepoutcome ErrorClass and
// role for the failed step's most recent outcome row, or ("","") when no
// row / class is available. Read-only.
func (e *Executor) failedStepErrorClass(ctx context.Context, executionID, stepID string) (class, role string) {
	if e.outcomeRepo == nil || executionID == "" || stepID == "" {
		return "", ""
	}
	rows, err := e.outcomeRepo.List(ctx, persistence.ExecutionStepOutcomeFilter{
		ExecutionID: &executionID,
		StepID:      &stepID,
		PageSize:    1,
	})
	if err != nil || len(rows) == 0 || rows[0] == nil {
		return "", ""
	}
	return rows[0].ErrorClass, rows[0].Role
}

// planRefusalCorrectiveHint builds the suffix appended to the lead's
// step prompt on the refusal-retry attempt. The hint:
//
//   - explicitly invites the lead to choose a research-only plan if
//     the task seems too risky to act on directly. Investigations
//     produce useful artifacts even when the underlying request was
//     ambiguous, so a researcher run is almost always salvageable.
//   - reminds the lead that refusing is itself an outcome the
//     swarm cannot use — the operator's task gets a hard fail.
//
// The previous error message is appended verbatim so the model sees
// what its first attempt produced.
func planRefusalCorrectiveHint(err error) string {
	prev := truncateForPrompt(err.Error(), 400)
	return "\n\n[CORRECTION: your previous response was a refusal (" + prev +
		"). Refusing leaves the operator with no progress. " +
		"If the task seems too risky to act on, return a research-only plan: " +
		`{"plan": [{"role": "researcher", "prompt": "<scope the request, ` +
		`identify constraints, and report findings without taking ` +
		`irreversible action>"}]}` +
		". If the request is unclear, do the same with a researcher plan " +
		"asking for clarification. Respond ONLY with the plan JSON.]"
}

// classifyLeadParseError maps a parsePlanSteps error to the outcome
// taxonomy. Matching on wrapped-error substrings is deliberate — the
// producer errors are constructed by package-local functions using
// fmt.Errorf with stable prefixes, so this is a stable contract
// without needing a sentinel-error hierarchy.
func classifyLeadParseError(err error) (outcome, errorClass string) {
	if err == nil {
		return string(stepoutcome.OK), ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "invalid JSON from lead agent"):
		return string(stepoutcome.ParseError), stepoutcome.ClassParseInvalidJSON
	case strings.Contains(msg, "lead agent refused to plan"):
		return string(stepoutcome.Refused), stepoutcome.ClassParsePlanRefused
	case strings.Contains(msg, "plan contains no steps"),
		strings.Contains(msg, "empty result from lead agent"):
		return string(stepoutcome.SchemaViolation), stepoutcome.ClassParsePlanNoSteps
	default:
		return string(stepoutcome.ParseError), stepoutcome.ClassParseInvalidJSON
	}
}

// buildPlanningPromptWithContext constructs the lead agent's
// planning prompt by injecting a role catalog (names +
// descriptions), format instructions, and (Phase 25+) the
// conversational context (scratchpad + recent thread).
//
// scratchpad and messages may be nil — legacy deployments without
// the conversational lifecycle wiring see no behavioural change.
// the lead's running scratchpad + recent conversation messages
// (Phase 25+). Each is optional. Caller fetches them from the
// repos and passes them in; this function handles formatting.
func buildPlanningPromptWithContext(
	stepPrompt string,
	swarm *registry.Swarm,
	scratchpad *persistence.TaskScratchpad,
	messages []*persistence.TaskMessage,
	recoveryCtx *RecoveryContext,
) string {
	var b strings.Builder

	// Recovery banner (2026-05-26): when a prior step failed and the
	// workflow routed to this plan step, the lead's ONE job is to
	// propose alternatives via outcome=checkpoint. We prepend a
	// high-salience banner ahead of every other context so the model
	// can't miss it, and the format spec below prunes `continue` from
	// the option menu — the lead literally can't pick what it doesn't
	// see. Observed pre-fix on T-0833: the lead emitted continue+plan
	// in recovery mode and the recover step terminally failed with
	// "recovery contract violation."
	if recoveryCtx != nil {
		b.WriteString("⚠️ RECOVERY MODE — read carefully.\n\n")
		fmt.Fprintf(&b, "A prior step failed and the workflow routed to you to keep the task alive:\n  failed_step:    %s\n  failure_class:  %s\n", recoveryCtx.FailedStep, recoveryCtx.FailureClass)
		if recoveryCtx.FailureReason != "" {
			fmt.Fprintf(&b, "  failure_reason: %s\n", truncateForPrompt(recoveryCtx.FailureReason, 600))
		}
		if len(recoveryCtx.BlockedURLs) > 0 {
			b.WriteString("  blocked_urls:\n")
			for _, blk := range recoveryCtx.BlockedURLs {
				fmt.Fprintf(&b, "    - %s (reason: %s)\n", blk.URL, blk.Reason)
			}
		}
		if block := learnedRemediationsBlock(recoveryCtx.LearnedRemediations); block != "" {
			// Advisory continuous-learning overlay: worker-mined signals on
			// what has historically resolved THIS failure class in THIS
			// project. Surfaced as evidence the lead weighs when proposing
			// alternatives — never as an instruction to auto-apply.
			b.WriteString("\nSimilar failures previously resolved here (advisory — weigh, don't blindly repeat):\n")
			for _, r := range recoveryCtx.LearnedRemediations {
				fmt.Fprintf(&b, "  - %s (confidence %.2f, %d resolved / %d regressed)\n",
					r.Action, r.Confidence, r.SupportCount, r.ContradictCount)
			}
		}
		b.WriteString("\nYour ONE job: propose 1–3 viable alternative approaches to the operator via outcome=checkpoint, kind=decision. ")
		b.WriteString("The `continue` outcome is FORBIDDEN here — re-spawning the failed role would just repeat the failure.\n\n")
	}

	if stepPrompt != "" {
		b.WriteString(stepPrompt)
		b.WriteString("\n\n")
	}

	// Phase 25+ — conversational context. When the task has
	// accumulated history under the new lifecycle, surface the
	// lead's running summary + the recent thread so the next
	// execution can act on what the operator said since last
	// time. Operator amendments and answers are the most common
	// reason a fresh execution fires.
	if scratchpad != nil && (scratchpad.Summary != "" || scratchpad.CurrentPhase != nil) {
		b.WriteString("=== Lead's running summary (your prior view of this task) ===\n")
		if scratchpad.CurrentPhase != nil && *scratchpad.CurrentPhase != "" {
			fmt.Fprintf(&b, "Current phase: %s\n", *scratchpad.CurrentPhase)
		}
		if scratchpad.Summary != "" {
			b.WriteString(scratchpad.Summary)
			b.WriteString("\n")
		}
		if len(scratchpad.OpenQuestions) > 0 {
			b.WriteString("Open questions:\n")
			b.WriteString(string(scratchpad.OpenQuestions))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if len(messages) > 0 {
		// Phase 32 — filter out messages that have been compressed
		// into a thread_summary `note`. Walk every note's metadata
		// for summarized_message_ids and build a hide-set; the
		// summary itself stays in the rendered thread (so the lead
		// can see what the older content was about) while the
		// originals drop out.
		hide := summarizedMessageIDs(messages)
		visible := make([]*persistence.TaskMessage, 0, len(messages))
		for _, m := range messages {
			if hide[m.ID] {
				continue
			}
			visible = append(visible, m)
		}

		b.WriteString("=== Conversation thread (most recent last) ===\n")
		// Cap at last 30 visible messages to bound prompt size;
		// older content lives in scratchpad summary or in the
		// thread_summary notes still rendered above.
		start := 0
		if len(visible) > 30 {
			start = len(visible) - 30
		}
		for _, m := range visible[start:] {
			fmt.Fprintf(&b, "[%s] %s (%s): %s\n",
				m.CreatedAt.Format("2006-01-02 15:04"),
				m.AuthorKind, m.MessageKind, oneLine(m.Content))
		}
		b.WriteString("\n")
		b.WriteString("Precedence when reconciling: most-recent directive > most-recent answer > scratchpad > older messages.\n")
		if len(hide) > 0 {
			fmt.Fprintf(&b, "Note: %d older message(s) were compressed by summarize_thread; their content is in the `note` summaries above.\n", len(hide))
		}
		b.WriteString("\n")
	}

	if swarm != nil && len(swarm.Roles) > 0 {
		b.WriteString("ROLES AVAILABLE IN THIS SWARM — these are the ONLY names you may use:\n")
		for _, r := range swarm.Roles {
			desc := r.Description
			if desc == "" {
				desc = "(no description)"
			}
			fmt.Fprintf(&b, "  - %s: %s\n", r.Name, desc)
		}
		b.WriteString("\n")
	}

	if recoveryCtx != nil {
		// Recovery-mode menu — `continue` is pruned. The model can't
		// pick what it doesn't see, which is stronger than telling it
		// "don't pick continue" and hoping the instruction sticks.
		b.WriteString(`Respond with ONLY a JSON object — no other text before or after.

You may emit one of THREE outcome shapes in recovery mode. Pick exactly one.
(Note: the "continue" outcome from the normal menu is NOT available here.
The workflow already tried to continue and it failed; you must propose
alternatives or signal a non-continue outcome.)

(1) Checkpoint — propose alternatives for the operator (DEFAULT):
   {"outcome":"checkpoint", "checkpoint":{"kind":"decision", "question":"<one-sentence summary of what failed + ask>", "options":[{"id":"<short-token>","label":"<human-readable proposal, 1 line>"}, ...], "default_if_no_response":"abort"}, "message":"...", "scratchpad_update":{...}}
     - decision: 1-3 viable alternatives + always include "abort with explanation"
     - action_required: { "kind":"action_required", "task_for_human":"...", "expected_format":"..." }
     - review: { "kind":"review", "draft":"..." }
   This is what you'll use 95% of the time in recovery — see your role's playbook for per-failure-class proposals.

(2) External wait — the failure will self-resolve at a known deadline:
   {"outcome":"external_wait", "external_wait":{"expected_by":"2026-06-01T12:00:00Z","reason":"vendor backend recovers nightly"}, "message":"..."}
   Use ONLY when the failure is a known transient and a real-world deadline will clear it.

(3) Closure request — the failure means the task itself should be closed:
   {"outcome":"closure_request", "closure_request":{"summary":"can't proceed; recommend close"}, "message":"..."}
   Use only when no alternative is viable and the operator should confirm close.

Rules:
- scratchpad_update is OPTIONAL but encouraged on every outcome (cap summary at 4 KB).
- DO NOT emit outcome=continue. DO NOT emit a "plan" key. The recover step
  is forbidden from spawning role steps — that's why you were called.
- DO NOT omit the "outcome" field. Empty/missing outcome maps to continue and
  fails the recovery contract.`)
	} else {
		b.WriteString(`Respond with ONLY a JSON object — no other text before or after.

You may emit one of FOUR outcome shapes. Pick exactly one.

(1) Continue with a plan — the legacy / default shape:
   {"outcome":"continue", "plan":{"steps":["role1","role2"],"rationale":"why","phase":"optional"}, "message":"brief summary", "scratchpad_update":{"summary":"...","current_phase":"..."}}
   Use this when you have enough information to make progress and the
   spawned roles can execute without further operator input.

(2) Checkpoint — block the task on operator input:
   {"outcome":"checkpoint", "checkpoint":{"kind":"decision|action_required|review", ...}, "message":"...", "scratchpad_update":{...}}
     - decision: { "kind":"decision", "question":"...", "options":[{"id":"a","label":"..."},{"id":"b","label":"..."}], "default_if_no_response":"a" (optional) }
     - action_required: { "kind":"action_required", "task_for_human":"...", "expected_format":"..." }
     - review: { "kind":"review", "draft":"..." }
   Use this when you need a human decision, real-world action, or approval before proceeding.

(3) External wait — wait on a real-world event with a deadline:
   {"outcome":"external_wait", "external_wait":{"expected_by":"2026-06-01T12:00:00Z","reason":"vendor will respond"}, "message":"..."}
   Use this when no operator action is needed but a deadline must pass first
   (vendor reply window, scheduled date, etc.).

(4) Closure request — recommend operator-confirmed close:
   {"outcome":"closure_request", "closure_request":{"summary":"installation verified, paid, archived"}, "message":"..."}
   Use this when you believe the task is complete and want the operator to
   confirm closure.

Rules:
- For continue, steps MUST contain ONLY role names from the catalog above.
- The scratchpad_update field is OPTIONAL but encouraged on every outcome —
  it's how you preserve context for the NEXT execution. Cap summary at 4 KB.
- Order in continue.steps matters: each role receives the previous role's output as context.
- minimum 1 step, maximum 8 steps for continue
- For backwards compatibility you may omit "outcome" and emit just
  {"plan":{...},"message":"..."} — that maps to outcome=continue.`)
	}

	return b.String()
}

// oneLine collapses newlines for compact prompt rendering.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	if len(s) > 500 {
		s = s[:497] + "..."
	}
	return s
}

// summarizedMessageIDs walks every `note` message and collects
// the message_ids referenced in its metadata.summarized_message_ids
// field. The result is a hide-set the prompt builder filters by.
//
// Phase 32: lead calls summarize_thread to compress an older span
// into a single summary `note`. The `note` itself stays in the
// rendered thread (so the lead sees what was summarized), while
// the original messages drop out of the prompt window.
func summarizedMessageIDs(messages []*persistence.TaskMessage) map[string]bool {
	hide := make(map[string]bool)
	for _, m := range messages {
		if m == nil || m.MessageKind != persistence.TaskMessageKindNote || len(m.Metadata) == 0 {
			continue
		}
		var meta struct {
			SummarizedIDs []string `json:"summarized_message_ids"`
		}
		if err := json.Unmarshal(m.Metadata, &meta); err != nil {
			continue
		}
		for _, id := range meta.SummarizedIDs {
			hide[id] = true
		}
	}
	return hide
}

// planRoleResolution captures the post-validation view of a
// lead's plan: what was kept, what was substituted via an
// alias, what was dropped because no role matched, and any
// alias collisions encountered while building the lookup.
//
// Pure-data return so the helper is unit-testable without
// standing up an executor + zerolog harness.
type planRoleResolution struct {
	valid       []string                 // canonical names, in plan order
	substituted []string                 // "editor→writer" pairs, log-only
	dropped     []string                 // names with no canonical + no alias
	collisions  []planRoleAliasCollision // duplicate alias points across roles
}

type planRoleAliasCollision struct {
	alias           string
	firstRole       string
	conflictingRole string
}

// resolvePlanRoles maps the lead's named roles onto the swarm
// catalog, substituting aliases for hallucinated synonyms.
// First-seen wins on alias collisions (the operator-side
// failure mode "two roles claim the same alias" is logged via
// the collision list and the operator must rename one).
func resolvePlanRoles(planSteps []string, roles []registry.SwarmRole) planRoleResolution {
	roleSet := make(map[string]struct{}, len(roles))
	aliasMap := make(map[string]string)
	var collisions []planRoleAliasCollision
	for _, r := range roles {
		roleSet[r.Name] = struct{}{}
		for _, alias := range r.Aliases {
			if alias == "" || alias == r.Name {
				continue
			}
			if existing, dup := aliasMap[alias]; dup && existing != r.Name {
				collisions = append(collisions, planRoleAliasCollision{
					alias:           alias,
					firstRole:       existing,
					conflictingRole: r.Name,
				})
				continue
			}
			aliasMap[alias] = r.Name
		}
	}
	res := planRoleResolution{collisions: collisions}
	res.valid = make([]string, 0, len(planSteps))
	for _, name := range planSteps {
		if _, ok := roleSet[name]; ok {
			res.valid = append(res.valid, name)
			continue
		}
		if canonical, ok := aliasMap[name]; ok {
			res.valid = append(res.valid, canonical)
			res.substituted = append(res.substituted, name+"→"+canonical)
			continue
		}
		res.dropped = append(res.dropped, name)
	}
	return res
}

// parsePlanSteps extracts the ordered role list and coordinator message from a
// lead agent result JSON. The message is forwarded to the first planned role.
func parsePlanSteps(data []byte) ([]string, string, error) {
	if len(data) == 0 {
		return nil, "", fmt.Errorf("empty result from lead agent")
	}
	var envelope struct {
		Plan struct {
			Steps []string `json:"steps"`
		} `json:"plan"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, "", fmt.Errorf("invalid JSON from lead agent: %w", err)
	}
	if len(envelope.Plan.Steps) > 0 {
		return envelope.Plan.Steps, stripReasoning(envelope.Message), nil
	}
	// Fallback: the LLM may have embedded plan JSON inside its message field
	// (e.g. mixed prose + JSON). Try to extract a JSON object from that text.
	if envelope.Message != "" {
		if steps, msg, err := extractPlanFromText(envelope.Message); err == nil {
			return steps, msg, nil
		}
		// Empty steps + non-empty message is a deliberate refusal by the
		// coordinator (e.g. a missing prerequisite). Surface the message
		// so the operator sees why, instead of a generic "no steps" error.
		return nil, "", fmt.Errorf("lead agent refused to plan: %s", stripReasoning(envelope.Message))
	}
	return nil, "", fmt.Errorf("lead agent plan contains no steps")
}

// extractPlanFromText scans raw text for an embedded JSON object containing
// {"plan":{"steps":[...]}} and returns the extracted steps and message.
func extractPlanFromText(text string) ([]string, string, error) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start == -1 || end <= start {
		return nil, "", fmt.Errorf("no JSON object in text")
	}
	var envelope struct {
		Plan struct {
			Steps []string `json:"steps"`
		} `json:"plan"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(text[start:end+1]), &envelope); err != nil {
		return nil, "", fmt.Errorf("embedded JSON parse failed: %w", err)
	}
	if len(envelope.Plan.Steps) == 0 {
		return nil, "", fmt.Errorf("embedded plan has no steps")
	}
	return envelope.Plan.Steps, stripReasoning(envelope.Message), nil
}

// missingDeclaredOutputs returns the list of paths the agent named in
// result.outputArtifacts that don't actually exist on disk. A claim
// that doesn't match reality gets surfaced as the producer's failure
// (SchemaViolation/ClassMissingOutput in the caller) so the next role
// doesn't silently run against a half-empty workspace.
//
// Path resolution:
//   - Absolute paths under /app/workspace are stripped of that prefix
//     and re-rooted on the host's worktreeDir — agents always see the
//     container-internal path in result.json.
//   - Relative paths are resolved against worktreeDir directly.
//
// Returns nil when every declared path exists OR when no outputArtifacts
// were declared at all (roles like scout that produce only a message
// are legitimate zero-artifact steps).
func (e *Executor) missingDeclaredOutputs(resultBytes []byte, worktreeDir string) []string {
	if len(resultBytes) == 0 || worktreeDir == "" {
		return nil
	}
	var parsed struct {
		OutputArtifacts []struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"outputArtifacts"`
	}
	if err := json.Unmarshal(resultBytes, &parsed); err != nil {
		// Result isn't JSON or is truncated — let the caller's normal
		// classification path deal with it; missingDeclaredOutputs is
		// a structural check, not a parse validator.
		return nil
	}
	if len(parsed.OutputArtifacts) == 0 {
		return nil
	}
	var missing []string
	for _, a := range parsed.OutputArtifacts {
		if a.Path == "" {
			continue
		}
		hostPath := resolveClaimedPath(a.Path, worktreeDir, worktreeDir)
		if hostPath == "" {
			missing = append(missing, a.Path)
			continue
		}
		if _, err := os.Stat(hostPath); err != nil {
			missing = append(missing, a.Path)
		}
	}
	return missing
}
