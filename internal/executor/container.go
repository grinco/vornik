package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"vornik.io/vornik/internal/budget"
	"vornik.io/vornik/internal/counterfactual"
	"vornik.io/vornik/internal/hallucination"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/runtime"
	"vornik.io/vornik/internal/safepath"
	"vornik.io/vornik/internal/stepoutcome"
	"vornik.io/vornik/internal/taintlineage"
	"vornik.io/vornik/internal/toolbudget"
	"vornik.io/vornik/internal/verifier"
)

// warmPoolStaticKeyWarnOnce deduplicates the "warm-pool role runs on static
// key" WARN log to one emission per (project, role) per process lifetime.
// Key format: projectID + "/" + roleName.
var warmPoolStaticKeyWarnOnce sync.Map

// shouldWarnWarmStaticKey returns true the first time it is called for a given
// (project, role) pair within the process lifetime, false on every subsequent
// call. It is the warn-dedup gate for the "warm-pool role runs on static key"
// log line; extracted so unit tests can exercise the package-level map directly.
func shouldWarnWarmStaticKey(project, role string) bool {
	key := project + "/" + role
	_, alreadyWarned := warmPoolStaticKeyWarnOnce.LoadOrStore(key, struct{}{})
	return !alreadyWarned
}

type retryableError struct {
	err error
}

func (e retryableError) Error() string {
	return e.err.Error()
}

func (e retryableError) Unwrap() error {
	return e.err
}

func markRetryable(err error) error {
	if err == nil {
		return nil
	}
	return retryableError{err: err}
}

// injectCostEnv writes per-million-token input/output prices into the env
// map so the agent container can log per-iteration cost hints. A nil
// pricing table or a model absent from the table writes "0" — the agent
// side treats zeros as "no cost data" and skips the dollar component of
// its log line. The model argument is whatever the container will report
// as VORNIK_LLM_MODEL (role override, envVar, or global default).
func injectCostEnv(env map[string]string, table *pricing.Table, model string) {
	if env == nil || table == nil || model == "" {
		return
	}
	entry, _ := table.Lookup(model)
	env["VORNIK_LLM_COST_INPUT_PER_M"] = strconv.FormatFloat(entry.InputUSDPerMillion, 'f', -1, 64)
	env["VORNIK_LLM_COST_OUTPUT_PER_M"] = strconv.FormatFloat(entry.OutputUSDPerMillion, 'f', -1, 64)
}

// injectBudgetEnv hands the agent a snapshot of the project's remaining
// USD envelope so the in-container tool loop can self-throttle before
// the next LLM call would breach the cap. Pairs with the per-million
// price env vars from injectCostEnv: the agent multiplies its running
// token usage by the prices, compares the projected next-call cost to
// the smaller of (daily, monthly) remaining, and bails with the
// budget_tripwire outcome if the next call wouldn't fit.
//
// Snapshot semantics: budget is read once per step, not per LLM call.
// A long step can over-shoot by one call's worth — that's the
// deliberate trade-off for keeping the agent loop free of daemon
// round-trips. The post-task tripwire/dispatch gate (budget.Check on
// task creation) catches anything the per-step snapshot missed.
//
// Skips silently when:
//   - env or project is nil (defensive — no-op if caller misuses it)
//   - the project has no hard caps configured (Budget caps default to 0,
//     which budget.Check treats as "uncapped" — agent has nothing to
//     compare against, so we leave the env vars unset)
//   - usageRepo is nil (no spend data; can't compute remaining)
//   - budget.Check errors (logged by caller; agent treats absent env
//     vars as "no budget enforcement", same as the uncapped case)
//
// Both *_REMAINING_USD vars are written together when caps exist on
// both periods, so the agent sees the tighter envelope. A value of 0
// means "exhausted" — the agent should bail before the very next call.
func injectBudgetEnv(ctx context.Context, env map[string]string, usageRepo budget.Repo, project *registry.Project, now time.Time) (budget.Decision, error) {
	if env == nil || project == nil || usageRepo == nil {
		return budget.Decision{}, nil
	}
	b := project.Budget
	if b.DailyHardUSD == 0 && b.MonthlyHardUSD == 0 {
		// No hard caps. Soft caps don't gate the agent — they're a
		// notification trigger only. Leave env clean so the agent
		// short-circuits its in-loop check.
		return budget.Decision{}, nil
	}

	decision, err := budget.Check(ctx, usageRepo, project, now)
	if err != nil {
		return decision, err
	}
	if b.DailyHardUSD > 0 {
		remaining := b.DailyHardUSD - decision.DailyUSD
		if remaining < 0 {
			remaining = 0
		}
		env["VORNIK_BUDGET_DAILY_REMAINING_USD"] = strconv.FormatFloat(remaining, 'f', 4, 64)
	}
	if b.MonthlyHardUSD > 0 {
		remaining := b.MonthlyHardUSD - decision.MonthlyUSD
		if remaining < 0 {
			remaining = 0
		}
		env["VORNIK_BUDGET_MONTHLY_REMAINING_USD"] = strconv.FormatFloat(remaining, 'f', 4, 64)
	}
	return decision, nil
}

func (e *Executor) effectiveRoleModel(roleConfig *registry.SwarmRole) string {
	if roleConfig == nil {
		return ""
	}
	if roleConfig.Model != "" {
		return roleConfig.Model
	}
	if roleConfig.Runtime.EnvVars != nil && roleConfig.Runtime.EnvVars["VORNIK_LLM_MODEL"] != "" {
		return roleConfig.Runtime.EnvVars["VORNIK_LLM_MODEL"]
	}
	if e != nil && e.config.AgentLLMEnv != nil {
		return e.config.AgentLLMEnv["VORNIK_LLM_MODEL"]
	}
	return ""
}

// effectiveRoleModelForTask is effectiveRoleModel with one extra
// precedence step: a counterfactual-replay task may carry a model
// override in its payload (Phase C v1's
// context.counterfactual.{role_model_override,model_override_all_roles}).
// Overrides win over the role's config so `vornikctl blackbox replay
// --variable model --value X` actually runs the new task with X.
//
// Tasks without a counterfactual block see no behaviour change —
// ExtractPayloadOverrides returns the zero value and ResolveModel
// returns empty, falling through to the native resolution.
func (e *Executor) effectiveRoleModelForTask(task *persistence.Task, roleConfig *registry.SwarmRole) string {
	if task != nil {
		// Operator-forced override (the "retry on fallback model"
		// action / steer keyword) wins, and unlike the counterfactual
		// block below it does NOT flag the task as a replay — the run
		// is real and its side effects must fire.
		if override := operatorModelOverride(task.Payload, roleConfig.Name); override != "" {
			return override
		}
		if override := counterfactual.ExtractPayload(task.Payload).ResolveModel(roleConfig.Name); override != "" {
			return override
		}
	}
	return e.effectiveRoleModel(roleConfig)
}

// refineAgentFailureOutcome recovers the agent's own diagnosis from a failed
// step's error text, so the ledger records WHY the container exited rather
// than only that it did.
//
// WHY THIS EXISTS. The agent diagnoses itself precisely — entrypoint.sh names
// the looping tool and repeat count, the cap path names the cap, the overflow
// path names the window — and all of it was being discarded. The 2026-08-16
// long-horizon arm produced 73 failed steps, every one recorded as
// failed/container_non_zero_exit, and the outcomes `degenerate_loop` and
// `iteration_exhausted` had NEVER been written anywhere in the bench DB.
//
// The handling was not missing. The defer's degenerate-loop arm is an
// `else if degenerateLoopDetail != ""` that requires err == nil — its comment
// reads "Container exit was clean but the tool loop got stuck". But the guard
// ends `write_result "FAILED" … ; return 1`, so the container always exits
// non-zero, err != nil, and the first branch consumes the case. The else-if is
// unreachable for the failure it names. This refinement therefore sits INSIDE
// the err != nil path rather than beside it.
//
// MATCH THE SPECIFIC PHRASE, IN PRECEDENCE ORDER. Bare keywords do not work
// here, for two independent reasons:
//
//   - error_detail has the container log appended, and those lines contain
//     "context_size=…". A `context` keyword match therefore captures
//     iteration-cap failures as context overflows. That artefact is what made
//     an earlier analysis report "iteration cap: zero rows" when the true
//     figure was 4.
//   - The degenerate-loop message DISCUSSES the context window in prose
//     ("Context was only 11% full … so this is NOT context exhaustion"), so it
//     must be matched before any context rule.
//
// Matching is case-insensitive deliberately: this function only picks a label,
// so tolerating a capitalisation change is free. classifyShapeFailure is the
// opposite case — it routes the corrective retry, matches case-sensitively,
// and its message contract is pinned by test (incident T-1089). Do not
// harmonise the two.
//
// Unrecognised text returns today's values, so this is purely additive.
func refineAgentFailureOutcome(detail string) (stepoutcome.Outcome, string) {
	lowered := strings.ToLower(detail)
	switch {
	case strings.Contains(lowered, "plausibility violation"):
		return stepoutcome.SchemaViolation, stepoutcome.ClassPlausibilityViolation
	case strings.Contains(lowered, "degenerate loop"):
		return stepoutcome.DegenerateLoop, stepoutcome.ClassDegenerateLoop
	case strings.Contains(lowered, "tool iteration limit"):
		return stepoutcome.IterationExhausted, stepoutcome.ClassIterationCap
	case strings.Contains(lowered, "context window"):
		// Reached only after the degenerate-loop case above has claimed the
		// messages that merely mention the window.
		return stepoutcome.Failed, stepoutcome.ClassContextOverflow

	// --- wave-2 arms. APPENDED, never interleaved: the four arms above are
	// ordered against two documented misreadings and that ordering is pinned
	// by test. A message matching both an old and a new arm takes the old one,
	// which is correct — the old arms are the more specific reading (a
	// provider error that mentions the context window IS a context overflow).
	case strings.Contains(lowered, "llm call failed"):
		return stepoutcome.Failed, stepoutcome.ClassLLMCallFailed
	case strings.Contains(lowered, "missing prerequisite"):
		return stepoutcome.Failed, stepoutcome.ClassMissingPrerequisite
	// `signal: killed` BEFORE `podman wait failed`: the killed rows are a
	// strict subset of the wait rows (the literal contains both), so the
	// generic arm would otherwise swallow every kill.
	case strings.Contains(lowered, "signal: killed"):
		return stepoutcome.Failed, stepoutcome.ClassContainerKilled
	case strings.Contains(lowered, "podman wait failed"):
		return stepoutcome.Failed, stepoutcome.ClassContainerWaitFailed
	case strings.Contains(lowered, "failed to start container"),
		strings.Contains(lowered, "podman run failed"):
		return stepoutcome.Failed, stepoutcome.ClassContainerStartFailed

	default:
		return stepoutcome.Failed, stepoutcome.ClassUnclassified
	}
}

// timeoutOutcomeAndClass decides the (outcome, errClass) pair for a step whose
// wall clock ran out.
//
// A deadline says WHEN a step died, not WHY. A step can hit its timeout while
// failing for a cause the agent named in result.json — a degenerate loop that
// spins until the deadline being the common shape — and that cause is the more
// useful of the two facts.
//
// It is also the actionable one. persistence.TimeoutStepOutcomes is documented
// as "the degraded outcomes that mean TRUNCATION — the only ones a timeout raise
// can address", so filing a degenerate loop under timeout tells that machinery
// the remedy is more wall clock, which just buys the loop longer to spin. The
// label selects a remediation, so a wrong label selects a wrong one.
//
// A genuine timeout — nothing named — keeps the timeout pair, because that is
// the one case a timeout raise legitimately addresses. refineAgentFailureOutcome
// signals "nothing recognised" with the ClassUnclassified
// fallback, which is what makes this distinguishable.
func timeoutOutcomeAndClass(err error) (string, string) {
	if refined, refinedClass := refineAgentFailureOutcome(errorBeforeLogTail(err.Error())); refinedClass != stepoutcome.ClassUnclassified {
		return string(refined), refinedClass
	}
	return string(stepoutcome.Timeout), stepoutcome.ClassContextTimeout
}

// classifyStepOutcome maps an agent step's (ctx, err) to the outcome label
// used by vornik_executor_agent_step_outcomes_total. Context cancellation
// wins over err — a cancelled run that incidentally also returns an error
// is still "cancelled", not "failed".
func classifyStepOutcome(ctx context.Context, err error) string {
	if ctx != nil {
		switch ctx.Err() {
		case context.Canceled:
			return "cancelled"
		case context.DeadlineExceeded:
			return "timeout"
		}
	}
	if err == nil {
		return "success"
	}
	// Strip the container-log tail before matching: this switch keys on the bare
	// word "timeout", which an agent log contains routinely (`curl: (28)
	// operation timeout`), so a step that merely exited non-zero would be
	// recorded as a timeout. See containerLogDelimiter.
	msg := errorBeforeLogTail(err.Error())
	if strings.Contains(msg, "deadline exceeded") || strings.Contains(msg, "timed out") || strings.Contains(msg, "timeout") {
		return "timeout"
	}
	// Distinguish shape failures from generic step failures so the
	// schema-compliance dashboards can attribute "missing required
	// keys" vs "JSON malformed" to a specific model. Pre-fix every
	// such error landed in the "failed" bucket and was invisible.
	// The string predicates mirror the error-message contracts in
	// container.go (schema violation:) and plan_step.go (could not
	// parse plan from / invalid JSON), keeping the classifier
	// independent of the producer's error-wrapping choices.
	if strings.Contains(msg, "schema violation:") || strings.Contains(msg, "is missing required keys") {
		return "schema_violation"
	}
	if strings.Contains(msg, "could not parse plan from") || strings.Contains(msg, "invalid JSON") || strings.Contains(msg, "invalid character") {
		return "parse_error"
	}
	return "failed"
}

// executeAgentStep runs a single agent step inside a container.
func (e *Executor) executeAgentStep(ctx context.Context, task *persistence.Task, execution *persistence.Execution, plan *executionPlan, stepID string, step registry.WorkflowStep, timeout time.Duration, opts *agentInputOpts) (_ string, _ []byte, err error) {
	// Record the per-step outcome on return so the effective-cost view
	// has an attempts-vs-successes signal per role+model. effectiveModel
	// is populated after role assignment; pre-assignment failures keep an
	// empty model label.
	//
	// degenerateLoopDetail is populated by persistToolAuditFromResult
	// during the step body when the audit scan spots 3+ consecutive
	// identical tool calls. When non-empty on a successful step, the
	// defer writes degenerate_loop instead of pending_validation so the
	// quality signal doesn't get lost to an overzealous "ok" finalize
	// from the consumer.
	var roleConfig *registry.SwarmRole
	effectiveModel := ""
	var degenerateLoopDetail string
	// budgetTripwireDetail mirrors degenerateLoopDetail — populated by
	// the result.json parser below when the agent self-aborted to stay
	// within the project's remaining budget envelope. The defer reads
	// it to override pending_validation with budget_tripwire so the
	// quality view doesn't credit a tripwire bail as a clean step.
	var budgetTripwireDetail string
	// promptTokenBudgetDetail is the prompt-token analogue of the
	// dollar budget tripwire: clean agent exit, but not an unconstrained
	// OK because the wrapper forced a tool-free finalization answer.
	var promptTokenBudgetDetail string
	// iterationCapDetail is the tool-budget analogue of the two above: the
	// agent exhausted its tool-call cap, produced a real answer on a forced
	// tool-free turn, and exited cleanly. The workflow must not take a failure
	// transition, but the quality row must not read ok either.
	var iterationCapDetail string
	// agentStamp carries the migration-106 budget columns stamped on the
	// outcome row for agent steps only. Populated from the resolved budget
	// (resolveRoleToolBudget) and the tool-audit count (persistToolAuditFromResult);
	// left zero so the three columns stay NULL for non-agent paths.
	var agentStamp agentBudgetStamp
	// taintStamp carries the migration-137 taint-lineage columns stamped on the
	// outcome row for agent steps only. Populated from persistToolAuditFromResult's
	// StepTaint classification at both the warm and ephemeral call sites; left
	// zero (untainted) if no tool audit was produced. The defer reads it so the
	// columns land regardless of whether the step succeeded or failed.
	var taintStampVal taintStamp
	// hallucinationSignalsBlob is set by the post-step detector hook
	// later in this function. Captured into the closure so the defer
	// can pass it to the outcome row regardless of whether the step
	// succeeded, failed verification, or hallucinated. Stored as raw
	// bytes (already JSON-marshalled) to keep the persistence layer
	// hallucination-package-free.
	var hallucinationSignalsBlob []byte
	// hallucinationDetail is the human-readable reason persisted on
	// the outcome row when High signals fail the step. Without it,
	// the row would just say "agent claimed N file(s) but verification
	// failed: ..." even though the actual failure was an unsupported
	// URL/ID claim — the operator's dashboard needs to distinguish.
	var hallucinationDetail string
	stepStartedAt := time.Now()
	defer func() {
		// Persist the richer outcome taxonomy. Success path writes
		// pending_validation (or degenerate_loop when the detector
		// fired). Failure paths write the terminal outcome directly.
		// recordStepOutcome emits the Prometheus event for terminal
		// outcomes — we deliberately don't call RecordAgentStepOutcome
		// here because that would double-count the counter once
		// finalizePendingOutcome fires on the consumer side.
		outcome := string(stepoutcome.PendingValidation)
		errClass := ""
		errDetail := ""
		if err != nil {
			switch classifyStepOutcome(ctx, err) {
			case "timeout":
				// A named cause outranks the wall clock; see
				// timeoutOutcomeAndClass.
				outcome, errClass = timeoutOutcomeAndClass(err)
			case "cancelled":
				outcome = string(stepoutcome.Cancelled)
				errClass = stepoutcome.ClassContextCancelled
			case "schema_violation":
				outcome = string(stepoutcome.SchemaViolation)
				errClass = stepoutcome.ClassVerifyFailed
			case "parse_error":
				outcome = string(stepoutcome.ParseError)
				errClass = stepoutcome.ClassVerifyFailed
			default:
				// The agent names its own cause in result.json, and that
				// text reaches us in err. Recover it rather than recording
				// only "the container exited non-zero" — the whole
				// 2026-08-16 long-horizon arm was one indistinguishable
				// bucket because this was thrown away. Falls back to
				// Failed/container_non_zero_exit for anything unrecognised,
				// so the change is additive.
				refinedOutcome, refinedClass := refineAgentFailureOutcome(errorBeforeLogTail(err.Error()))
				outcome = string(refinedOutcome)
				errClass = refinedClass
			}
			errDetail = err.Error()
			// Hallucination-driven failures get a distinct error class
			// so the dashboard can group them apart from generic
			// container failures. The detector populates
			// hallucinationDetail in tandem with err so this branch
			// fires only when the detector chose to fail the step.
			if hallucinationDetail != "" {
				errClass = stepoutcome.ClassHallucinated
				errDetail = hallucinationDetail
			}
		} else if degenerateLoopDetail != "" {
			// Container exit was clean but the tool loop got stuck in a
			// repeated call pattern. Quality failure of the step itself —
			// not attributed to an upstream producer.
			outcome = string(stepoutcome.DegenerateLoop)
			errClass = stepoutcome.ClassDegenerateLoop
			errDetail = degenerateLoopDetail
		} else if budgetTripwireDetail != "" {
			// Agent voluntarily bailed before its next LLM call would
			// have breached the project's remaining budget envelope.
			// Step exit was clean (status=COMPLETED in result.json) so
			// the workflow doesn't take an OnFail transition — but the
			// quality signal must reflect that this wasn't usable
			// output, just an early stop. Operators see budget_tripwire
			// rows on the dashboard and know to either widen the cap
			// or break the work into smaller tasks.
			outcome = string(stepoutcome.BudgetTripwire)
			errClass = stepoutcome.ClassBudgetTripwire
			errDetail = budgetTripwireDetail
		} else if iterationCapDetail != "" {
			outcome = string(stepoutcome.IterationExhausted)
			errClass = stepoutcome.ClassIterationCap
			errDetail = iterationCapDetail
		} else if promptTokenBudgetDetail != "" {
			// Agent voluntarily stopped the tool loop before prompt
			// replay crossed the configured per-step token ceiling.
			// The wrapper already made one tool-free finalization call,
			// so the workflow may continue, but metrics should not
			// count it as an unconstrained OK.
			outcome = string(stepoutcome.PromptTokenBudget)
			errClass = stepoutcome.ClassPromptTokenBudget
			errDetail = promptTokenBudgetDetail
		}
		durMS := time.Since(stepStartedAt).Milliseconds()
		// Carry the container's exit status onto the row. nil when the step
		// did not fail in a container, which stays NULL rather than claiming
		// a clean exit — see migration 169 and containerExitCodeFromError.
		agentStamp.ContainerExitCode = containerExitCodeFromError(err)
		e.recordStepOutcomeWithSignalsAndBudget(ctx, task, execution, stepID, step.Role, effectiveModel, outcome, errClass, errDetail, nil, &durMS, hallucinationSignalsBlob, agentStamp, taintStampVal)
	}()

	// Used by verifyClaimedModifications as the mtime floor: any file the
	// agent claims to have modified must have an mtime >= stepStart (minus
	// a 1s slack for sub-second FS precision). Captured before anything
	// else so the check is strict.
	stepStart := time.Now()

	// Pre-sample git HEAD so the deception verifier downstream can
	// compare claimed files_changed against the real diff count. Empty
	// when the project isn't a git repo or the worktree path isn't
	// resolved yet — verifyRoleClaims falls through gracefully on either.
	// Plan-step path samples its own pre/post HEAD pair separately;
	// this is the regular-agent-step counterpart for dev-pipeline et al.
	preStepHEAD := ""
	if plan != nil && plan.worktreeDir != "" {
		preStepHEAD = gitHEAD(ctx, plan.worktreeDir)
	}

	// effectiveProjectDir is the host path that corresponds to
	// /app/workspace/project/ inside the container. Worktree mode
	// points at the per-task worktree; non-worktree fallback points
	// at the project's persistent workspace root. persistArtifacts
	// walks <effectiveProjectDir>/artifacts/out to capture deliverables
	// the writer dropped into the project tree (the previous
	// `workspaceDir/project/...` path was a no-op because the bind
	// mount only exists inside the container's namespace).
	effectiveProjectDir := ""
	if plan != nil {
		effectiveProjectDir = plan.worktreeDir
	}
	if effectiveProjectDir == "" && e.config.ProjectWorkspacePath != "" && task.ProjectID != "" {
		effectiveProjectDir = filepath.Join(e.config.ProjectWorkspacePath, task.ProjectID)
	}

	// Snapshot the project-persisted artifact tree BEFORE the
	// agent runs so persistArtifacts can tell "this file's bytes
	// changed during the step" from "this file was inherited from
	// the previous task's worktree merge and just got its mtime
	// touched by git checkout". The mtime gate alone misses the
	// latter — see SnapshotArtifactDir doc for the incident
	// reproduction.
	preStepArtifactSnapshot := e.SnapshotArtifactDir(filepath.Join(effectiveProjectDir, "artifacts", "out"))

	tempRoot, err := os.MkdirTemp("", "vornik-exec-*")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create execution workspace: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempRoot) }()

	inputDir := filepath.Join(tempRoot, "input")
	outputDir := filepath.Join(tempRoot, "output")
	workspaceDir := filepath.Join(tempRoot, "workspace")
	for _, dir := range []string{inputDir, outputDir, workspaceDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", nil, fmt.Errorf("failed to create execution directory %s: %w", dir, err)
		}
	}

	// Stage input artifacts from the previous step into the workspace.
	if opts != nil && len(opts.InputArtifacts) > 0 {
		allowedRoots := allowedStagingRoots(e.config.ProjectWorkspacePath, task.ProjectID, e.config.ArtifactStoragePath)
		if stageErr := e.stageInputArtifacts(workspaceDir, opts.InputArtifacts, allowedRoots); stageErr != nil {
			e.logger.Warn().Err(stageErr).
				Str("execution_id", execution.ID).
				Msg("artifact staging: could not prepare workspace staging dirs")
		}
	}
	// Snapshot the upstream OUTPUT files after staging and before the agent
	// runs. persistArtifacts uses the workspace:-prefixed fingerprints to
	// distinguish unchanged handoff inputs from files this step created or
	// revised. Without this, every downstream step re-registered all upstream
	// files, producing lists such as 31 rows for 9 physical deliverables.
	for name, hash := range e.SnapshotArtifactDir(filepath.Join(workspaceDir, "artifacts", "out")) {
		preStepArtifactSnapshot["workspace:"+name] = hash
	}

	swarmID := ""
	if plan.swarm != nil {
		swarmID = plan.swarm.ID
	}
	if opts == nil {
		opts = &agentInputOpts{}
	}
	if opts.ProjectTimezone == "" && plan.project != nil {
		opts.ProjectTimezone = plan.project.Budget.Timezone
	}
	// Mirror the step's output-file contract into the payload. Set HERE, on the
	// one path that also enforces it below, so the two cannot drift: an agent
	// told about a glob is always told about the same glob the daemon will
	// check.
	if opts.RequireOutputGlob == "" {
		opts.RequireOutputGlob = step.RequireOutputGlob
	}
	// Same reasoning for the role's plausibility rules: set HERE, on the one
	// path that also EVALUATES them below, so an agent told about a rule is
	// always told about the rule the daemon will actually apply.
	//
	// Resolved locally rather than read from the outer roleConfig: that
	// variable is not assigned until well AFTER buildAgentInput has already
	// run, so reading it here yields nil and the rules never reach the
	// payload. The first version of this did exactly that, guarded by a
	// `roleConfig != nil` check that turned the mistake into a silent no-op —
	// the agent kept being failed for rules it was never shown, and the
	// in-container nudge never fired once across a whole benchmark arm.
	if len(opts.PlausibilityRules) == 0 && plan != nil && plan.swarm != nil {
		if rc, rcErr := findSwarmRole(plan.swarm, step.Role); rcErr == nil && rc != nil {
			opts.PlausibilityRules = rc.PlausibilityRules
		}
	}
	// Layer 1 of the context-discovery hardening: pre-load the
	// project's canonical context files (PROJECT_CONTEXT.md +
	// USER_GUIDANCE.md) so the agent doesn't burn tool calls
	// walking the workspace to find them. effectiveProjectDir is
	// the project workspace root the executor mounts into the
	// container at /app/workspace, so its .autonomy/ tree is the
	// same one the agent would scan post-load.
	//
	// Skipped for the recovery step path — its agentInputOpts
	// re-creation in plan_step.go's adaptive workflow already
	// uses the prior opts; we only fill in if not already set
	// (empty CanonicalContext is the zero value, and a non-empty
	// one only lands here via a prior caller).
	if opts.CanonicalContext.Empty() && effectiveProjectDir != "" {
		opts.CanonicalContext = resolveCanonicalContext(effectiveProjectDir)
		if e.metrics != nil && !opts.CanonicalContext.Empty() {
			e.metrics.RecordCanonicalContextLoaded(task.ProjectID, opts.CanonicalContext.Source)
			for _, f := range opts.CanonicalContext.Truncated {
				e.metrics.RecordCanonicalContextTruncated(task.ProjectID, f)
			}
		}
	}
	// Stash the resolved source on the executor so the step-
	// outcome recorder can stamp execution_step_outcomes.
	// context_source without re-walking the workspace. Cleared
	// when the execution terminates.
	if opts.CanonicalContext.Source != "" {
		e.contextSourceByExecution.Store(execution.ID, opts.CanonicalContext.Source)
	}
	// Pre-load approved knowledge skills for this role, same guard as
	// CanonicalContext so an adaptive re-spawn reusing prior opts
	// doesn't double-load.
	if len(opts.Skills) == 0 {
		opts.Skills = e.resolveSkillIndex(ctx, task.ProjectID, step.Role)
	}
	// Tool-budget guidance is injected only when the grant tool exists for this
	// deployment AND this role may actually call it. The second half was missing:
	// the block told every role to call a tool most of them were forbidden, and
	// they obediently tried (§12.6a of the agent-quality design).
	opts.ToolGrantAvailable = e.toolGrantsWired && RoleMayGrantTools(plan.swarm, step.Role)
	// Workspace-git guidance is injected only when the project really is a
	// worktree — exactly the predicate startContainer uses to decide whether to
	// bind-mount the main .git read-only, so the block can never describe a
	// deployment it does not apply to.
	opts.WorktreeGitReadOnly = plan != nil && projectRootFromWorktree(plan.worktreeDir) != ""
	// Advisory guidance blocks this swarm's operator switched off (LLD 09
	// §13.3.1). Read fresh from the plan's swarm on every step rather than
	// cached on the executor, so a config reload takes effect on the next step
	// like every other swarm field. Invariant blocks are unaffected — neither
	// the config loader nor the composition seam will suppress one.
	if plan.swarm != nil {
		opts.SuppressedGuidanceBlocks = plan.swarm.SuppressedGuidanceBlocks
	}
	// Tell the agent its REAL budget. Hardcoding 30m here made the runtime
	// contract's timeoutSeconds a number that was usually wrong.
	if opts != nil {
		opts.StepTimeout = timeout
	}
	input := buildAgentInput(task, execution.ID, plan.workflow.ID, swarmID, stepID, step.Role, step.Prompt, opts)
	// 0o600 — task.json holds the step prompt + any inline
	// secrets / credentials passed from project config.
	if err := os.WriteFile(filepath.Join(inputDir, "task.json"), input, 0o600); err != nil {
		return "", nil, fmt.Errorf("failed to write task input: %w", err)
	}

	// Write mcp.json only for local-subprocess fallback. In normal
	// daemon-proxy mode, mcp-bridge uses VORNIK_API_URL and the agent
	// container never needs a credentials-bearing MCP config file.
	if plan.project != nil && len(plan.project.MCP.Servers) > 0 && shouldWriteMCPConfig(e.config.AgentLLMEnv) {
		if mcpData, encErr := buildMCPConfig(plan.project.MCP.Servers); encErr == nil {
			// 0o600 — mcp.json carries MCP server credentials.
			_ = os.WriteFile(filepath.Join(inputDir, "mcp.json"), mcpData, 0o600)
		} else {
			e.logger.Warn().Err(encErr).Str("task_id", task.ID).Msg("failed to encode mcp.json — MCP tools unavailable for this step")
		}
	}

	roleConfig, err = findSwarmRole(plan.swarm, step.Role)
	if err != nil {
		return "", nil, err
	}
	effectiveModel = e.effectiveRoleModelForTask(task, roleConfig)

	// Agent-LLM health gate (LLD 2026-07-12-agent-llm-health-breaker): a
	// per-model circuit breaker fed by agent-container call outcomes. When
	// the model's circuit is OPEN, fast-reject WITHOUT starting a container
	// — the returned *chat.ModelUnhealthyError flows through
	// isModelUnhealthyFailure (skip the infra ladder) and
	// isModelShapedFailure (trigger the role's modelFallback), so a sick
	// primary flips to the fallback in seconds, not tens of minutes (the
	// 2026-07-12 ~12-container-starts incident). The Record defer (LIFO,
	// runs before the outcome defer) folds this step's outcome into the
	// breaker on return: success/failure via chat.IsUpstreamInfraError,
	// abstain on a chat-breaker MODEL_UNHEALTHY reject (topology-2
	// isolation). Nil registry = passthrough.
	var agentHealthProbe bool
	if e.agentHealth != nil {
		permitted, probe, reject := e.agentHealth.Gate(effectiveModel)
		if !permitted {
			return "", nil, reject
		}
		agentHealthProbe = probe
		defer func() { e.agentHealth.Record(effectiveModel, agentHealthProbe, err) }()
	}

	// Warm path: reuse a warm container if the role policy allows it.
	// Falls back to ephemeral container if the warm pool is exhausted.
	isReplay := counterfactual.ExtractPayload(task.Payload).IsReplay
	if roleConfig.RuntimePolicy == "warm" && e.warmPool != nil && !isReplay {
		cid, result, werr := e.executeWarmAgentStep(ctx, task, execution, plan, stepID, roleConfig, input, workspaceDir, timeout, stepStart, preStepArtifactSnapshot)
		if werr == nil {
			agentStamp.AgentImageID = e.observeAgentImageID(ctx, cid)
			// Mirror the ephemeral path's post-exit persistence: tool
			// audit log, LLM usage (cost+tokens), and the degenerate-
			// loop detector. Without these, warm-pool runs were absent
			// from task_llm_usage — which inflated ModelEffectiveCostUSD
			// because the success counter got incremented by the defer
			// while their cost was never summed into entry.costUSD.
			if len(result) > 0 {
				var toolCount int
				var stepTaint taintlineage.StepTaint
				toolCount, degenerateLoopDetail, stepTaint = e.persistToolAuditFromResult(ctx, task, execution, stepID, result)
				if toolCount > 0 {
					agentStamp.ToolCallsUsed = &toolCount
				}
				taintStampVal = e.taintStampFromStep(stepTaint)
				e.recordLLMUsageFromResult(ctx, task, execution, stepID, step.Role, effectiveModel, result)
			}
			// Output-shape contract applies to warm runs too; missing
			// required keys downgrade the apparent success into an
			// INVALID_OUTPUT failure the gate evaluator can trust.
			if len(roleConfig.RequiredOutputKeys) > 0 && len(result) > 0 {
				if missing := validateRequiredOutputKeys(result, roleConfig.RequiredOutputKeys); len(missing) > 0 {
					return "", nil, fmt.Errorf("schema violation: role %q result.json is missing required keys: %v",
						step.Role, missing)
				}
			}
			return cid, result, nil
		}
		// Warm pool unavailable — fall through to ephemeral container.
	}

	// Crash-recovery re-attach (sandbox idempotency): if this step has an
	// in-flight container recorded from a prior run that died on an UNCLEAN
	// daemon crash (a clean shutdown drains the container in pauseWithReason,
	// so this only fires after a kill/OOM/panic), ADOPT that container instead
	// of re-spawning — re-running the step would duplicate its side effects.
	// Any doubt (no record / container gone / inspect error) falls back to a
	// fresh run below, so normal execution is byte-identical.
	var containerID string
	if reID, reOut, reAttached := e.reattachInFlightContainer(ctx, execution, stepID); reAttached {
		containerID = reID
		outputDir = reOut // adopt the original run's output dir to read its result.json
	} else {
		// Mint the per-task API key here — in the same stack frame as the
		// defer — so that a startContainer failure still triggers revoke.
		// Previously the mint lived inside startContainer, meaning a failure
		// after minting would leak the key until its 48-hour expiry.
		// extraEnv carries the minted VORNIK_API_KEY override into startContainer
		// where it is merged last (after all other env building), ensuring it
		// wins over the static AgentLLMEnv value.
		extraEnv := make(map[string]string)
		// Dynamic per-role tool budget: scale the role's static
		// VORNIK_MAX_TOOL_ITERATIONS by the planner's complexity tier when
		// tool_budget is enabled. Ephemeral path only — warm roles return
		// above before extraEnv exists, so they run on their static budget
		// (LLD §8). Autonomy/delegation/checkpoint tasks are held to the
		// tighter autonomy ceiling; only operator-initiated tasks get the
		// full factor.
		budgetTier := ""
		if opts != nil {
			budgetTier = opts.ComplexityTier
		}
		// Slice-4 active budget consumer: on the absent-verdict path, when the
		// gate is on and the budget resolver is wired, look up the learned tier
		// for this (project, role) pair. An explicit planner verdict always wins
		// — we only fill the gap when budgetTier is empty (LLD §7).
		// Nil resolver means "no learned tier" (Community path, or EE not yet
		// wired) — falls back to the default budget, no panic.
		if budgetTier == "" && e.instinctToolBudget && e.instinctBudgetResolver != nil {
			// minConf is an intentional v1 constant (== instinct active_confidence
			// default), NOT a config knob — LLD §7 defers per-knob config to "if a
			// need appears" (the deadDays-style YAGNI call). Promote to
			// instinct.consumers.tool_budget_min_confidence only when tuning is
			// actually needed. (Companion review 2026-06-21, finding 7.)
			const minConf = 0.6
			if ltr, ok := e.instinctBudgetResolver.LearnedTier(ctx, task.ProjectID, step.Role, minConf); ok {
				budgetTier = ltr.Tier
				e.logger.Debug().
					Str("execution_id", execution.ID).
					Str("role", step.Role).
					Str("learned_tier", budgetTier).
					Str("instinct_id", ltr.InstinctID).
					Msg("tool_budget: instinct active consumer supplied learned tier (absent-verdict path)")
				// Record the application row — best-effort; errors are logged
				// but never block the step. Result is "ignored" at apply time;
				// the feedback loop can grade it later when enabled (LLD §7).
				e.recordLearnedBudgetApplication(ctx, task, execution, stepID, ltr.InstinctID)
			}
		}
		// Stamp the tier on the outcome row for every ephemeral agent step,
		// regardless of whether the tool-budget feature is enabled (LLD §4).
		// When a learned tier was applied, stamp it so the extractor mines it.
		agentStamp.ComplexityTier = budgetTier
		// Item 5 (2026-07-18): resolve the ORIGIN creation source through the
		// checkpoint chain so an operator task keeps operator budget headroom on
		// a checkpoint-retry (not the tighter autonomy cap). Coupled with the
		// step-timeout twin in workflow.go via the same helper.
		autonomousTask := e.budgetAutonomous(ctx, task)
		if eff, inject := resolveRoleToolBudget(roleConfig, toolbudget.Tier(budgetTier),
			autonomousTask, e.config.ToolBudget); inject {
			extraEnv["VORNIK_MAX_TOOL_ITERATIONS"] = strconv.Itoa(eff)
			e.metrics.RecordToolBudgetResolved(step.Role, budgetTier)
			e.logger.Debug().
				Str("execution_id", execution.ID).
				Str("role", step.Role).
				Str("tier", budgetTier).
				Int("base", roleToolBudgetBase(roleConfig)).
				Int("effective", eff).
				Bool("autonomous", autonomousTask).
				Msg("tool_budget: scaled VORNIK_MAX_TOOL_ITERATIONS")
			// Stamp the resolved effective budget on the outcome row so the
			// budget-instinct extractor can mine over/under-provisioning as
			// a pure function over outcome rows (migration 106, LLD §4).
			agentStamp.EffectiveToolBudget = &eff
		}
		// Couple the per-call LLM timeout to THIS step's effective
		// wall-clock so a single LLM call can never outlive the step that
		// contains it (the 2026-06-18 IBKR "podman wait timed out"
		// incident: the agent's 300s/call default exceeded review_risk's
		// 240s step, so one slow upstream call outlived the step and the
		// container was killed mid-call). This per-step override wins over
		// the static global agent_llm.timeout in AgentLLMEnv because
		// extraEnv is merged last. Safe by construction for every workflow,
		// regardless of config.
		var llmCeiling time.Duration
		if e.config.AgentLLMEnv != nil {
			if s := e.config.AgentLLMEnv["VORNIK_LLM_TIMEOUT"]; s != "" {
				if n, convErr := strconv.Atoi(s); convErr == nil && n > 0 {
					llmCeiling = time.Duration(n) * time.Second
				}
			}
		}
		if llmTO := perCallTimeoutForStep(timeout, llmCeiling); llmTO > 0 {
			extraEnv["VORNIK_LLM_TIMEOUT"] = strconv.Itoa(int(llmTO.Seconds()))
			if timeout > 0 && llmCeiling >= timeout {
				// Tier-1 loud guard: the operator's global ceiling would
				// itself have outlived this step. The coupling clamped it,
				// but surface the misconfiguration so it gets fixed.
				e.logger.Warn().
					Str("execution_id", execution.ID).
					Str("role", step.Role).
					Dur("agent_llm_timeout", llmCeiling).
					Dur("step_timeout", timeout).
					Dur("clamped_to", llmTO).
					Msg("agent_llm.timeout >= step timeout: a single LLM call could outlive the step; clamped per-step. Lower agent_llm.timeout below the smallest step timeout.")
			}
		}
		// Same invariant for run_shell: the agent enforces VORNIK_SHELL_TIMEOUT
		// via `timeout "$SHELL_TIMEOUT"` (entrypoint.sh), defaulting to 300s
		// and never injected by the daemon — so one shell command could
		// outlive any step <300s, the latent sibling of the LLM bug above.
		// Couple it per-step below the step budget too.
		if shellTO := perCallTimeoutForStep(timeout, defaultAgentShellTimeout); shellTO > 0 {
			extraEnv["VORNIK_SHELL_TIMEOUT"] = strconv.Itoa(int(shellTO.Seconds()))
		}
		if minted := e.injectPerTaskKey(ctx, task.ProjectID, task.ID, extraEnv); minted {
			// Register revoke immediately after a successful mint so the
			// defer fires on both success and failure paths of startContainer
			// and everything that follows.
			defer e.revokeTaskKey(task.ID)
		} else if isReplay {
			// Replay side-effect enforcement binds provenance to the
			// per-task key. Falling back to a shared static key would let a
			// replay omit/spoof X-Task-ID and reach real MCP side effects.
			return "", nil, fmt.Errorf("counterfactual replay requires a per-task API key")
		}
		// GitHub outbound token — ephemeral path (the dev-swarm default, incl.
		// github-classifier). Mirrors the warm path; shared helper so they can't
		// drift (incident 2026-06-13: only the warm path was wired, so ephemeral
		// agents hit "gh: not logged into any GitHub hosts").
		e.injectGitHubToken(ctx, extraEnv, plan.project)
		containerID, err = e.startContainer(ctx, task, execution.ID, roleConfig.Runtime.Image, step.Role, inputDir, outputDir, workspaceDir, roleConfig, plan.worktreeDir, timeout, extraEnv)
		if err != nil {
			return "", nil, markRetryable(fmt.Errorf("failed to start container: %w", err))
		}
		// Record the in-flight container so a daemon crash before this step's
		// result is processed can re-attach to it on recovery instead of
		// re-running the step (which would duplicate side effects). Cleared
		// implicitly when the workflow loop's next saveCheckpoint overwrites
		// the snapshot from its in-memory state (which carries no in-flight).
		e.markStepInFlight(ctx, execution, stepID, containerID, tempRoot)
	}
	agentStamp.AgentImageID = e.observeAgentImageID(ctx, containerID)

	exitCode, err := e.waitForCompletion(ctx, containerID, timeout)
	if err != nil {
		// Cleanup must run on a context that survives parent
		// cancellation. Same ctx that just returned ctx.Err() will
		// make exec.CommandContext fail immediately on
		// `podman stop` / `podman rm`, so the container keeps running
		// while the deferred os.RemoveAll(tempRoot) at function exit
		// rm-rf's the bind-mounted workspace under it. The agent then
		// observes /app/workspace/* vanishing mid-iteration and
		// either crashes ("INVALID_JSON" from a 0-byte request) or
		// writes an emergency result.json into a non-existent dir.
		// Use force-remove on a fresh background context so the rm
		// is synchronous and the mount is released before the defer
		// fires.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = e.runtime.RemoveContainer(cleanupCtx, containerID, true)
		cleanupCancel()
		return "", nil, markRetryable(fmt.Errorf("failed waiting for container: %w", err))
	}
	// Always persist artifacts, even on failure — the step's
	// {step}-response.md contains diagnostic output. stepStart +
	// preStepArtifactSnapshot together gate the project-persisted
	// tree walk so stale artifacts from prior tasks aren't
	// re-registered as this step's. The snapshot is the
	// load-bearing half (mtime is unreliable across worktree
	// checkouts); stepStart is kept as a cheap pre-filter.
	stepOutputs, _ := e.persistArtifacts(ctx, execution.ID, task.ProjectID, task.ID, workspaceDir, effectiveProjectDir, stepStart, preStepArtifactSnapshot)
	// Hand the harvested store-backed outputs to the workflow loop so
	// the next step's ephemeral container can re-stage them (task e9a5).
	plan.stepOutputArtifacts = stepOutputs

	// Read result.json once — used for both audit and status checks.
	resultPath := filepath.Join(outputDir, "result.json")
	resultBytes, readErr := os.ReadFile(resultPath)
	e.logger.Debug().
		Str("execution_id", execution.ID).
		Str("step", stepID).
		Int("exit_code", exitCode).
		Int("result_bytes", len(resultBytes)).
		Err(readErr).
		Msg("audit: read result.json")

	// Secret-leak scan on the agent's primary output channel.
	// Default action (Redact) substitutes findings with a typed
	// marker before the bytes go anywhere downstream — persisted
	// to the executions table, copied to artifacts, surfaced in
	// the dashboard, forwarded to Telegram. The original file on
	// disk is left intact (the worktree is ephemeral) so an
	// operator debugging a failed run can still see what the
	// agent actually produced. Detect-mode (operator override)
	// logs without modifying. Block-mode (Phase 2) returns a
	// sentinel error so the step fails with SECRET_LEAK class
	// while the redacted body still flows through audit.
	// VALIDATE BEFORE REDACT. rawResultBytes is what the agent actually wrote;
	// resultBytes is the redacted form. The split exists because redaction is a
	// raw byte-span replacement (secrets.Redact) with no JSON awareness: a span
	// covering a quoted token's quotes leaves a bare token where a string belongs
	// and the payload stops parsing. Every structural read then fails at once —
	// validateRequiredOutputKeys reports EVERY required key as missing because
	// normalizedResultPayload errored, and the usage/tool-audit parsers give up
	// too, landing tool_calls_used NULL.
	//
	// Measured 2026-08-20: all 10 of 10 schema violations in bench arm 7 had a
	// result_json redaction recorded (historically 120 of 180). The control makes
	// it causal rather than correlated — 6 of 9 PASSING rungs were also redacted,
	// so redaction is fatal only when the splice lands where it breaks the JSON.
	// The trigger is ordinary agent output: case ids, commit SHAs, generated
	// identifiers. The failure was reported as the model's, on a metric named
	// model_schema_violation_rate.
	//
	// WHICH BYTES GO WHERE, and why this costs no confidentiality:
	//
	//   rawResultBytes — reads whose output is structure or numbers, never
	//     persisted content: validateRequiredOutputKeys (returns key NAMES from
	//     the role config), recordLLMUsageFromResult (integers only), and
	//     persistToolAuditFromResult (each entry is independently redacted by
	//     the auditredact repository decorator before it is stored).
	//
	//   resultBytes — anything whose parsed value is persisted or forwarded.
	//     Above all the agent's own `message`, which becomes agentError, reaches
	//     the executions table, the dashboard, Telegram, and downstream prompts.
	//     That must never be built from the raw bytes.
	//
	// Redaction is unchanged: same detector, same action resolution, same audit
	// rows. Only the reads that cannot leak are moved ahead of it.
	rawResultBytes := resultBytes
	var secretLeakErr error
	resultBytes, secretLeakErr = e.scanResultForSecrets(ctx, task, execution, stepID, resultBytes)

	// Persist tool audit entries regardless of exit code. The degenerate-
	// loop detail is captured into a closure variable that the defer
	// reads — see the defer at the top of this function.
	// rawResultBytes here, not resultBytes: both of these parse structure that
	// redaction can destroy, and neither persists unredacted content. Tool-audit
	// entries are redacted individually at the repository seam before
	// storage, and the usage reader takes integers only. Parsing the redacted
	// form is what nulled tool_calls_used on every corrupted result.
	if len(rawResultBytes) > 0 {
		var toolCount int
		var stepTaint taintlineage.StepTaint
		toolCount, degenerateLoopDetail, stepTaint = e.persistToolAuditFromResult(ctx, task, execution, stepID, rawResultBytes)
		if toolCount > 0 {
			agentStamp.ToolCallsUsed = &toolCount
		}
		taintStampVal = e.taintStampFromStep(stepTaint)
		e.recordLLMUsageFromResult(ctx, task, execution, stepID, step.Role, effectiveModel, rawResultBytes)
	} else {
		e.logger.Warn().Str("execution_id", execution.ID).Str("step", stepID).
			Msg("audit: result.json is empty or missing — no audit entries")
	}

	// Trust result.json status field over exit code.
	var agentError string
	// Block-mode secret-leak takes precedence over the agent's own
	// COMPLETED/FAILED self-report: even a successful task whose
	// output contained credentials must surface as a SECRET_LEAK
	// failure so operators see it. The classifier matches the
	// "secret_leak:" prefix to TaskFailureClassSecretLeak.
	if secretLeakErr != nil {
		agentError = secretLeakErr.Error()
	}
	if len(resultBytes) > 0 {
		var resultStatus struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		}
		agentOutcome, agentOutcomeDetail := agentQualityOutcome(resultBytes)
		if json.Unmarshal(resultBytes, &resultStatus) == nil {
			if agentError == "" && resultStatus.Status == "FAILED" {
				agentError = "agent reported FAILED status: " + resultStatus.Message
			}
			// Agent-emitted outcome override. Clean exit
			// (status=COMPLETED), but the agent chose to stop early for
			// a budget guard and the per-step quality row needs to
			// reflect that instead of being terminal-swept to OK.
			switch agentOutcome {
			case string(stepoutcome.BudgetTripwire):
				budgetTripwireDetail = agentOutcomeDetail
				if budgetTripwireDetail == "" {
					budgetTripwireDetail = "agent self-aborted on budget tripwire (no detail provided)"
				}
			case string(stepoutcome.PromptTokenBudget):
				promptTokenBudgetDetail = agentOutcomeDetail
				if promptTokenBudgetDetail == "" {
					promptTokenBudgetDetail = "agent self-finalized on prompt-token budget (no detail provided)"
				}
			case string(stepoutcome.IterationExhausted):
				// The agent hit its tool-call cap and answered on a forced
				// tool-free turn rather than dying with no output at all.
				// Clean exit so the workflow does not take a failure
				// transition, but the quality row must still record the
				// exhaustion — 14 of 47 failed steps in the 2026-08-16 ctx32k
				// arm were iteration caps, every one sitting exactly at its
				// budget, and before this they produced nothing judgeable.
				iterationCapDetail = agentOutcomeDetail
				if iterationCapDetail == "" {
					iterationCapDetail = "agent self-finalized on the tool iteration cap (no detail provided)"
				}
			}
		}
	}

	// Fall back to exit code if status wasn't explicitly FAILED but code is non-zero.
	if agentError == "" && exitCode != 0 {
		agentError = fmt.Sprintf("container exited with code %d", exitCode)
		if len(resultBytes) > 0 {
			agentError = string(resultBytes)
		}
	}

	// Output-shape contract: if the role declares requiredOutputKeys,
	// enforce them before treating the step as successful. Malformed
	// output ("status": undefined, missing "approved") has historically
	// slipped past gates and produced "completed with empty fields"
	// task records. Fail loud here instead, with a message the
	// failure classifier maps to INVALID_OUTPUT so S2 dashboards and
	// retry policy see the correct class.
	// rawResultBytes: this is a STRUCTURAL check on what the agent wrote, and its
	// output is the list of key names from the role's own config — no payload
	// content is persisted by it. Running it on the redacted form made it report
	// every required key missing whenever redaction broke the JSON, which is the
	// misattribution documented at the scan above and pinned by
	// TestValidateRequiredOutputKeys_rawPassesWhereRedactedFails.
	if agentError == "" && len(roleConfig.RequiredOutputKeys) > 0 && len(rawResultBytes) > 0 {
		if missing := validateRequiredOutputKeys(rawResultBytes, roleConfig.RequiredOutputKeys); len(missing) > 0 {
			agentError = fmt.Sprintf("schema violation: role %q result.json is missing required keys: %v",
				step.Role, missing)
		}
	}

	// Output-file contract: a step declaring require_output_glob must
	// have written at least one matching file DURING this step, or it
	// fails loud with the "schema violation:" prefix (one corrective
	// shape retry, then on_fail). Incident
	// task_20260712143854_429a3500d692d23c: 7 of 8 deep-research
	// subtasks completed without writing their promised findings file
	// and the parent chain "succeeded" into an empty publish.
	// globVerified records that the filesystem — not the model — confirmed
	// this step's declared output. It is handed to the plausibility pass
	// below so a self-reported file list cannot fail a step whose output
	// was already verified for real.
	globVerified := false
	if agentError == "" && step.RequireOutputGlob != "" {
		if outputGlobSatisfied(workspaceDir, effectiveProjectDir, step.RequireOutputGlob, stepStart) {
			globVerified = true
		} else {
			agentError = fmt.Sprintf(
				"schema violation: output contract for step %q not met — no file matching %q was written during this step. You MUST write the declared output file before finishing.",
				stepID, step.RequireOutputGlob)
		}
	}

	// Tool contract: an auth-class tool failure fails the step (no opt-in),
	// and a step declaring require_tools must have completed each of them.
	//
	// Runs AFTER the output-file contract and BEFORE plausibility, because a
	// connector rejection explains a missing file and a thin result far better
	// than either explains itself. Incident 2026-08-25: a connector lost auth,
	// the agent narrated the 401 correctly in its own payload, and the task
	// completed — nothing between the tool call and the task's status could
	// name what had happened. See tool_contract.go.
	if agentError == "" {
		if v := e.evaluateToolContract(ctx, execution, task, &step, stepID); v != nil {
			agentError = v.Message
			if v.AuthClass {
				e.logger.Error().
					Str("execution_id", execution.ID).
					Str("task_id", task.ID).
					Str("project_id", task.ProjectID).
					Str("step", stepID).
					Str("outcome_class", "auth").
					Msg("step failed: a connector rejected the credential — " + v.Message)
			}
		}
	}

	// Plausibility rules: layered on top of RequiredOutputKeys to
	// catch the half-honest output ("approved":true with empty
	// "feedback") that passes shape validation but isn't actually
	// usable downstream. WarnOnly rules emit a log line and don't
	// gate; gate-mode rules fail the step with INVALID_OUTPUT.
	// rawResultBytes for the same reason as the required-keys check: this is a
	// structural evaluation, and a violation's Detail is built from the field name
	// plus the rule's own `When` config (formatViolationDetail) — never from a
	// payload value. So nothing here persists agent content, and evaluating the
	// redacted form would report spurious violations on exactly the results
	// redaction had corrupted, misattributing a third symptom of one cause.
	if agentError == "" && len(roleConfig.PlausibilityRules) > 0 && len(rawResultBytes) > 0 {
		violations := EvaluatePlausibilityWithGroundTruth(rawResultBytes, roleConfig.PlausibilityRules, globVerified)
		var blocking []string
		for _, v := range violations {
			if v.WarnOnly {
				e.logger.Warn().
					Str("execution_id", execution.ID).
					Str("step", stepID).
					Str("role", step.Role).
					Str("rule", v.RuleName).
					Str("detail", v.Detail).
					Msg("plausibility: warn-only rule fired — step still passes")
				continue
			}
			blocking = append(blocking, fmt.Sprintf("%s: %s", v.RuleName, v.Detail))
		}
		if len(blocking) > 0 {
			agentError = fmt.Sprintf("plausibility violation: role %q failed %d rule(s): %s",
				step.Role, len(blocking), strings.Join(blocking, "; "))
		}
	}

	if agentError != "" {
		if logs, logErr := e.runtime.Logs(ctx, containerID, containerLogTailLines); logErr == nil && logs != "" {
			// Container logs frequently include shell output that
			// echoes env vars or curl auth headers — scan + redact
			// at read time so the failed-task UI doesn't display
			// the secret. The actual container log isn't modified;
			// only the bytes we surface to the operator.
			logs = string(e.scanContainerLogsForSecrets(ctx, execution, stepID, []byte(logs)))
			agentError += containerLogSection(logs)
		}
		if rmErr := e.runtime.RemoveContainer(context.Background(), containerID, true); rmErr != nil {
			e.logger.Warn().Err(rmErr).Str("container_id", containerID).Msg("failed to remove container after agent failure")
		}
		return "", nil, newContainerExitError(exitCode, agentError)
	}

	// Verify file claims (modified_files, outputArtifacts paths,
	// produced_files) in result.json against the real filesystem.
	// effectiveProjectDir was computed at the top of this function
	// (worktree path when worktrees are in use, project's persistent
	// root otherwise) — same path the container saw mounted at
	// /app/workspace/project. A claim that doesn't match reality
	// fails the step here so the next role doesn't silently run
	// against a half-empty workspace.
	if verifyErr := e.verifyClaimedFiles(resultBytes, workspaceDir, effectiveProjectDir, stepStart); verifyErr != nil {
		if rmErr := e.runtime.RemoveContainer(context.Background(), containerID, true); rmErr != nil {
			e.logger.Warn().Err(rmErr).Str("container_id", containerID).Msg("failed to remove container after verify failure")
		}
		return "", nil, verifyErr
	}

	// Cross-cutting deception checks (testing.passed:true → toolAudit
	// must show actual execution; review.checked_commit → object must
	// exist; files_changed:N → real diff count must match). The regular
	// agent-step path runs dev-pipeline's coder/tester/reviewer through
	// here, so without this call the per-role verification only fires
	// for plan-spawned roles. Sampling postStepHEAD after the agent
	// run lets the files_changed accuracy check work the same way the
	// plan-step path's already does.
	postStepHEAD := ""
	if plan != nil && plan.worktreeDir != "" {
		postStepHEAD = gitHEAD(ctx, plan.worktreeDir)
	}
	if claimsErr := e.verifyRoleClaims(ctx, resultBytes, preStepHEAD, postStepHEAD, effectiveProjectDir); claimsErr != nil {
		if rmErr := e.runtime.RemoveContainer(context.Background(), containerID, true); rmErr != nil {
			e.logger.Warn().Err(rmErr).Str("container_id", containerID).Msg("failed to remove container after role-claim verify failure")
		}
		return "", nil, claimsErr
	}

	// Phase 1 hallucination detection: scan the agent's prose for
	// claims (URLs, task/project IDs, artifact filenames, numeric
	// counts) and cross-reference each against this step's
	// tool_audit + artifact list. Signals land on the outcome row
	// regardless of severity; High-severity findings fail the
	// step so the scheduler's retry path picks it up.
	//
	// The detector is best-effort: any error during build/scan
	// degrades silently (the step succeeds without signals), so a
	// transient audit-DB hiccup never blocks otherwise-good work.
	if e.hallucinationDetector != nil {
		signalBlob, hallucDetail, hallucErr := e.runHallucinationDetector(ctx, task, execution, stepID, resultBytes)
		hallucinationSignalsBlob = signalBlob
		if hallucErr != nil {
			hallucinationDetail = hallucDetail
			if rmErr := e.runtime.RemoveContainer(context.Background(), containerID, true); rmErr != nil {
				e.logger.Warn().Err(rmErr).Str("container_id", containerID).Msg("failed to remove container after hallucination failure")
			}
			return "", nil, hallucErr
		}
	}

	// Trading scorecard_floor SOFT-DROP (design 2026-07-25): before the
	// verifiers run, drop floor-failing OPEN proposals from the strategist's
	// result so a no-qualifying-candidate tick NO_ACTIONs instead of the whole
	// step hard-failing. Integrity violations (protected-symbol close, unknown
	// action, unparseable proposal) still HARD-fail the step here. No-op for
	// non-trading projects / non-proposal steps (self-gated). The rewritten
	// resultBytes is what flows to the risk-officer/executor; the (now
	// SeverityWarn) scorecard_floor verifier below observes the filtered set.
	filtered, floorErr := e.filterTradingFloor(task, resultBytes)
	if floorErr != nil {
		hallucinationDetail = floorErr.Error()
		if rmErr := e.runtime.RemoveContainer(context.Background(), containerID, true); rmErr != nil {
			e.logger.Warn().Err(rmErr).Str("container_id", containerID).Msg("failed to remove container after trading-floor hard-fail")
		}
		return "", nil, floorErr
	}
	resultBytes = filtered

	// Phase 2 outcome verifiers: project-declared declarative
	// invariants over (artifacts, audit, result.json). Distinct
	// from Phase 1 which scans prose; Phase 2 scrutinises actual
	// work. A verifier failure fails the step so the scheduler
	// retries it.
	if verifyErr := e.runVerifiers(ctx, task, execution, stepID, resultBytes, effectiveProjectDir); verifyErr != nil {
		hallucinationDetail = verifyErr.Error()
		if rmErr := e.runtime.RemoveContainer(context.Background(), containerID, true); rmErr != nil {
			e.logger.Warn().Err(rmErr).Str("container_id", containerID).Msg("failed to remove container after verifier failure")
		}
		return "", nil, verifyErr
	}

	// Clean up the container immediately — we've read result.json and
	// persisted artifacts. Leaving it running leaks resources across
	// multi-step workflows (prior steps' containers were never removed).
	_ = e.runtime.RemoveContainer(context.Background(), containerID, false)

	return containerID, resultBytes, nil
}

// observeAgentImageID reads the immutable image identity from the container
// that actually runs the step. Configured tags are mutable intent and are
// deliberately rejected as provenance.
func (e *Executor) observeAgentImageID(ctx context.Context, containerID string) string {
	if e == nil || e.runtime == nil || containerID == "" {
		return ""
	}
	container, err := e.runtime.InspectContainer(ctx, containerID)
	if err != nil || container == nil {
		return ""
	}
	imageID := strings.TrimSpace(container.Image)

	// PODMAN RETURNS A BARE 64-HEX ID, not a "sha256:"-prefixed one — verified
	// on podman 5.8.4, 2026-08-29. Requiring the prefix therefore discarded a
	// good id on EVERY container: a full 30-task arm produced 162 outcome rows
	// with no image id, every arm key was PARTIAL, and `bench agent rollup`
	// refused to merge a completed run. The provenance axis had never worked.
	//
	// Both forms are accepted and normalised to the prefixed one, because the
	// stored value is compared across runs and two spellings of the same id
	// would read as two different images.
	imageID = strings.TrimPrefix(imageID, "sha256:")
	if !isHexImageID(imageID) {
		// A tag ("ghcr.io/...:bench") is mutable intent, not provenance, and a
		// short hex string is not an id. Neither is evidence.
		return ""
	}
	return "sha256:" + imageID
}

// isHexImageID reports whether s is a full-length hex container image id.
//
// Length is checked, not just the alphabet: "abc123" is hex and is not an image
// id, and padding or accepting it would invent provenance from a fragment.
func isHexImageID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// extractProseFromResult pulls model-authored prose out of an
// agent's result.json, ignoring structured fields that carry
// nested tool-call serialisations (which cause false-positive
// hallucination signals when scanned naively). Concatenates
// every recognised string-valued prose field plus the entries
// of any output_text / final_text array. Returns an empty
// string when the JSON doesn't parse or contains no
// prose-bearing field — caller treats that as "nothing to
// scan", not "everything to scan", so a malformed result.json
// degrades to no-op rather than to an unbounded noise burst.
func extractProseFromResult(resultBytes []byte) string {
	if len(resultBytes) == 0 {
		return ""
	}
	var p map[string]any
	if err := json.Unmarshal(resultBytes, &p); err != nil {
		return ""
	}
	// Every key here is a known model-authored prose field. The
	// list errs on the side of inclusion: a field this code
	// hasn't seen yet won't be scanned, which means it can't
	// produce false positives but also means a future role with
	// a novel field name is invisible to the detector. That
	// trade-off favours precision over recall in the v1 detector.
	proseKeys := []string{
		"message", "summary", "output", "final_answer",
		"final_text", "answer", "response", "explanation",
		"notes", "reasoning",
	}
	var b strings.Builder
	for _, k := range proseKeys {
		if v, ok := p[k]; ok {
			appendString(&b, v)
		}
	}
	return b.String()
}

// appendString writes v into b when v is a string, or
// recursively writes string entries when v is an array of
// strings. Other types are skipped — we don't unmarshal
// arbitrary nested values because that's exactly the trap that
// produced v1's tool-history false positives.
func appendString(b *strings.Builder, v any) {
	switch s := v.(type) {
	case string:
		if s != "" {
			b.WriteString(s)
			b.WriteString("\n")
		}
	case []any:
		for _, e := range s {
			if str, ok := e.(string); ok && str != "" {
				b.WriteString(str)
				b.WriteString("\n")
			}
		}
	}
}

// extractTaskType pulls the task type out of the payload JSON.
// Tasks store their operator-meaningful type ("research",
// "feature", "bug-fix") inside the payload's "taskType" field
// rather than as a top-level column on the row, so this scans
// for it. Returns "" when the payload is missing/unparseable —
// verifiers with WhenTaskType filters skip the row in that case.
func extractTaskType(task *persistence.Task) string {
	if task == nil || len(task.Payload) == 0 {
		return ""
	}
	var p map[string]any
	if err := json.Unmarshal(task.Payload, &p); err != nil {
		return ""
	}
	if t, ok := p["taskType"].(string); ok {
		return t
	}
	return ""
}

// runVerifiers loads the project's Phase 2 verifier list and
// runs each applicable one against the step's actual output.
// Returns nil on pass (no verifiers configured, or all passed),
// and a multi-violation error on fail. The error message lists
// every violation so the operator's retry message has the full
// picture, not just the first failure.
//
// projectDir is the absolute host path to the task's project workspace
// root (effectiveProjectDir from the call site — worktree path or
// ProjectWorkspacePath/<projectID>). Passed through to verifier.Input so
// file-backed verifiers like cv_claims_grounded can read workspace files
// (e.g. .autonomy/RESUME.md) without inline duplication. Empty string is
// safe: verifiers that need it will abstain when it is absent.
// filterTradingFloor applies the scorecard_floor SOFT-DROP to an agent step's
// result (design 2026-07-25-scorecard-floor-soft-drop): floor-failing OPEN
// proposals are dropped so a no-qualifying-candidate tick NO_ACTIONs instead of
// hard-failing the step; integrity violations (protected-symbol close, unknown
// action, unparseable proposal) return an error that fails the step. Self-gated:
// a no-op for non-trading projects (no watchlist) or when the floor flags are
// off, and — via FilterTradingProposals — for step outputs with no proposals[]
// (risk-officer/executor steps), so it can run on every agent step safely.
func (e *Executor) filterTradingFloor(task *persistence.Task, resultBytes []byte) ([]byte, error) {
	if e.workflows == nil {
		return resultBytes, nil
	}
	proj, ok := e.workflows.(interface {
		GetProject(string) *registry.Project
	})
	if !ok {
		return resultBytes, nil
	}
	p := proj.GetProject(task.ProjectID)
	if p == nil || len(p.Trading.Watchlist) == 0 {
		return resultBytes, nil
	}
	fc := &verifier.TradingFloorConfig{
		ScorecardEnabled:   p.Trading.Scorecard.Enabled,
		RegimeEnabled:      p.Trading.Regime.Enabled,
		MinEntryTotal:      p.Trading.Scorecard.MinEntryTotal,
		BlockLongInRiskOff: p.Trading.Regime.BlockLongInRiskOff,
		StaleBehavior:      p.Trading.Regime.StaleBehavior,
		MinComponentCount:  p.Trading.Regime.MinComponentCount,
		ProtectedSymbols:   p.Trading.ProtectedSymbols,
	}
	return verifier.FilterTradingProposals(resultBytes, fc)
}

func (e *Executor) runVerifiers(ctx context.Context, task *persistence.Task, execution *persistence.Execution, stepID string, resultBytes []byte, projectDir string) error {
	if e.workflows == nil {
		return nil
	}
	proj, ok := e.workflows.(interface {
		GetProject(string) *registry.Project
	})
	if !ok {
		return nil
	}
	p := proj.GetProject(task.ProjectID)
	if p == nil || len(p.Verifiers) == 0 {
		return nil
	}

	cfgs, skipped := verifier.ConfigsFromMaps(p.Verifiers)
	if skipped > 0 {
		e.logger.Warn().
			Str("project", task.ProjectID).
			Int("skipped", skipped).
			Msg("verifier: dropped malformed config entries")
	}
	if len(cfgs) == 0 {
		return nil
	}

	in := e.buildVerifierInput(ctx, task, execution, stepID, resultBytes, projectDir, p)

	violations := verifier.RunAll(ctx, cfgs, in)
	if len(violations) == 0 {
		return nil
	}

	// Partition by severity. Warn-tier violations surface in the
	// log but don't abort the step — they're advisory signals that
	// let the operator see a problem (e.g. a single scraper block
	// in a 30-fetch research run) without burning a retry on what
	// the rest of the workflow can absorb. Fail-tier is the
	// historical zero-tolerance behaviour.
	var (
		failMsgs    []string
		warnMsgs    []string
		hasTerminal bool
		blockedURLs []verifier.BlockedURL
	)
	for _, v := range violations {
		if v.Severity == verifier.SeverityWarn {
			warnMsgs = append(warnMsgs, v.Error())
			continue
		}
		failMsgs = append(failMsgs, v.Error())
		if v.Terminal {
			hasTerminal = true
		}
		// Aggregate permanent blocks across all failing verifiers so
		// the recovery step sees the full picture in one slice.
		// Transient blocks are intentionally dropped — they're
		// retried via the normal Terminal=true path.
		for _, b := range v.BlockedURLs {
			if b.Permanent {
				blockedURLs = append(blockedURLs, b)
			}
		}
	}
	if len(warnMsgs) > 0 {
		e.logger.Warn().
			Str("execution_id", execution.ID).
			Str("step", stepID).
			Int("warnings", len(warnMsgs)).
			Strs("details", warnMsgs).
			Msg("verifier: advisory violation(s) — step continues")
		// Persist warn-tier violations as a separate outcome row so
		// the soak panel + post-mortem can surface "step passed but
		// had N warnings" without operators having to grep journald.
		// Companion row, not a replacement for the producer's own
		// outcome — the unique index on (execution_id, step_id) only
		// guards pending_validation rows, so verifier_warn can coexist.
		e.recordWarnViolationsOutcome(ctx, task, execution, stepID, warnMsgs)
	}
	if len(failMsgs) == 0 {
		return nil
	}
	// Backstop: if any violation flagged a missing-pattern artifact,
	// write a stub artifact carrying the audit summary so the next
	// adaptive-routing iteration (or the post-mortem) sees what
	// actually happened. The stub doesn't change THIS iteration's
	// verdict — the step still fails — but it closes the "agent
	// silently dropped the file" gap that costs operators triage
	// time. Idempotent: a stub already on disk is skipped.
	e.writeBackstopArtifacts(ctx, task, execution, stepID, cfgs, in, violations)

	e.logger.Warn().
		Str("execution_id", execution.ID).
		Str("step", stepID).
		Bool("terminal", hasTerminal).
		Int("violations", len(failMsgs)).
		Int("permanent_blocks", len(blockedURLs)).
		Strs("details", failMsgs).
		Msg("verifier: blocking step on outcome-verifier violation(s)")
	return joinVerifierErrorsWithBlocks(failMsgs, hasTerminal, blockedURLs)
}

// buildVerifierInput assembles the verifier.Input snapshot for a single step.
// Extracted from runVerifiers to keep that function's cognitive complexity
// within the project's gocognit limit. Best-effort: DB errors leave the
// relevant slices nil (verifiers fall through rather than block on infra
// flakiness).
func (e *Executor) buildVerifierInput(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	stepID string,
	resultBytes []byte,
	projectDir string,
	p *registry.Project,
) verifier.Input {
	in := verifier.Input{
		ResultJSON:     resultBytes,
		TaskType:       extractTaskType(task),
		StepID:         stepID,
		CreationSource: string(task.CreationSource),
		ProjectDir:     projectDir,
	}
	// Trading projects expose their watchlist to the verifier
	// engine so the proposals_match_watchlist verifier can reject
	// strategist hallucinations (model invents a ticker not on the
	// watchlist). Nil/empty for non-trading projects, which makes
	// the verifier a clean no-op there.
	if len(p.Trading.Watchlist) > 0 {
		in.WatchlistAllowList = p.Trading.Watchlist
		// Deterministic SMA(50) for any long-open proposals, so the
		// entry_gate_consistent verifier can re-check the trend floor
		// the strategist's prose claimed but may have fabricated
		// (2026-06-12 NVDA whipsaw). No-op (nil) when there are no
		// open proposals, which is the common case per tick.
		in.EntryGateIndicators = e.entryGateIndicators(ctx, p, resultBytes)
		// Project the scorecard/regime entry-floor config so the
		// scorecard_floor verifier can enforce the code-side floor on
		// OPEN proposals and refuse EXIT-class actions on protected
		// symbols. Inert unless both gate flags are enabled (soak gate).
		in.TradingFloor = &verifier.TradingFloorConfig{
			ScorecardEnabled:   p.Trading.Scorecard.Enabled,
			RegimeEnabled:      p.Trading.Regime.Enabled,
			MinEntryTotal:      p.Trading.Scorecard.MinEntryTotal,
			BlockLongInRiskOff: p.Trading.Regime.BlockLongInRiskOff,
			StaleBehavior:      p.Trading.Regime.StaleBehavior,
			MinComponentCount:  p.Trading.Regime.MinComponentCount,
			ProtectedSymbols:   p.Trading.ProtectedSymbols,
		}
	}
	if e.auditRepo != nil && execution.ID != "" {
		execID := execution.ID
		// Step-scoping the audit (2026-05-26): without filter.StepID,
		// the recover step inherits the failed research step's audit
		// rows and re-fails the same verifier with the same numbers
		// (the "lead planning failed: 4/18 fetch(es) blocked" pattern
		// observed on T-87bf). Each step's verifier must only see
		// THAT step's tool calls.
		filter := persistence.ToolAuditFilter{
			ExecutionID: &execID,
			PageSize:    500,
		}
		if stepID != "" {
			sid := stepID
			filter.StepID = &sid
		}
		entries, err := e.auditRepo.List(ctx, filter)
		if err != nil {
			e.logger.Warn().Err(err).Str("execution_id", execID).Str("step", stepID).Msg("verifier: tool_audit fetch failed; running with empty audit")
		} else {
			in.AuditEntries = entries
		}
	}
	if e.artifactRepo != nil && task.ID != "" {
		tID := task.ID
		arts, err := e.artifactRepo.List(ctx, persistence.ArtifactFilter{
			TaskID:   &tID,
			PageSize: 100,
		})
		if err != nil {
			e.logger.Warn().Err(err).Str("task_id", tID).Msg("verifier: artifact fetch failed; running with empty list")
		} else {
			in.Artifacts = arts
		}
	}
	return in
}

// joinVerifierErrors builds the step-failure error for runVerifiers.
// Pulled out as a pure function so the terminal-wrap decision is
// trivially testable without a real executor + verifier stack.
//
// When any violation is terminal, the joined error is wrapped in a
// TerminalVerifierError so workflow.go can detect it via errors.As
// and skip the step.OnFail retry without parsing the message. The
// human-readable surface (used by log lines and the post-mortem
// trail) is identical either way; only the type changes.
func joinVerifierErrors(failMsgs []string, hasTerminal bool) error {
	return joinVerifierErrorsWithBlocks(failMsgs, hasTerminal, nil)
}

// joinVerifierErrorsWithBlocks is the BlockedURL-aware variant. When
// the violations carry permanent-block URLs and Terminal=false, the
// returned error wraps in *RecoverableVerifierError so the executor's
// on_fail path can extract the URLs and forward them to the recovery
// step (typically a lead checkpoint proposing alternative sources).
// Empty blockedURLs falls through to the plain joinVerifierErrors
// behaviour.
func joinVerifierErrorsWithBlocks(failMsgs []string, hasTerminal bool, blockedURLs []verifier.BlockedURL) error {
	if len(failMsgs) == 0 {
		return nil
	}
	joined := fmt.Errorf("phase-2 verifier(s) failed: %s", strings.Join(failMsgs, "; "))
	if hasTerminal {
		return &TerminalVerifierError{Err: joined}
	}
	if len(blockedURLs) > 0 {
		return &RecoverableVerifierError{Err: joined, BlockedURLs: blockedURLs}
	}
	return joined
}

// RecoverableVerifierError marks a verifier failure that the
// executor's on_fail path can route to a recovery step instead of
// just failing the task. The BlockedURLs slice carries the permanent
// blocks (auth_required, captcha, paywall) so the recovery step's
// lead role can propose alternatives via a `decision` checkpoint
// without a separate audit-replay pass.
//
// The wrapping is parallel to TerminalVerifierError; the two are
// mutually exclusive at the joinVerifierErrors call site (Terminal
// short-circuits before this wrapping). Workflow.go uses errors.As
// to distinguish.
type RecoverableVerifierError struct {
	Err         error
	BlockedURLs []verifier.BlockedURL
}

func (e *RecoverableVerifierError) Error() string {
	if e == nil || e.Err == nil {
		return "recoverable verifier failure"
	}
	return e.Err.Error()
}

func (e *RecoverableVerifierError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// TerminalVerifierError marks a verifier failure that the adaptive
// routing loop must not retry. workflow.go checks `errors.As` on this
// type and exits the loop with the wrapped error instead of routing
// to step.OnFail. See verifier.Violation.Terminal for the per-impl
// signal that bubbles up.
type TerminalVerifierError struct {
	Err error
}

func (e *TerminalVerifierError) Error() string {
	if e == nil || e.Err == nil {
		return "terminal verifier failure"
	}
	return e.Err.Error()
}

func (e *TerminalVerifierError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// recordWarnViolationsOutcome writes one execution_step_outcomes row
// of outcome=verifier_warn carrying the joined warn-tier verifier
// messages. Best-effort: a nil outcomeRepo or DB error is logged but
// never aborts the verifier flow — warn violations are advisory and
// must not block the step. Pulled out into its own helper so the
// runVerifiers path stays a thin orchestrator and the persistence
// shape is unit-testable without standing up the full executor.
func (e *Executor) recordWarnViolationsOutcome(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	stepID string,
	warnMsgs []string,
) {
	if e == nil || e.outcomeRepo == nil || len(warnMsgs) == 0 {
		return
	}
	if task == nil || execution == nil {
		return
	}
	detail := strings.Join(warnMsgs, "; ")
	row := &persistence.ExecutionStepOutcome{
		ID:          persistence.GenerateID("outc"),
		ProjectID:   task.ProjectID,
		TaskID:      task.ID,
		ExecutionID: execution.ID,
		StepID:      stepID,
		Outcome:     string(stepoutcome.VerifierWarn),
		ErrorClass:  "verifier_warn",
		ErrorDetail: truncateDetailPreservingEnds(detail, 2000),
		RecordedAt:  time.Now().UTC(),
	}
	finalized := time.Now().UTC()
	row.FinalizedAt = &finalized
	if err := e.outcomeRepo.Record(ctx, row); err != nil {
		e.logger.Warn().Err(err).
			Str("execution_id", execution.ID).
			Str("step", stepID).
			Int("warn_count", len(warnMsgs)).
			Msg("verifier_warn outcome: persist failed (advisory; step continues)")
	}
}

// isTerminalVerifierError reports whether err is (or wraps) a
// TerminalVerifierError. Tiny helper so workflow.go's "skip on_fail"
// branch reads as one boolean check and so the predicate is unit-
// testable without standing up the workflow loop.
func isTerminalVerifierError(err error) bool {
	if err == nil {
		return false
	}
	var t *TerminalVerifierError
	return errors.As(err, &t)
}

// runHallucinationDetector builds a step-scoped grounding context,
// runs the detector against the agent's result.json prose, and
// returns the marshalled signals + an error iff the detector
// chose to fail the step. The error string is what surfaces in
// the agent_step retry loop, so it deliberately mentions the
// claim type that tripped — operators reading retry warnings
// shouldn't have to open the UI to know what went wrong.
//
// Three return values:
//   - signalBlob: JSON-marshalled []Signal, persisted on the row
//     regardless of pass/fail so the UI can show ALL findings
//     (including warn-only ones that didn't block).
//   - hallucDetail: human-readable detail string for the outcome
//     row's error_detail, only set when the detector blocks.
//   - blockErr: non-nil iff signals contained High severity. The
//     caller wraps this and returns it so the existing retry
//     plumbing kicks in.
func (e *Executor) runHallucinationDetector(ctx context.Context, task *persistence.Task, execution *persistence.Execution, stepID string, resultBytes []byte) ([]byte, string, error) {
	if e.hallucinationDetector == nil || len(resultBytes) == 0 {
		return nil, "", nil
	}
	// Pull the agent's prose fields out of result.json. Scanning
	// raw JSON bytes was the v1 approach but produced massive
	// false-positive bursts: small models routinely embed a
	// stringified dump of their tool-call history inside their
	// own response prose, which means the result.json contains
	// double-escaped tool inputs/outputs. The URLs in those
	// nested dumps come out JSON-escaped (`https://x.com\\`)
	// while the audit holds them un-escaped, so the membership
	// check fails on every URL the agent ever fetched. Scoping
	// to declared prose fields (message / summary / output /
	// final_answer) leaves the actual model-authored narrative
	// while ignoring the embedded tool-history noise. If no
	// recognised field exists, we degrade to no-op rather than
	// risk the v1 noise.
	text := extractProseFromResult(resultBytes)
	if text == "" {
		return nil, "", nil
	}

	gc, err := hallucination.BuildForStep(ctx, e.auditRepo, e.artifactRepo, execution.ID, task.ID)
	if err != nil {
		// Best-effort: log and run the detector on whatever
		// partial context loaded. False-positive risk on a partial
		// context is bounded — every rule no-ops when its
		// grounding source is empty.
		e.logger.Warn().
			Err(err).
			Str("execution_id", execution.ID).
			Str("step", stepID).
			Msg("hallucination: grounding-context build failed; running detector on partial context")
	}
	signals := e.hallucinationDetector.Scan(text, gc)
	e.hallucinationMetrics.ObserveSignals(task.ProjectID, signals)
	if len(signals) == 0 {
		return nil, "", nil
	}

	signalBlob, mErr := json.Marshal(signals)
	if mErr != nil {
		// Marshalling can't realistically fail here (Signal is
		// pure JSON-safe primitives) — log and proceed with no
		// blob so the outcome row still records the failure.
		e.logger.Warn().Err(mErr).Msg("hallucination: failed to marshal signals; persisting without blob")
		signalBlob = nil
	}

	if !e.hallucinationDetector.ShouldBlock(signals) {
		// Warn-level only: persist signals, don't block the step.
		e.logger.Info().
			Str("execution_id", execution.ID).
			Str("step", stepID).
			Int("signals", len(signals)).
			Str("highest_severity", string(hallucination.HighestSeverity(signals))).
			Msg("hallucination: warn-level signals recorded")
		return signalBlob, "", nil
	}

	// High severity: the step is failing. Build a one-line
	// summary for the error_detail column.
	var topClaims []string
	for _, s := range signals {
		if !s.Severity.Block() {
			continue
		}
		topClaims = append(topClaims, fmt.Sprintf("%s=%q", s.ClaimType, s.ClaimValue))
		if len(topClaims) >= 5 {
			break
		}
	}
	detail := fmt.Sprintf("hallucination detector found %d unsupported claim(s): %s", len(signals), strings.Join(topClaims, ", "))
	e.logger.Warn().
		Str("execution_id", execution.ID).
		Str("step", stepID).
		Int("signals", len(signals)).
		Strs("top_claims", topClaims).
		Msg("hallucination: blocking step on high-severity signal")
	return signalBlob, detail, errors.New(detail)
}

// executeWarmAgentStep runs a task on a warm container from the pool.
func (e *Executor) executeWarmAgentStep(ctx context.Context, task *persistence.Task, execution *persistence.Execution, plan *executionPlan, stepID string, roleConfig *registry.SwarmRole, inputData []byte, workspaceDir string, timeout time.Duration, stepStart time.Time, preStepArtifactSnapshot ArtifactDirSnapshot) (string, []byte, error) {
	key := runtime.PoolKey{
		ProjectID: task.ProjectID,
		Role:      roleConfig.Name,
		Image:     roleConfig.Runtime.Image,
		// Per-role network policy keys the pool so a daemon-only warm
		// container is never reused for a host role (Step B).
		Network: runtime.NetworkMode(roleConfig.Runtime.Network),
	}

	// Build per-role env overrides so warm containers get the correct model and limits.
	roleEnv := make(map[string]string, len(roleConfig.Runtime.EnvVars)+5)
	for k, v := range roleConfig.Runtime.EnvVars {
		roleEnv[k] = v
	}
	// Project-scoped named secrets (per-secret allowlist) — warm path mirror.
	for k, v := range e.namedSecretEnv(task.ProjectID) {
		roleEnv[k] = v
	}

	// Warm containers are keyed per (project, role, image), so the project
	// ID is stable for the container's lifetime. Inject it here so the agent
	// can call project-scoped endpoints like /api/v1/projects/{id}/mcp/*;
	// without it, mcp-bridge fails with "VORNIK_API_URL is set but
	// VORNIK_PROJECT_ID is empty" on every warm task.
	if task.ProjectID != "" {
		roleEnv["VORNIK_PROJECT_ID"] = task.ProjectID
	}
	if task.ID != "" {
		roleEnv["VORNIK_TASK_ID"] = task.ID
	}
	if execution != nil && execution.ID != "" {
		roleEnv["VORNIK_EXECUTION_ID"] = execution.ID
	}
	// Creation-source + user-context-path stamps. These let role
	// systemPrompts in the swarm config branch deterministically:
	// USER tasks read .autonomy/USER_GUIDANCE.md (or whatever the
	// project's autonomy.userContextFilePath points at); autonomy /
	// delegation / checkpoint tasks read the existing
	// PROJECT_CONTEXT.md path. Without this split a USER request
	// like "Ingest this CV" gets squeezed through the autonomy
	// procedure and the agent fabricates compliance with a
	// procedure that doesn't apply.
	if task.CreationSource != "" {
		roleEnv["VORNIK_TASK_CREATION_SOURCE"] = string(task.CreationSource)
	}
	if task.CreationSource == persistence.TaskCreationSourceUser && plan.project != nil {
		if userCtx := plan.project.ResolveUserContextFilePath(); userCtx != "" {
			roleEnv["VORNIK_USER_CONTEXT_PATH"] = userCtx
		}
	}
	// Least-privilege GitHub outbound (warm path): mint a short-lived (~1h)
	// installation token and inject GH_TOKEN/GITHUB_TOKEN when the project
	// wires `github` credentials. Shared with the ephemeral path via the same
	// helper so the two can't drift. Inert for projects without `github` set.
	e.injectGitHubToken(ctx, roleEnv, plan.project)
	// Effective model: operator override > counterfactual > role.model > role
	// envVars > global (effectiveRoleModelForTask — same as the ephemeral path
	// and the metrics path, so launch + recorded model can't drift). Subsumes
	// the role.model default AND the counterfactual replay override
	// (`vornikctl blackbox replay --variable model`), and adds the operator
	// override (Fallback-model button / model:fallback hint / recovery
	// model_fallback action) that was previously dropped on this path (2026-06-20).
	if m := e.effectiveRoleModelForTask(task, roleConfig); m != "" {
		roleEnv["VORNIK_LLM_MODEL"] = m
	}
	// Apply model-specific limits for the effective model (role override, envVars, or global).
	// For warm containers the base env already has global defaults baked in; roleEnv carries
	// only the delta, so fall back to the global model when nothing overrides it here.
	effectiveModel := roleEnv["VORNIK_LLM_MODEL"]
	if effectiveModel == "" {
		effectiveModel = e.config.AgentLLMEnv["VORNIK_LLM_MODEL"]
	}
	if effectiveModel != "" {
		if limit, ok := e.config.ModelLimits[effectiveModel]; ok {
			if limit.MaxTokens > 0 {
				roleEnv["VORNIK_LLM_MAX_TOKENS"] = strconv.Itoa(limit.MaxTokens)
			}
			if limit.ContextSize > 0 {
				roleEnv["VORNIK_LLM_CONTEXT_SIZE"] = strconv.Itoa(limit.ContextSize)
			}
		}
	}
	// Role-level explicit overrides win over everything.
	if roleConfig.MaxTokens > 0 {
		roleEnv["VORNIK_LLM_MAX_TOKENS"] = strconv.Itoa(roleConfig.MaxTokens)
	}
	if roleConfig.ContextSize > 0 {
		roleEnv["VORNIK_LLM_CONTEXT_SIZE"] = strconv.Itoa(roleConfig.ContextSize)
	}
	// Counterfactual budget tightens max_tokens when present. The
	// "override only LOWERS" rule applies here too — if the
	// override is larger than the role's existing cap, ignore it.
	if cfBudget := counterfactual.ExtractPayload(task.Payload).Budget; cfBudget.MaxTokens > 0 {
		current, _ := strconv.Atoi(roleEnv["VORNIK_LLM_MAX_TOKENS"])
		if current == 0 || cfBudget.MaxTokens < current {
			roleEnv["VORNIK_LLM_MAX_TOKENS"] = strconv.Itoa(cfBudget.MaxTokens)
		}
	}
	injectCostEnv(roleEnv, e.pricing, effectiveModel)
	if _, err := injectBudgetEnv(ctx, roleEnv, e.llmUsageRepo, plan.project, time.Now().UTC()); err != nil {
		// Budget snapshot failed (e.g. transient DB error). The agent
		// will see no VORNIK_BUDGET_*_REMAINING_USD env vars and treat
		// the step as unbounded — same shape as a project with no
		// caps configured. Log so an operator can correlate; don't
		// block the step on it.
		e.logger.Warn().
			Err(err).
			Str("project_id", task.ProjectID).
			Str("step_id", stepID).
			Msg("budget snapshot failed — agent will run without remaining-budget hints")
	}

	entry := e.warmPool.Acquire(key)
	if entry == nil {
		// Cold start of a warm container — this is the ONLY point the
		// container's env is baked, so the scoped credential must be
		// injected here (re-keying on Acquire reuse would require
		// restarting the container, defeating the pool). Finding B1(b):
		// warm-pool containers previously baked the UNSCOPED static
		// agent key for both daemon callbacks and the LLM proxy. Pools
		// are keyed per (project, role, image), so a PROJECT-scoped key
		// is stable for the container's lifetime and closes the
		// cross-project escalation (a prompt-injected warm agent can no
		// longer replay an all-access credential against other
		// projects, incl. broker place_order). Residual reported to the
		// orchestrator: warm-path callers are project-scoped, not
		// task-scoped, so B3's per-task audit binding does not constrain
		// them. On mint failure we keep the static key (availability)
		// and warn once per (project, role).
		if !e.injectWarmProjectKey(ctx, task.ProjectID, roleConfig.Name, roleEnv) {
			if e.apiKeyMinter != nil && shouldWarnWarmStaticKey(task.ProjectID, roleConfig.Name) {
				e.logger.Warn().
					Str("project", task.ProjectID).
					Str("role", roleConfig.Name).
					Str("task_id", task.ID).
					Msg("warm-pool role runs on the static agent key; project-scoped key mint unavailable")
			}
		}
		var err error
		entry, err = e.warmPool.StartWarm(ctx, key, roleEnv)
		if err != nil {
			// Fallback: start a new ephemeral container
			return "", nil, markRetryable(fmt.Errorf("warm pool unavailable: %w", err))
		}
	}

	// Mirror staged input artifacts from the per-step temp workspace into
	// the warm container's persistent workspace. The staging block in
	// executeAgentStep writes into workspaceDir/artifacts/in/, which is
	// the temp dir that ONLY ephemeral containers mount — warm containers
	// have a separate entry.WorkspaceDir bound at /app/workspace, so
	// without this copy the agent sees an empty artifacts/in even though
	// task.json points at /app/workspace/artifacts/in/<file>. Also clear
	// any stale entries from the previous task on this warm container.
	stagedInDir := filepath.Join(workspaceDir, "artifacts", "in")
	warmInDir := filepath.Join(entry.WorkspaceDir, "artifacts", "in")
	_ = os.RemoveAll(warmInDir)
	if entries, err := os.ReadDir(stagedInDir); err == nil && len(entries) > 0 {
		if err := os.MkdirAll(warmInDir, 0o755); err != nil {
			e.warmPool.Release(entry, false)
			return "", nil, markRetryable(fmt.Errorf("failed to create warm artifacts/in: %w", err))
		}
		for _, ent := range entries {
			if ent.IsDir() {
				continue
			}
			data, rerr := os.ReadFile(filepath.Join(stagedInDir, ent.Name()))
			if rerr != nil {
				continue
			}
			safeName, nerr := safepath.CleanFileName(ent.Name())
			if nerr != nil {
				continue
			}
			dst, jerr := safepath.JoinUnder(warmInDir, safeName)
			if jerr != nil {
				continue
			}
			// 0o600 — staged input artifacts can be operator-private.
			if werr := os.WriteFile(dst, data, 0o600); werr != nil {
				e.logger.Warn().Err(werr).Str("dst", dst).Msg("warm: failed to mirror staged input artifact")
			}
		}
	}

	if err := e.warmPool.InjectTask(entry, inputData); err != nil {
		e.warmPool.Release(entry, false)
		return "", nil, markRetryable(fmt.Errorf("failed to inject task into warm container: %w", err))
	}

	resultBytes, err := e.warmPool.WaitForTaskDone(ctx, entry, timeout)
	if err != nil {
		e.warmPool.Release(entry, false)
		return "", nil, markRetryable(fmt.Errorf("warm container task failed: %w", err))
	}

	// Persist artifacts from the warm container's workspace.
	// Release as UNHEALTHY on persistArtifacts failure so the
	// container is torn down rather than recycled — the workspace
	// still contains the previous task's artifacts/result.json,
	// and a healthy-release would leak that into the next task's
	// Acquire (dirty workspace, wrong inputs).
	warmEffectiveProjectDir := ""
	if plan != nil {
		warmEffectiveProjectDir = plan.worktreeDir
	}
	if warmEffectiveProjectDir == "" && e.config.ProjectWorkspacePath != "" && task.ProjectID != "" {
		warmEffectiveProjectDir = filepath.Join(e.config.ProjectWorkspacePath, task.ProjectID)
	}
	// Pre-step snapshot was taken in the outer scope before the
	// warm container ran — same scope as stepStart. The snapshot
	// is the load-bearing half (mtime alone is unreliable under
	// per-task git worktrees) so the warm path must thread it
	// through too — without this every warm-pool task would re-
	// register every leftover under the project-persisted
	// artifacts tree as ITS deliverable.
	stepOutputs, err := e.persistArtifacts(ctx, execution.ID, task.ProjectID, task.ID, entry.WorkspaceDir, warmEffectiveProjectDir, stepStart, preStepArtifactSnapshot)
	if err != nil {
		e.warmPool.Release(entry, false)
		return "", nil, err
	}
	// Hand the harvested store-backed outputs to the workflow loop so
	// the next step's ephemeral container can re-stage them (task e9a5).
	if plan != nil {
		plan.stepOutputArtifacts = stepOutputs
	}

	e.warmPool.Release(entry, true)
	return entry.ContainerID, resultBytes, nil
}

// startContainer creates and starts a container for the task with tracing.
// projectDirOverride, when non-empty, is mounted as the project directory instead
// of the default path derived from ProjectWorkspacePath — used for worktree isolation.
// extraEnv, when non-nil, is merged into envVars after all other env building so
// caller-supplied values (e.g. the minted per-task VORNIK_API_KEY) win over defaults.
func (e *Executor) startContainer(ctx context.Context, task *persistence.Task, executionID, image, role, inputDir, outputDir, workspaceDir string, roleConfig *registry.SwarmRole, projectDirOverride string, timeout time.Duration, extraEnv map[string]string) (string, error) {
	roleEnvVars := roleConfig.Runtime.EnvVars
	// Build env vars: start with LLM config, then merge role-specific overrides.
	envVars := make(map[string]string, len(e.config.AgentLLMEnv)+len(roleEnvVars)+4)
	for k, v := range e.config.AgentLLMEnv {
		envVars[k] = v
	}
	for k, v := range roleEnvVars {
		envVars[k] = v
	}
	// Project-scoped named secrets (per-secret allowlist): inject only the
	// operator-declared credentials this project is allowed to bind. Placed
	// after role envVars so an authorized secret wins over a same-named role
	// literal; the minted per-task key (extraEnv) still wins over everything.
	for k, v := range e.namedSecretEnv(task.ProjectID) {
		envVars[k] = v
	}
	// Effective model: operator override > counterfactual > role.model > role
	// envVars > global (effectiveRoleModelForTask — the SAME resolution used for
	// metrics/usage, so the launched model and the recorded model can't drift).
	// Fixes operator_model_override (Fallback-model button / model:fallback hint /
	// recovery model_fallback action) never reaching VORNIK_LLM_MODEL (2026-06-20).
	if m := e.effectiveRoleModelForTask(task, roleConfig); m != "" {
		envVars["VORNIK_LLM_MODEL"] = m
	}
	// Apply model-specific limits for the effective model — regardless of whether the
	// model came from role.model, runtime.envVars, or the global agent_llm config.
	if effectiveModel := envVars["VORNIK_LLM_MODEL"]; effectiveModel != "" {
		if limit, ok := e.config.ModelLimits[effectiveModel]; ok {
			if limit.MaxTokens > 0 {
				envVars["VORNIK_LLM_MAX_TOKENS"] = strconv.Itoa(limit.MaxTokens)
			}
			if limit.ContextSize > 0 {
				envVars["VORNIK_LLM_CONTEXT_SIZE"] = strconv.Itoa(limit.ContextSize)
			}
		}
	}
	// Role-level explicit token overrides win over model limits.
	if roleConfig.MaxTokens > 0 {
		envVars["VORNIK_LLM_MAX_TOKENS"] = strconv.Itoa(roleConfig.MaxTokens)
	}
	if roleConfig.ContextSize > 0 {
		envVars["VORNIK_LLM_CONTEXT_SIZE"] = strconv.Itoa(roleConfig.ContextSize)
	}
	// Counterfactual budget — tighten max_tokens for replays.
	// Mirrors the warm-pool path above; same LOWERS-only rule.
	if cfBudget := counterfactual.ExtractPayload(task.Payload).Budget; cfBudget.MaxTokens > 0 {
		current, _ := strconv.Atoi(envVars["VORNIK_LLM_MAX_TOKENS"])
		if current == 0 || cfBudget.MaxTokens < current {
			envVars["VORNIK_LLM_MAX_TOKENS"] = strconv.Itoa(cfBudget.MaxTokens)
		}
	}
	if e.config.LogLevel != "" {
		envVars["VORNIK_LOG_LEVEL"] = e.config.LogLevel
	}
	// Per-task project scope for mcp-bridge's daemon-proxy mode. Without
	// this, the bridge can't tell which project's MCP tools to ask the
	// daemon for — the project is the security boundary here, not the
	// task. Combined with VORNIK_API_URL (set container-wide in service
	// init), this enables agent containers to use MCP tools without
	// spawning their own subprocesses.
	if task.ProjectID != "" {
		envVars["VORNIK_PROJECT_ID"] = task.ProjectID
	}
	if task.ID != "" {
		envVars["VORNIK_TASK_ID"] = task.ID
	}
	if executionID != "" {
		envVars["VORNIK_EXECUTION_ID"] = executionID
	}
	// Cold-start env-stamp for creation_source + user-context-path.
	// Mirrors the warm-path roleEnv block above so role
	// systemPrompts can branch on VORNIK_TASK_CREATION_SOURCE +
	// VORNIK_USER_CONTEXT_PATH regardless of whether the agent
	// container was spun up cold or recycled from the warm pool.
	if task.CreationSource != "" {
		envVars["VORNIK_TASK_CREATION_SOURCE"] = string(task.CreationSource)
	}
	if task.CreationSource == persistence.TaskCreationSourceUser && e.workflows != nil {
		if proj := e.workflows.GetProject(task.ProjectID); proj != nil {
			if userCtx := proj.ResolveUserContextFilePath(); userCtx != "" {
				envVars["VORNIK_USER_CONTEXT_PATH"] = userCtx
			}
		}
	}
	injectCostEnv(envVars, e.pricing, envVars["VORNIK_LLM_MODEL"])
	// Snapshot the project's remaining budget for the agent's in-loop
	// tripwire. Cold-start path resolves the project through the
	// workflow resolver since startContainer doesn't carry the plan.
	// A nil resolver (some test paths) silently skips — same as a
	// project with no caps.
	if e.workflows != nil {
		if proj := e.workflows.GetProject(task.ProjectID); proj != nil {
			if _, err := injectBudgetEnv(ctx, envVars, e.llmUsageRepo, proj, time.Now().UTC()); err != nil {
				e.logger.Warn().
					Err(err).
					Str("project_id", task.ProjectID).
					Str("execution_id", executionID).
					Msg("budget snapshot failed — agent will run without remaining-budget hints")
			}
		}
	}

	// Apply caller-supplied env overrides last so they win over every
	// default built above. The primary use is the minted per-task
	// VORNIK_API_KEY passed in from executeAgentStep where the paired
	// defer revokeTaskKey was already registered.
	for k, v := range extraEnv {
		envVars[k] = v
	}

	// Resolve the project directory mounted into the container.
	// A worktree override takes precedence over the default per-project path.
	var projectDir string
	if projectDirOverride != "" {
		projectDir = projectDirOverride
	} else if e.config.ProjectWorkspacePath != "" {
		projectDir = filepath.Join(e.config.ProjectWorkspacePath, task.ProjectID)
		_ = os.MkdirAll(projectDir, 0o755)
	}

	modelSource := "global"
	if roleConfig.Model != "" {
		modelSource = "role"
	} else if roleConfig.Runtime.EnvVars["VORNIK_LLM_MODEL"] != "" {
		modelSource = "envVars"
	}
	e.logger.Info().
		Str("task_id", task.ID).
		Str("image", image).
		Str("role", role).
		Str("llm_model", envVars["VORNIK_LLM_MODEL"]).
		Str("model_source", modelSource).
		Msg("starting container")

	// When ProjectDir points inside an .worktrees/ subdirectory, the
	// worktree's .git file holds an absolute path back to the main
	// project's .git. Expose that path to the container so git commands
	// inside the worktree resolve correctly. See ContainerConfig.ProjectGitDir.
	var projectGitDir string
	if projectDir != "" {
		if root := projectRootFromWorktree(projectDir); root != "" {
			projectGitDir = filepath.Join(root, ".git")
		}
	}

	// Build container config
	config := &runtime.ContainerConfig{
		Image:          image,
		ProjectID:      task.ProjectID,
		Role:           role,
		TaskID:         task.ID,
		EnvVars:        envVars,
		InputDir:       inputDir,
		OutputDir:      outputDir,
		WorkspaceDir:   workspaceDir,
		ProjectDir:     projectDir,
		ProjectGitDir:  projectGitDir,
		TimeoutSeconds: int(timeout.Seconds()),
		// Per-role network policy (mitigation plan §7.1 step A). Empty
		// preserves the permissive default; roles opt into stricter
		// modes via runtime.network in their swarm config.
		Network: runtime.NetworkMode(roleConfig.Runtime.Network),
	}

	// Start the container - runtime will handle its own tracing
	containerID, err := e.runtime.StartContainer(ctx, config)
	if err != nil {
		return "", fmt.Errorf("failed to start container: %w", err)
	}

	return containerID, nil
}

// injectPerTaskKey mints a scoped per-task API key and overwrites both
// VORNIK_API_KEY and VORNIK_LLM_API_KEY in envVars. Returns true when a
// fresh key was minted so the caller can register a paired revokeTaskKey
// defer. If the minter is nil or mint fails the env map is left unchanged
// (static key passes through) and false is returned — availability beats
// key freshness.
//
// Finding B1(a): VORNIK_LLM_API_KEY is the credential the agent
// entrypoint presents to the daemon's chat-completions proxy
// (agent_llm.endpoint IS the daemon). It previously carried the
// UNSCOPED static agent key, so a prompt-injected agent could read
// $VORNIK_LLM_API_KEY from its own env and replay it as an all-access
// credential for cross-project writes and mcp/tools/call (incl. broker
// place_order). Overwriting it with the same project-scoped per-task key
// leaves NO unscoped credential inside the container. The chat proxy
// accepts a project-scoped key for the agent's own project
// (chat_proxy.go → requestAllowsProject), so this is safe.
func (e *Executor) injectPerTaskKey(ctx context.Context, projectID, taskID string, envVars map[string]string) bool {
	if e.apiKeyMinter == nil || projectID == "" {
		return false
	}
	raw, err := e.apiKeyMinter.MintTaskKey(ctx, projectID, taskID)
	if err != nil {
		e.logger.Warn().Err(err).Str("task_id", taskID).
			Msg("per-task key mint failed; falling back to static agent key")
		return false
	}
	envVars["VORNIK_API_KEY"] = raw
	envVars["VORNIK_LLM_API_KEY"] = raw
	return true
}

// warmProjectKeyMinter is an OPTIONAL capability on top of APIKeyMinter
// (Finding B1(b)). Warm-pool containers are baked once and reused across
// tasks, so a per-TASK key would go stale on reuse; instead they get a
// PROJECT-scoped key. Pools are already keyed per (project, role, image),
// so a project-scoped key is stable for the container's lifetime and
// closes the HIGH-severity cross-project escalation that the unscoped
// static key opened. The interface is defined here (not in executor.go)
// and discovered via type assertion so dev/sqlite minters that only
// implement APIKeyMinter keep working unchanged.
type warmProjectKeyMinter interface {
	// MintProjectScopedKey returns a RAW project-scoped key for the
	// warm pool's (projectID, role). The implementation persists only
	// the hash. role is advisory provenance only — scope is the
	// project.
	MintProjectScopedKey(ctx context.Context, projectID, role string) (string, error)
}

// injectWarmProjectKey mints a project-scoped key for a warm-pool
// container and overwrites VORNIK_API_KEY + VORNIK_LLM_API_KEY in the
// warm roleEnv (Finding B1(b)). Returns true when a fresh scoped key was
// injected. When the minter is nil, doesn't implement
// warmProjectKeyMinter, projectID is empty, or the mint fails, the env
// is left unchanged and false is returned — availability beats key
// freshness, exactly like the ephemeral injectPerTaskKey path.
//
// Without this, warm containers carry the unscoped static agent key as
// both their daemon-callback credential AND their LLM-proxy credential,
// which a prompt-injected agent can replay for cross-project writes and
// mcp/tools/call (incl. broker place_order).
func (e *Executor) injectWarmProjectKey(ctx context.Context, projectID, role string, roleEnv map[string]string) bool {
	if e.apiKeyMinter == nil || projectID == "" {
		return false
	}
	pm, ok := e.apiKeyMinter.(warmProjectKeyMinter)
	if !ok {
		return false
	}
	raw, err := pm.MintProjectScopedKey(ctx, projectID, role)
	if err != nil || raw == "" {
		if err != nil {
			e.logger.Warn().Err(err).Str("project", projectID).Str("role", role).
				Msg("warm-pool project-scoped key mint failed; falling back to static agent key")
		}
		return false
	}
	roleEnv["VORNIK_API_KEY"] = raw
	roleEnv["VORNIK_LLM_API_KEY"] = raw
	return true
}

// revokeTaskKey revokes the per-task key for taskID. It is safe to call
// with a nil minter (no-op). Runs on a fresh background context with a
// 10-second timeout so a cancelled parent context (e.g. task timeout)
// still triggers cleanup.
func (e *Executor) revokeTaskKey(taskID string) {
	if e.apiKeyMinter == nil {
		return
	}
	revokeCtx, revokeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer revokeCancel()
	if rErr := e.apiKeyMinter.RevokeTaskKey(revokeCtx, taskID); rErr != nil {
		e.logger.Warn().Err(rErr).Str("task_id", taskID).
			Msg("per-task key revoke failed; key will expire via expires_at")
	}
}

// buildMCPConfig serialises project MCP servers into the mcp.json format consumed
// by the mcp-bridge binary inside agent containers. Environment variable references
// of the form ${VAR} in Env values are expanded using the host daemon's environment.
// The `allowed_tools` field is passed through so the in-container mcp.Client
// applies the same filter the host-side manager does — otherwise an agent could
// see tools the project scope-restricted.
func buildMCPConfig(servers []registry.MCPServerConfig) ([]byte, error) {
	type mcpServerEntry struct {
		Name         string            `json:"name"`
		Transport    string            `json:"transport"`
		Command      string            `json:"command,omitempty"`
		Args         []string          `json:"args,omitempty"`
		Env          map[string]string `json:"env,omitempty"`
		URL          string            `json:"url,omitempty"`
		AllowedTools []string          `json:"allowed_tools,omitempty"`
	}
	entries := make([]mcpServerEntry, 0, len(servers))
	for _, s := range servers {
		expanded := make(map[string]string, len(s.Env))
		for k, v := range s.Env {
			expanded[k] = expandMCPEnvSafe(v)
		}
		entries = append(entries, mcpServerEntry{
			Name:         s.Name,
			Transport:    s.Transport,
			Command:      s.Command,
			Args:         s.Args,
			Env:          expanded,
			URL:          s.URL,
			AllowedTools: s.AllowedTools,
		})
	}
	return json.Marshal(entries)
}

func shouldWriteMCPConfig(agentEnv map[string]string) bool {
	return strings.TrimSpace(agentEnv["VORNIK_API_URL"]) == ""
}

func expandMCPEnvSafe(s string) string {
	return os.Expand(s, func(name string) string {
		if strings.HasPrefix(name, "VORNIK_") {
			return ""
		}
		return os.Getenv(name)
	})
}

// waitForCompletion waits for the container to finish.
func (e *Executor) waitForCompletion(ctx context.Context, containerID string, timeout time.Duration) (int, error) {
	// Use context with timeout
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return e.runtime.WaitForExit(ctx, containerID, timeout)
}

// allowedStagingRoots returns the host paths the artifact-staging code
// will accept as legitimate sources for input_files. The roots are:
//
//   - os.TempDir() — previous-step workspace dirs and /tmp-fallback
//     Telegram uploads;
//   - <projectWorkspacePath>/<projectID>/uploads/ — Telegram uploads
//     that landed in the project-workspace tree (legacy path; the
//     dispatcher now snapshots these into the artifact store, but
//     the allowlist keeps backwards compat for in-flight retries
//     that pre-date the snapshotting change);
//   - <artifactStoragePath> — durable INPUT artifacts the dispatcher
//     snapshotted on create_task. This is the load-bearing entry
//     for retry survival.
//
// stageInputArtifacts materialises the supplied artifacts into the
// step's EPHEMERAL workspace and rewrites each art["path"] to the
// container-relative path the role will read from. Routing depends on
// the artifact's class:
//
//   - class=="output" — a prior step's product handed forward via the
//     artifact store (task e9a5). Staged into <workspaceDir>/artifacts/
//     out/<name>, the directory roles read their inputs from in a
//     multi-step STATIC workflow. art["path"] is rewritten to
//     /app/workspace/artifacts/out/<safeName>.
//   - otherwise — a task upload (Telegram attachment, snapshotted
//     input). Existing contract: staged into <workspaceDir>/artifacts/
//     in/<name>, rewritten to /app/workspace/artifacts/in/<safeName>.
//
// Security is unchanged: srcPath (preferring "sourcePath", falling back
// to the agent-written "path") must resolve under allowedRoots via
// resolveStagingSrc — the durable store path passes because the store
// root is allowed, while an agent's raw container path is rejected.
// Names are sanitised with safepath.CleanFileName and targets joined
// with safepath.JoinUnder; files are written 0o600 (inputs may be
// operator-private). Returns an error only if a staging directory can't
// be created; individual unstageable artifacts are skipped (logged),
// preserving the pre-extraction best-effort behaviour.
func (e *Executor) stageInputArtifacts(workspaceDir string, arts []map[string]string, allowedRoots []string) error {
	artifactsInDir := filepath.Join(workspaceDir, "artifacts", "in")
	artifactsOutDir := filepath.Join(workspaceDir, "artifacts", "out")
	for _, art := range arts {
		srcPath := art["sourcePath"]
		if srcPath == "" {
			srcPath = art["path"] // agent writes "path", not "sourcePath"
		}
		name := art["name"]
		if srcPath == "" || name == "" {
			continue
		}
		// Security: only stage files from a small allowlist of host
		// paths. Artifact paths come from agent-controlled result.json
		// and could otherwise reference arbitrary host files (e.g.
		// /etc/passwd). resolveStagingSrc handles the dangerous bits
		// (absolute resolution before the allowed-roots gate, symlink
		// containment) and returns the canonical path the rest of the
		// staging code reads from.
		absSrc, ok := resolveStagingSrc(srcPath, allowedRoots)
		if !ok {
			e.logger.Warn().
				Str("src_path", srcPath).
				Strs("allowed_roots", allowedRoots).
				Msg("artifact staging: rejecting path outside allowed roots")
			continue
		}
		safeName, err := safepath.CleanFileName(name)
		if err != nil {
			continue
		}
		data, readErr := os.ReadFile(absSrc)
		if readErr != nil {
			continue
		}

		// Route by class: prior-step outputs go where roles read their
		// upstream products (artifacts/out/); task uploads keep the
		// legacy artifacts/in/ destination.
		var destDir, containerPrefix string
		if art["class"] == "output" {
			destDir = artifactsOutDir
			containerPrefix = "/app/workspace/artifacts/out/"
		} else {
			destDir = artifactsInDir
			containerPrefix = "/app/workspace/artifacts/in/"
		}
		if mkErr := os.MkdirAll(destDir, 0o755); mkErr != nil {
			return fmt.Errorf("create staging dir %s: %w", destDir, mkErr)
		}
		targetPath, joinErr := safepath.JoinUnder(destDir, safeName)
		if joinErr != nil {
			continue
		}
		// 0o600 — input artifacts can be operator-private
		// (uploaded credentials, user PII). Container reads
		// as the same UID.
		_ = os.WriteFile(targetPath, data, 0o600)
		// Rewrite path to container-relative path for task.json.
		art["path"] = containerPrefix + safeName
	}
	return nil
}

// Symlinks are resolved so a caller can't smuggle a path through a
// symlink farm.
func allowedStagingRoots(projectWorkspacePath, projectID, artifactStoragePath string) []string {
	roots := []string{}
	add := func(p string) {
		if p == "" {
			return
		}
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			roots = append(roots, resolved)
		} else {
			roots = append(roots, filepath.Clean(p))
		}
	}
	add(os.TempDir())
	if projectWorkspacePath != "" && projectID != "" {
		add(filepath.Join(projectWorkspacePath, projectID, "uploads"))
	}
	add(artifactStoragePath)
	return roots
}

// resolveStagingSrc canonicalises an agent-supplied artifact source
// path before it reaches the allowed-roots gate, then reports whether
// the result lives beneath one of the allowed roots. Returns
// (absolutePath, true) on accept; ("", false) on any path that:
//   - cannot be resolved to absolute (rare — filepath.Abs only fails
//     when os.Getwd fails, e.g. CWD was deleted)
//   - lands outside every allowed root after symlink resolution.
//
// The pre-audit code only ran the gate when filepath.IsAbs(srcPath)
// returned true, so a relative path like "./../../etc/passwd" walked
// straight through to os.ReadFile and Go resolved it against the
// daemon's CWD. Always-absolute is the fix.
func resolveStagingSrc(srcPath string, allowedRoots []string) (string, bool) {
	if srcPath == "" {
		return "", false
	}
	absSrc, err := filepath.Abs(srcPath)
	if err != nil {
		return "", false
	}
	cleanSrc, err := filepath.EvalSymlinks(absSrc)
	if err != nil {
		cleanSrc = filepath.Clean(absSrc)
	}
	if !pathUnderAny(cleanSrc, allowedRoots) {
		return "", false
	}
	// Return the symlink-resolved path so the subsequent
	// os.ReadFile uses the same target the gate just validated.
	// Returning absSrc would re-resolve symlinks at read time —
	// a TOCTOU window where an attacker who can swap a symlink
	// between EvalSymlinks here and ReadFile at the call site
	// reads through to a different file outside any allowed
	// root. Closes that race.
	return cleanSrc, true
}

// pathUnderAny reports whether cleanPath is exactly one of the roots
// or sits beneath one. Both arguments must already be EvalSymlinks-
// resolved (or filepath.Clean-fallback) to avoid path-confusion bugs.
func pathUnderAny(cleanPath string, roots []string) bool {
	for _, r := range roots {
		if cleanPath == r {
			return true
		}
		if strings.HasPrefix(cleanPath, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// recordLearnedBudgetApplication appends an instinct_applications row for a
// learned-tier surfacing on the tool_budget surface (Slice 4, LLD §7). The
// row is recorded with result="ignored" at apply time; the feedback loop
// grades it later when consumers.application_feedback is enabled.
//
// Best-effort: any error is logged at Debug level and never blocks the step.
// The Prometheus counter is bumped regardless of the RecordApplication outcome
// (the surfacing happened whether or not the feedback row persisted).
func (e *Executor) recordLearnedBudgetApplication(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	stepID string,
	instinctID string,
) {
	if e == nil || e.instinctRepo == nil {
		return
	}
	if instinctID == "" {
		return
	}
	taskID := ""
	execID := ""
	if task != nil {
		taskID = task.ID
	}
	if execution != nil {
		execID = execution.ID
	}
	appErr := e.instinctRepo.RecordApplication(ctx, &persistence.InstinctApplication{
		InstinctID:  instinctID,
		TaskID:      taskID,
		Surface:     persistence.InstinctSurfaceToolBudget,
		Result:      persistence.InstinctResultIgnored,
		ExecutionID: execID,
		StepID:      stepID,
	})
	if appErr != nil {
		e.logger.Debug().Err(appErr).
			Str("instinct_id", instinctID).
			Msg("recording instinct application (tool_budget) failed; non-fatal")
	}
	// Bump the surfacing counter regardless of the RecordApplication outcome.
	// Nil-safe — an executor without a metrics sink simply doesn't emit.
	if e.instinctMetrics != nil && e.instinctMetrics.ApplicationsTotal != nil {
		e.instinctMetrics.ApplicationsTotal.WithLabelValues(
			persistence.InstinctSurfaceToolBudget, persistence.InstinctResultIgnored).Inc()
	}
}

// agentQualityOutcome extracts the AGENT's step-quality label from a
// result.json — `iteration_exhausted`, `prompt_token_budget` or
// `budget_tripwire` — and its detail.
//
// It reads `agentOutcome` first and falls back to `outcome`, which is where the
// agent wrote this label before 2026-08-19. The fallback matters because the
// daemon and the agent image are deployed separately, so a newer daemon
// routinely runs against an older image.
//
// The fallback is SAFE ONLY BECAUSE it is whitelisted to the three agent labels.
// The legacy `outcome` field is also where the LEAD emits its workflow decision
// (checkpoint / external_wait / closure_request), and reading a `checkpoint`
// here would record a lead decision as a budget bail. Sharing that one field is
// exactly the collision this split exists to end: the agent's label was
// injected after the structured merge and destroyed the lead's decision, which
// failed every recovery attempt while the response artifact looked perfect.
func agentQualityOutcome(resultBytes []byte) (outcome, detail string) {
	var parsed struct {
		AgentOutcome       string `json:"agentOutcome"`
		AgentOutcomeDetail string `json:"agentOutcomeDetail"`
		Outcome            string `json:"outcome"`
		OutcomeDetail      string `json:"outcomeDetail"`
	}
	if json.Unmarshal(resultBytes, &parsed) != nil {
		return "", ""
	}
	if parsed.AgentOutcome != "" {
		return parsed.AgentOutcome, parsed.AgentOutcomeDetail
	}
	switch parsed.Outcome {
	case string(stepoutcome.BudgetTripwire),
		string(stepoutcome.PromptTokenBudget),
		string(stepoutcome.IterationExhausted):
		return parsed.Outcome, parsed.OutcomeDetail
	}
	return "", ""
}

// containerLogMaxBytes bounds the container-log tail appended to a step error.
//
// 16 KiB keeps the diagnostic value — the failure is in the last few lines, and
// 16 KiB is far more than that — while bounding what can reach a downstream
// PROMPT.
const containerLogMaxBytes = 16 * 1024

// containerLogTailLines is how many lines we ask the runtime for. The BYTE cap
// above is the real bound; this only decides how deep a window we fetch before
// that cap applies.
//
// Was 50, which measurement showed was too shallow to diagnose with: the agent
// emits a preflight line and an iteration line per iteration, so any step with a
// real tool phase fills 50 lines with its own bookkeeping and pushes the
// step-end decisions out of the window. On the dev-pipeline report step
// (2026-08-20) that cost the one line needed to tell two causes apart — whether
// the tool-free schema finalization fired, or the no-tool nudge did — so a
// failing step's log tail could show 40 iterations of preamble and nothing about
// how it ended.
//
// Raising it is close to free precisely BECAUSE capContainerLog bounds bytes: a
// deeper window cannot widen the blast radius the byte cap exists to contain, it
// just stops the interesting lines being evicted before the cap ever runs.
const containerLogTailLines = 400

// containerLogSection renders the log tail appended to a step error: a header
// naming the window actually requested, and the byte-capped body.
//
// The header text and the line count used to be independent literals — the
// runtime was asked for 50 while the header said "last 50 lines" — so changing
// one silently made the other lie. Deriving the text from the constant is what
// keeps them honest.
func containerLogSection(logs string) string {
	return fmt.Sprintf("%slast %d lines) ---\n%s",
		containerLogDelimiter, containerLogTailLines, capContainerLog(logs))
}

// truncateDetailPreservingEnds bounds a step's error_detail while keeping BOTH
// ends, because the two useful parts sit at opposite ones: the daemon's own
// message at the head, and the container log's last lines at the tail — where
// the step decided what to do.
//
// A single-ended truncation cannot serve this field, and the head-keeping one
// that used to (truncateStr, s[:max]) silently defeated capContainerLog. That
// function keeps the log's TAIL on the explicit reasoning that "the failure is at
// the end. Keeping the head would discard exactly the part worth reading" — and
// then the 2000-byte head-truncation at the persistence boundary discarded it
// again. Net effect: every stored detail was the daemon's message plus the
// agent's startup and earliest iterations.
//
// Measured 2026-08-20: that is why raising containerLogTailLines from 50 to 400
// changed nothing observable. The deeper tail was fetched and appended, then cut
// off. Step-end lines — the schema finalization, the no-tool nudge, the iteration
// cap — are beyond 2000 bytes by construction and could never be stored, so a
// whole day's failures were diagnosed from evidence that structurally excluded
// the answer.
//
// Split is 60/40 head/tail: the message is short and bounded, so most of the
// budget goes to the log, but the head share must comfortably exceed the longest
// daemon message or the reader loses what the step was even failed for.
func truncateDetailPreservingEnds(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	const notice = "\n[… %d bytes elided …]\n"
	headLen := limit * 6 / 10
	tailLen := limit - headLen
	elided := len(s) - headLen - tailLen
	if headLen <= 0 || tailLen <= 0 || elided <= 0 {
		// Cap too small to split meaningfully — keep the head, which at least
		// names the failure.
		return s[:limit]
	}
	return s[:headLen] + fmt.Sprintf(notice, elided) + s[len(s)-tailLen:]
}

// containerLogDelimiter opens the log section appended to a step error.
//
// It is the boundary between what the DAEMON concluded and what the CONTAINER
// printed. Only the former is a machine contract: the retry ladder routes on
// substrings of the error ("plausibility violation", "schema violation:",
// "output contract for step"), and every one of those phrases is also printable
// by an agent — most easily by echoing the corrective hint the ladder itself
// injected on the previous attempt. Classifiers therefore cut here first, via
// errorBeforeLogTail, so diagnostics can never steer control flow.
const containerLogDelimiter = "\n\n--- Container Log ("

// containerExitError carries a container's exit status alongside the failure
// message, so the step-outcome row can record the code instead of leaving the
// residual bucket undiagnosable.
//
// Measured 2026-08-26: `container_non_zero_exit` was 52.3% of classified step
// failures and only 0.4% of those rows held an exit code, because the code
// reached error_detail only through the uncommon `container exited with code
// %d` fallback. The value was known here all along and simply never travelled.
//
// A typed error rather than an extra return value: the failure crosses several
// wrappers before it is recorded, and this is the shape the codebase already
// uses for the same problem — ClassifyExecutionFailure lifts a
// `FailureClass() string` off the error the same way.
//
// Error() returns the message VERBATIM. Both classifiers on this path
// (ClassifyExecutionFailure and refineAgentFailureOutcome) match on message
// text, so decorating it here would silently reclassify every container
// failure.
type containerExitError struct {
	exitCode int
	msg      string
}

func newContainerExitError(exitCode int, msg string) *containerExitError {
	return &containerExitError{exitCode: exitCode, msg: msg}
}

func (e *containerExitError) Error() string { return e.msg }

// ContainerExitCode reports the container's exit status. Zero is a real value
// — a container can exit 0 and still fail the step on a verifier rejection —
// so callers must distinguish it from "no container ran", which is the absence
// of this interface, not a zero from it.
func (e *containerExitError) ContainerExitCode() int { return e.exitCode }

// containerExitCodeFromError lifts a container exit status out of an error
// chain, or nil when the error did not come from a container. nil means "no
// container ran" and is NOT interchangeable with a zero code.
func containerExitCodeFromError(err error) *int {
	if err == nil {
		return nil
	}
	var coded interface{ ContainerExitCode() int }
	if errors.As(err, &coded) {
		code := coded.ContainerExitCode()
		return &code
	}
	return nil
}

// errorBeforeLogTail returns msg with any appended container-log section
// removed, leaving only the daemon's own message.
func errorBeforeLogTail(msg string) string {
	if i := strings.Index(msg, containerLogDelimiter); i >= 0 {
		return msg[:i]
	}
	return msg
}

// capContainerLog bounds the container-log tail by BYTES, keeping the end.
//
// The append is bounded by 50 LINES, which is no bound at all: one line can be
// arbitrarily long. Measured 2026-08-19 — the recovery-probe fixture's 1s
// timeout produced a runtime error whose log tail turned the lead's recovery
// context into a crash blob; the lead then generated ~1 MB responses over ~149s,
// tripped the model-health circuit breaker, and failed 24 of 27 attempts with
// MODEL_UNHEALTHY. Two measurements were invalidated before the cause was found.
//
// This matters beyond operator display: the error string is echoed into
// context.recovery.failure_reason and becomes part of the NEXT agent's prompt.
// An unbounded blob there is a correctness problem.
//
// Keeps the TAIL because that is where the failure is, and says it truncated so
// a partial tail is not read as a whole log.
func capContainerLog(logs string) string {
	if len(logs) <= containerLogMaxBytes {
		return logs
	}
	tail := logs[len(logs)-containerLogMaxBytes:]
	// Start at a line boundary when one is close by, so the first retained line
	// is not a fragment. Bounded search: a log with no newlines keeps the raw
	// tail rather than scanning the whole string.
	if idx := strings.IndexByte(tail, '\n'); idx >= 0 && idx < 1024 {
		tail = tail[idx+1:]
	}
	return fmt.Sprintf("[... %d bytes truncated; showing the last %d bytes ...]\n%s",
		len(logs)-len(tail), len(tail), tail)
}
