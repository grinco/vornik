// Package playbook returns operator-actionable remediations for a
// given task failure class. The corpus is rule-based for now —
// historically-effective recovery-rate analysis is a deferred
// follow-on; a flat lookup ships value today and the Explainer +
// failed-task UI both have a natural surface to render it.
//
// Each entry covers:
//   - Cause: one-line plain-English description of what the class means.
//   - Suggestions: ordered list of concrete things to try, cheapest-first.
//   - References: pointers to docs/CLI commands an operator can run.
//
// Adding a class: append a Lookup entry below; tests catch missing
// failure classes via TestPlaybookCoversAllFailureClasses.
package playbook

import (
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/stepoutcome"
)

// Scope names which failure-class vocabulary an entry belongs to. The corpus
// is one flat map over two disjoint vocabularies — task classes
// (persistence.TaskFailureClass*, UPPER_SNAKE, describing a whole task) and
// step classes (stepoutcome.Class*, lower_snake, describing one step of one
// execution). Keys cannot collide, so Lookup needs no scope argument; this
// field exists so a surface can say which lifetime it is describing rather
// than leaving the operator to infer it from the case of the string.
//
// The disjointness and the case conventions are enforced by
// internal/playbook/vocabulary_test.go, not by convention alone.
type Scope string

// Scope values. One per failure-class vocabulary.
const (
	// ScopeTask — persistence.TaskFailureClass*, describing a whole task.
	ScopeTask Scope = "task"
	// ScopeStep — stepoutcome.Class*, describing one step of one execution.
	ScopeStep Scope = "step"
)

// Entry is the per-class remediation record.
type Entry struct {
	// Scope is the vocabulary this entry's Class belongs to.
	Scope Scope `json:"scope,omitempty"`

	// Class is the persistence.TaskFailureClass* string the entry
	// matches. Mirrored on the wire so consumers can group rows
	// without re-running the lookup.
	Class string `json:"class"`
	// HumanMessage is the end-user-friendly one-sentence
	// explanation. Added 2026.6.0 SaaS-readiness for surfaces
	// where the audience is the project's user (Telegram chat
	// reply, web UI failed-task primary banner), not the
	// operator. Avoids jargon like "iteration cap", "shape
	// retry", "context deadline" — words a Telegram-only user
	// has never seen. Falls back to Cause when empty.
	HumanMessage string `json:"humanMessage,omitempty"`
	// Cause is a one-line plain-English description of what the
	// failure class actually means. Operator-facing — uses the
	// system's vocabulary (iteration cap, shape retry, etc.).
	// Renders above the suggestions so the operator sees WHY
	// before WHAT.
	Cause string `json:"cause"`
	// Suggestions are ordered cheapest-action-first. Each line is a
	// single concrete thing the operator can try. Avoid prose
	// paragraphs — these get surfaced in compact UI / CLI tables.
	Suggestions []string `json:"suggestions"`
	// References point at docs / CLI / config that elaborate on a
	// suggestion. Optional but encouraged when the suggestion alone
	// requires context the operator might not have.
	References []string `json:"references,omitempty"`
}

// HumanFriendly returns the audience-appropriate one-line summary
// — HumanMessage when set, otherwise Cause as the fallback. Saves
// every consumer the same nil-check boilerplate.
func (e Entry) HumanFriendly() string {
	if e.HumanMessage != "" {
		return e.HumanMessage
	}
	return e.Cause
}

// Lookup returns the playbook entry for a failure class, or a
// generic "unknown class" entry when the class isn't in the corpus.
// Never returns nil — the consumer always gets something to render.
func Lookup(class string) Entry {
	if e, ok := corpus[class]; ok {
		return e
	}
	return Entry{
		Class:        class,
		HumanMessage: "Something went wrong, but the cause isn't a known pattern. Try again — if it keeps happening, share this task ID with your administrator.",
		Cause:        "Unrecognised failure class. The classifier didn't match any known pattern.",
		Suggestions: []string{
			"Read the task's last_error verbatim — most unrecognised classes have a clear textual cause.",
			"Check internal/executor/failure_classifier.go to see whether a new pattern should be added.",
			"Run `vornikctl task explain <id>` to get an LLM-generated summary of the failure context.",
		},
	}
}

// All returns the complete corpus, sorted by class name. Powers
// `vornikctl playbook list` and the failed-task UI's class index.
func All() []Entry {
	out := make([]Entry, 0, len(corpus))
	for _, e := range corpus {
		out = append(out, e)
	}
	// Stable order — class string ascending.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Class < out[j-1].Class; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// corpus is the rule-based playbook indexed by failure class. Keep
// each entry compact and operator-actionable — avoid documentation-
// style prose. When a class lands a new shipped behaviour (e.g.
// model fallback, checkpoint+continuation), update the affected
// entries here so the suggestion order reflects what the daemon
// already does for the operator.
var corpus = map[string]Entry{
	persistence.TaskFailureClassToolIterationLimit: {
		Class:        persistence.TaskFailureClassToolIterationLimit,
		Scope:        ScopeTask,
		HumanMessage: "Your agent took too many steps and didn't finish in time. Try a smaller scope or a more focused request.",
		Cause:        "The agent burned its VORNIK_MAX_TOOL_ITERATIONS budget without producing a final answer. The reasoning loop didn't converge in time.",
		Suggestions: []string{
			"Confirm a checkpoint follow-up task was scheduled — the executor auto-creates one when it merges partial work; check the task's ParentTaskID chain.",
			"Set `modelFallback: moonshotai.kimi-k2.5` (or another stronger model) on the failing role in the swarm YAML so the next attempt retries on a model that converges.",
			"If the role's cap is unusually low (feasibility:14, scout:28), raise VORNIK_MAX_TOOL_ITERATIONS in the role's envVars by ~30%.",
			"Check the tool_audit_log for the task — a degenerate loop (3+ identical tool calls) means the prompt needs tightening, not a higher cap.",
		},
		References: []string{
			"Backlog: 'Reliability / hardening — deferred' (model fallback + checkpoint chain are in place since 2026-04-30).",
		},
	},
	persistence.TaskFailureClassToolError: {
		Class:        persistence.TaskFailureClassToolError,
		Scope:        ScopeTask,
		HumanMessage: "One of the tools the agent tried to use failed. The agent's environment ran into a problem (missing command, permission issue, or similar).",
		Cause:        "A specific tool call inside the agent (shell, file_write, run_shell, podman) failed. Distinct from iteration-limit failures: the model wasn't running out of turns, a single command broke.",
		Suggestions: []string{
			"Open the task's tool_audit_log and find the last failing tool entry — its stderr usually points at the exact command/path.",
			"Common causes: missing binary (git-lfs, jq), permission denied (worktree mount), command syntax (cd && rm -rf .worktrees).",
			"If the tool is shell-based and the agent quoted user input verbatim, sanitize the prompt — quoting bugs trip this class often.",
		},
	},
	persistence.TaskFailureClassInvalidOutput: {
		Class:        persistence.TaskFailureClassInvalidOutput,
		Scope:        ScopeTask,
		HumanMessage: "The agent answered, but its response didn't match the expected format. Often resolves on retry; the model may need a clearer prompt.",
		Cause:        "The agent emitted parseable JSON, but it failed shape validation (requiredOutputKeys) or plausibility rules (e.g. {approved:true, feedback:''}).",
		Suggestions: []string{
			"The shape-retry layer already re-prompted with a corrective hint once. If still failing, the role's prompt needs explicit examples of the required output shape.",
			"Tighten the role's system prompt with a concrete '## Output\\n```json\\n{...}\\n```' example block.",
			"Check whether the model is too small for the JSON shape required — Gemma-4 frequently produces parseable-but-empty outputs that trip plausibility rules.",
			"If the failure is plausibility (not schema), confirm the rule's intent matches the role's actual contract — sometimes the rule is wrong.",
		},
	},
	persistence.TaskFailureClassLLMError: {
		Class:        persistence.TaskFailureClassLLMError,
		Scope:        ScopeTask,
		HumanMessage: "The AI model service had a problem and couldn't complete your request. Usually a transient outage — retrying often works.",
		Cause:        "The chat provider returned an error, the gateway refused to route, or the response stream broke mid-flight.",
		Suggestions: []string{
			"Run `vornikctl models list --provider <provider>` to confirm the model is still in the gateway's catalog.",
			"Check `journalctl --user -u vornik | grep gateway` for upstream 5xx clusters that line up with the failure timestamp.",
			"If a single provider is failing repeatedly, set a `modelFallback:` on the affected role to a different vendor.",
		},
		References: []string{
			"Backlog item E (deferred): 'Provider-level rate-limit backpressure at the scheduler' would auto-pause new task starts during a wide outage.",
		},
	},
	persistence.TaskFailureClassRateLimited: {
		Class:        persistence.TaskFailureClassRateLimited,
		Scope:        ScopeTask,
		HumanMessage: "Your AI model provider is rate-limiting you. Wait a few minutes and try again, or contact your administrator about quotas.",
		Cause:        "The chat provider returned 429 / rate-limit. The infra-retry layer already backed off; the limit is sustained, not transient.",
		Suggestions: []string{
			"Check the provider's usage dashboard — most 429 patterns clear when the rolling minute/hour window passes.",
			"If you're running parallel tasks against the same provider, drop scheduler.max_concurrent_tasks until the rate-limit window resolves.",
			"Set `modelFallback:` to a different provider on the affected roles so retries don't all hit the same upstream.",
		},
	},
	persistence.TaskFailureClassTimeout: {
		Class:        persistence.TaskFailureClassTimeout,
		Scope:        ScopeTask,
		HumanMessage: "Your task took longer than allowed and was stopped. Try a smaller scope, or ask your administrator to raise the time limit.",
		Cause:        "The execution context's deadline elapsed. Either workflow.maxWallClock fired (proactive cap) or a step's timeout: ... was exceeded.",
		Suggestions: []string{
			"Check the workflow YAML for `maxWallClock:` — if set, this is the proactive ceiling.",
			"Per-step timeouts (workflow.steps.<id>.timeout) are independent of maxWallClock; tighten or relax based on observed step duration in the dashboard.",
			"If the run was making forward progress under the watchdog's no-progress threshold, raising the cap is the right call. If not, the agent was stuck — see TOOL_ITERATION_LIMIT instead.",
		},
	},
	persistence.TaskFailureClassMergeFailed: {
		Class:        persistence.TaskFailureClassMergeFailed,
		Scope:        ScopeTask,
		HumanMessage: "The agent's work conflicted with concurrent changes and couldn't be merged. Try again, or contact your administrator to resolve the conflict.",
		Cause:        "The agent's worktree merge to the project's main branch failed — usually because the worktree had commits that conflict with concurrent main-branch state.",
		Suggestions: []string{
			"The worktree is preserved at `<projectDir>/.worktrees/<taskID>` for manual recovery — the commits live there.",
			"Salvage manually: `cd <projectDir> && git merge worktree/<taskID> --no-ff` — usually a 3-way merge resolves it.",
			"If concurrent task scheduling on the same project is the root cause, set scheduler concurrency to 1 for that project until the conflict pattern is understood.",
		},
	},
	persistence.TaskFailureClassGateFailed: {
		Class:        persistence.TaskFailureClassGateFailed,
		Scope:        ScopeTask,
		HumanMessage: "A reviewer step in your workflow rejected the work. The previous step's output didn't pass quality gates.",
		Cause:        "A workflow gate evaluated false against the producer's output (e.g. reviewer.approved == true returned false).",
		Suggestions: []string{
			"Read the producer step's output — the gate condition + last_error name the field that didn't match.",
			"This is usually correct gate behaviour: the upstream role decided not to approve. Check whether the on_fail branch handles it.",
			"If the gate is mis-firing, check the condition syntax — string equality vs boolean, and the field's exact key.",
		},
	},
	persistence.TaskFailureClassBudgetBlocked: {
		Class:        persistence.TaskFailureClassBudgetBlocked,
		Scope:        ScopeTask,
		HumanMessage: "This project hit its spending limit. Wait until the next billing window, or contact your administrator to raise the cap.",
		Cause:        "The project's daily or monthly hard cap was exceeded. Healthy enforcement, not an error.",
		Suggestions: []string{
			"Check `vornikctl project show <id>` for the configured cap and current spend.",
			"Raise the cap in the project YAML (`autonomy.daily_hard_usd` / `autonomy.monthly_hard_usd`) if the spend was legitimate.",
			"Investigate the per-role cost breakdown on `/ui/spend?project=<id>` — a runaway role is the usual culprit.",
		},
	},
	persistence.TaskFailureClassWorkflowRole: {
		Class:        persistence.TaskFailureClassWorkflowRole,
		Scope:        ScopeTask,
		HumanMessage: "This project's configuration has a mismatch. Contact your administrator — the workflow refers to a role that isn't in this swarm.",
		Cause:        "A workflow step references a role name that doesn't exist in the swarm assigned to the project.",
		Suggestions: []string{
			"Run `vornikctl doctor` — workflow_swarm_compat surfaces these mismatches with the exact role + swarm.",
			"Either add the role to the swarm YAML or rename the workflow step's `role:` to match an existing role.",
			"For adaptive workflows the lead role is auto-substituted via swarm.leadRole — check that's set.",
		},
	},
	persistence.TaskFailureClassWorkflowCfg: {
		Class:        persistence.TaskFailureClassWorkflowCfg,
		Scope:        ScopeTask,
		HumanMessage: "This project's workflow configuration has an error. Contact your administrator.",
		Cause:        "The workflow YAML referenced a step ID that doesn't exist (typo in on_success / on_fail / gate target) or the workflow file was malformed.",
		Suggestions: []string{
			"Run `vornikctl doctor` — config_validation surfaces YAML syntax errors and unresolved step references.",
			"Open the workflow YAML and grep for the unresolved step ID; it's almost always a typo in on_success / on_fail / gates[].target.",
		},
	},
	persistence.TaskFailureClassWorkflowDrift: {
		Class:        persistence.TaskFailureClassWorkflowDrift,
		Scope:        ScopeTask,
		HumanMessage: "This project's workflow was changed while a task was running. Please retry — the new run will use the current configuration.",
		Cause:        "The execution stored a workflow hash that no longer matches the live YAML, AND no snapshot was captured (legacy execution row).",
		Suggestions: []string{
			"Should not happen for executions started after 2026-04-30 — workflow snapshots eliminate this class. Check the execution's created_at.",
			"If the row is post-snapshot and still hits this class, file an issue: `e.execRepo.SetWorkflowSnapshot` failed silently at execution start.",
			"For legacy rows: revert the workflow YAML to the version that was active when the execution started, or cancel and reschedule the task.",
		},
	},
	persistence.TaskFailureClassStuckExecution: {
		Class:        persistence.TaskFailureClassStuckExecution,
		Scope:        ScopeTask,
		HumanMessage: "Your task stopped making progress and was stopped automatically. The agent was likely waiting on something that never finished.",
		Cause:        "The watchdog detected the execution had stopped advancing its state checkpoint within the configured stuck threshold.",
		Suggestions: []string{
			"Check the executor's last container log line — usually shows what the agent was doing when it stalled.",
			"If the stall pattern is consistent, raise `watchdog.stuck_threshold` in config.yaml or look for a tool that hangs (network reads with no timeout are common).",
			"`vornikctl doctor` flags stuck executions; `--fix` cancels them so the lease can recycle.",
		},
	},
	persistence.TaskFailureClassLeaseExpired: {
		Class:        persistence.TaskFailureClassLeaseExpired,
		Scope:        ScopeTask,
		HumanMessage: "The service was restarted while your task was running. It will retry automatically — no action needed.",
		Cause:        "The scheduler's recovery loop found a leased task whose lease had expired without the executor finishing — usually a daemon restart or crash mid-execution.",
		Suggestions: []string{
			"This is normal recovery. The task should re-queue automatically; check that it's progressing now.",
			"If it keeps recurring on the same task, the underlying failure repeats deterministically — combine with `vornikctl task explain <id>` to diagnose.",
		},
	},
	persistence.TaskFailureClassRuntimeError: {
		Class:        persistence.TaskFailureClassRuntimeError,
		Scope:        ScopeTask,
		HumanMessage: "The service couldn't start your task's agent. Contact your administrator — this is a server-side issue, not something you can fix.",
		Cause:        "The container runtime (podman) failed to start the agent or returned a non-zero exit before the agent could write result.json.",
		Suggestions: []string{
			"Check `vornikctl doctor` — podman_config surfaces rootless / userns issues that produce this class.",
			"Confirm the agent image is pulled: `podman images | grep vornik-agent`.",
			"Check `journalctl --user -u vornik | grep podman` for the runtime error around the failure timestamp.",
		},
	},
	persistence.TaskFailureClassCancelled: {
		Class:        persistence.TaskFailureClassCancelled,
		Scope:        ScopeTask,
		HumanMessage: "This task was cancelled. Start a new one if you'd like to try again.",
		Cause:        "Operator-initiated stop or context cancellation. Not a fault.",
		Suggestions: []string{
			"No remediation needed. If the task was cancelled unintentionally, retry via `vornikctl task retry <id>` or schedule a fresh task.",
		},
	},
	persistence.TaskFailureClassOrphaned: {
		Class:        persistence.TaskFailureClassOrphaned,
		Scope:        ScopeTask,
		HumanMessage: "An internal cleanup process tidied up a stale record from this task. No action needed.",
		Cause:        "An execution row had no matching task — schema integrity issue.",
		Suggestions: []string{
			"`vornikctl doctor --fix` includes an orphan_fk_rows pass that cleans these up.",
		},
	},
	persistence.TaskFailureClassUnknown: {
		Class:        persistence.TaskFailureClassUnknown,
		Scope:        ScopeTask,
		HumanMessage: "Something went wrong, but the cause isn't a known pattern. Try again — if it keeps happening, share this task ID with your administrator.",
		Cause:        "The classifier didn't match the failure to any known pattern. Most often means a new failure mode.",
		Suggestions: []string{
			"Read last_error verbatim and check container logs.",
			"If the failure shape is recurring, add a pattern to internal/executor/failure_classifier.go and a corresponding playbook entry here.",
			"`vornikctl task explain <id>` produces an LLM summary that often spots the pattern faster than human pattern-matching.",
		},
	},
	persistence.TaskFailureClassSecretLeak: {
		Class:        persistence.TaskFailureClassSecretLeak,
		Scope:        ScopeTask,
		HumanMessage: "Your task's output contained something that looked like a secret (API key, token) and was blocked to keep it safe. Rephrase the request to avoid asking for credentials in the response.",
		Cause:        "Phase 2 secret-leak detector found a credential-shaped value in the task's result.json (or another Block-mode checkpoint) and refused to persist it. The task ran successfully on its own terms — it's the output that the secrets policy rejected.",
		Suggestions: []string{
			"Inspect the task's last_error — the message includes the count + types (`secret_leak: N finding(s)`). Common types: openai_key, anthropic_key, github_pat, jwt, generic_kv (envvar=value style).",
			"Most often the agent echoed an env var or a curl command into result.message. Tighten the role's prompt to say 'do not include API keys / Authorization headers in your output' and rerun via `vornikctl task retry`.",
			"If the value is a legitimate output that happens to look key-shaped (long base64 IDs, signed JWT delivery tokens), add it to the secrets allowlist in configs/secrets.yaml.",
			"Operator override: switch the result_json checkpoint to `redact` for this project — secrets get scrubbed but the task succeeds. Trade-off: no SECRET_LEAK class to investigate later.",
		},
		References: []string{
			"internal/secrets/secrets.go — pattern + allowlist corpus.",
			"BACKLOG: 'Secret leak detection + prevention' (Phase 1 shipped 2026-04-XX, Phase 2 in flight).",
		},
	},

	// ---- Task vocabulary: the four the hand-kept guard never named ----
	//
	// These shipped without a corpus entry because the old
	// TestPlaybookCoversAllFailureClasses listed its classes by hand and
	// stopped at 19 of 23. Three of the four are emitted in production today.

	persistence.TaskFailureClassChildFailed: {
		Class:        persistence.TaskFailureClassChildFailed,
		Scope:        ScopeTask,
		HumanMessage: "A sub-task of your request failed, so the parent could not finish.",
		Cause:        "A delegated child task reached a terminal failure and the parent was failed with it. The real cause is on the child, not here.",
		Suggestions: []string{
			"Find the child: `vornikctl task list --parent <id>`, then read ITS failure class — that is where the actionable cause is.",
			"`vornikctl task explain <child-id>` summarises the child's failure context.",
			"If several children failed the same way, treat it as a workflow or role problem rather than N task problems.",
		},
	},
	persistence.TaskFailureClassInvalidOutputLoop: {
		Class:        persistence.TaskFailureClassInvalidOutputLoop,
		Scope:        ScopeTask,
		HumanMessage: "The assistant kept returning output in the wrong format and the system stopped it.",
		Cause:        "The shape-retry watchdog hit its loop cap: the role produced schema-violating output repeatedly, and the corrective retry never converged. An escalation of INVALID_OUTPUT, not a one-off.",
		Suggestions: []string{
			"Read the LAST attempt's output — a loop usually repeats one misunderstanding, so one example is enough.",
			"Check the role's output schema is expressible by the model in use; a 27B model looping on a schema a larger model satisfies is a model-choice signal, not a prompt bug.",
			"Tighten the role prompt to state the required keys literally, then `vornikctl task retry`.",
		},
	},
	persistence.TaskFailureClassHallucinatedPlacement: {
		Class:        persistence.TaskFailureClassHallucinatedPlacement,
		Scope:        ScopeTask,
		HumanMessage: "The assistant reported doing something it did not actually do, so the result was rejected.",
		Cause:        "A phase-2 verifier found the agent claimed a placement (a trade, a write, a call) with no corroborating tool-audit record. The step is failed deliberately rather than trusting the claim.",
		Suggestions: []string{
			"Compare the claim against `tool_audit_log` for the task — the verifier's detail names what it could not corroborate.",
			"This is a model-honesty signal, not an infrastructure fault. Repeated hits on one role/model pair is a reason to change the pair.",
			"Do NOT relax the verifier to make the task pass; the whole value of the class is that it refused an unverifiable claim.",
		},
	},
	persistence.TaskFailureClassDelegationGuard: {
		Class:        persistence.TaskFailureClassDelegationGuard,
		Scope:        ScopeTask,
		HumanMessage: "The request tried to spawn more work than it is allowed to, and was stopped.",
		Cause:        "A delegation guard refused the task — depth, fan-out or cycle limits on delegated work. The guard raises a typed error so this class is authoritative, not text-matched.",
		Suggestions: []string{
			"Read last_error: the guard names which limit it enforced.",
			"A cycle usually means a workflow delegates to itself transitively — inspect the delegated_workflow chain.",
			"Raise the limit only after establishing the fan-out is intended; the guard exists to stop runaway delegation.",
		},
	},

	// ---- Step vocabulary (internal/stepoutcome) ----
	//
	// Absent from this corpus entirely until 2026-08-26, which is why
	// `playbook show container_non_zero_exit` answered "Unrecognised failure
	// class" for the fleet's largest failure bucket. See Finding B of
	// docs/audits/2026-08-26-silent-controls-audit.md.

	stepoutcome.ClassUnclassified: {
		Class:        stepoutcome.ClassUnclassified,
		Scope:        ScopeStep,
		HumanMessage: "Something went wrong in this step and the system could not identify what. The step's own error detail usually says — read that first.",
		Cause:        "NOT A DIAGNOSIS — the name is the point. The step failed and refineAgentFailureOutcome matched none of its known phrases, so no cause was determined. This class was called container_non_zero_exit until 2026-08-26, which named how the step ARRIVED here and read like a finding while carrying none.",
		Suggestions: []string{
			"Read error_detail VERBATIM — the agent usually named its own cause and it simply is not a phrase the refiner matches yet.",
			"`vornikctl task explain <id>` summarises the failure context.",
			"If the same wording recurs, add an arm to refineAgentFailureOutcome (internal/executor/container.go) and an entry here. That is how this bucket shrinks.",
		},
		References: []string{
			"https://docs.vornik.io — why this bucket is 52% of classified step failures and what is being done about it.",
		},
	},
	stepoutcome.ClassContainerFAILEDState: {
		Class:        stepoutcome.ClassContainerFAILEDState,
		Scope:        ScopeStep,
		HumanMessage: "The step reported itself as failed.",
		Cause:        "The agent wrote a FAILED status into result.json — a self-declared failure rather than a crash. The agent's own message is the cause.",
		Suggestions: []string{
			"error_detail carries the agent's stated reason; start there rather than in the container log.",
			"A self-declared failure is often a missing input or an impossible instruction, not a defect.",
		},
	},
	stepoutcome.ClassParseInvalidJSON: {
		Class:        stepoutcome.ClassParseInvalidJSON,
		Scope:        ScopeStep,
		HumanMessage: "The assistant's reply was not valid JSON, so it could not be read.",
		Cause:        "result.json did not parse. Usually prose wrapped around the JSON, a code fence, or a truncated write.",
		Suggestions: []string{
			"Check whether the output was TRUNCATED rather than malformed — a cut-off write and a bad format need different fixes.",
			"Tighten the role prompt to require a bare JSON object with no prose and no code fence.",
			"Persistent on one model is a model-choice signal.",
		},
	},
	stepoutcome.ClassParsePlanNoSteps: {
		Class:        stepoutcome.ClassParsePlanNoSteps,
		Scope:        ScopeStep,
		HumanMessage: "The planner produced an empty plan.",
		Cause:        "The plan parsed but contained zero steps, so there was nothing to execute.",
		Suggestions: []string{
			"Read the lead's output — an empty plan usually means the task description gave it nothing actionable.",
			"Check the role has the tools the plan would need; a lead with no capability to act sometimes plans nothing.",
		},
	},
	stepoutcome.ClassParsePlanRefused: {
		Class:        stepoutcome.ClassParsePlanRefused,
		Scope:        ScopeStep,
		HumanMessage: "The assistant declined to plan this request.",
		Cause:        "The planner returned a refusal rather than a plan. A decision by the model, not a parse fault.",
		Suggestions: []string{
			"Read the refusal text — it usually names what it objected to.",
			"A refusal is a signal about the request or the prompt, not an error to retry blindly.",
		},
	},
	stepoutcome.ClassGateInvalidJSON: {
		Class:        stepoutcome.ClassGateInvalidJSON,
		Scope:        ScopeStep,
		HumanMessage: "A quality check could not read the assistant's reply.",
		Cause:        "A workflow gate's own evaluation output failed to parse, so the gate could not return a verdict.",
		Suggestions: []string{
			"Distinguish this from gate_eval_failed: here the gate could not be READ, there it ran and rejected.",
			"Check the gate's model — gate evaluation is a structured-output task and small models fail it more often.",
		},
	},
	stepoutcome.ClassGateEvalFailed: {
		Class:        stepoutcome.ClassGateEvalFailed,
		Scope:        ScopeStep,
		HumanMessage: "A quality check rejected this step's work.",
		Cause:        "A workflow gate evaluated the step and returned a rejection. Often the SUBJECT behaving correctly rather than the instrument failing — a gate rejecting downstream work is the gate doing its job.",
		Suggestions: []string{
			"Read the gate's stated reason before treating this as a regression.",
			"On a benchmark arm, a review step reporting downstream_rejected across several days is a stable signal, not a new fault.",
		},
	},
	stepoutcome.ClassIterationCap: {
		Class:        stepoutcome.ClassIterationCap,
		Scope:        ScopeStep,
		HumanMessage: "The step took too many actions without finishing.",
		Cause:        "The agent burned its tool-iteration budget without producing a final answer. The step-level twin of TOOL_ITERATION_LIMIT.",
		Suggestions: []string{
			"Read the last few tool calls — a cap hit at the end of productive work needs a bigger budget; a cap hit on repeated identical calls is a degenerate loop wearing a different label.",
			"Narrow the step's scope before raising the cap.",
		},
	},
	stepoutcome.ClassDegenerateLoop: {
		Class:        stepoutcome.ClassDegenerateLoop,
		Scope:        ScopeStep,
		HumanMessage: "The assistant repeated the same action over and over and was stopped.",
		Cause:        "The audit scan spotted three or more consecutive identical tool calls. The agent was stuck, not working.",
		Suggestions: []string{
			"Read the repeated call — a loop on file_read of a path that does not exist means an upstream step did not produce what this one expects.",
			"Raising the wall clock or the iteration cap does NOT help; it buys the loop longer to spin.",
		},
	},
	stepoutcome.ClassVerifyFailed: {
		Class:        stepoutcome.ClassVerifyFailed,
		Scope:        ScopeStep,
		HumanMessage: "The assistant's claims did not match what it actually did.",
		Cause:        "A verifier compared the step's claims against the tool-audit record and found them unsupported.",
		Suggestions: []string{
			"The verifier's detail names the specific unsupported claim.",
			"Recurrent hits on one role/model pair is a model-honesty signal.",
		},
	},
	stepoutcome.ClassMissingOutput: {
		Class:        stepoutcome.ClassMissingOutput,
		Scope:        ScopeStep,
		HumanMessage: "The assistant said it produced a file that is not there.",
		Cause:        "The agent declared an outputArtifact in result.json but the named file is not on disk. Caught at the producer so the consumer does not loop on a file that will never appear.",
		Suggestions: []string{
			"Check whether the agent wrote to a different path than it declared — a relative/absolute mix-up is the common shape.",
			"This class exists to surface the real failure at the producer; do not chase the downstream consumer's file_read loop.",
		},
	},
	stepoutcome.ClassContextCancelled: {
		Class:        stepoutcome.ClassContextCancelled,
		Scope:        ScopeStep,
		HumanMessage: "This step was cancelled before it finished.",
		Cause:        "The step's context was cancelled — an operator stop, a superseding attempt, or a parent giving up.",
		Suggestions: []string{
			"Check whether a later attempt of the same step succeeded; a retry ladder cancels its own earlier attempts, so this is often bookkeeping rather than lost work.",
			"A high cancellation share on one workflow is worth separating from failures before reading it as a health signal.",
		},
	},
	stepoutcome.ClassContextTimeout: {
		Class:        stepoutcome.ClassContextTimeout,
		Scope:        ScopeStep,
		HumanMessage: "This step ran out of time.",
		Cause:        "The step's wall clock expired with no cause named by the agent. A genuine timeout — a named cause outranks the clock and would be recorded instead.",
		Suggestions: []string{
			"Raising the timeout is legitimate HERE, unlike for a degenerate loop or an iteration cap wearing a timeout.",
			"Check the model endpoint's latency before raising it; a slow self-hosted host is a different problem from a step that needs longer.",
		},
	},
	stepoutcome.ClassHallucinated: {
		Class:        stepoutcome.ClassHallucinated,
		Scope:        ScopeStep,
		HumanMessage: "The assistant stated something that could not be confirmed, so the step was rejected.",
		Cause:        "The claim-grounding detector found a High-severity unsupported claim — a URL never fetched, an ID that does not exist. The step is failed so the retry path picks it up.",
		Suggestions: []string{
			"The hallucination_signals column carries per-claim detail; read it rather than guessing which claim tripped it.",
			"url_not_fetched is the most common rule and is the one most prone to false positives — check the claim before treating it as model misbehaviour.",
		},
	},
	stepoutcome.ClassBudgetTripwire: {
		Class:        stepoutcome.ClassBudgetTripwire,
		Scope:        ScopeStep,
		HumanMessage: "The step stopped early to stay within your spending limit.",
		Cause:        "The agent self-aborted because the next LLM call would have breached the project's remaining budget for the active period. A deliberate stop, not a fault.",
		Suggestions: []string{
			"error_detail carries the estimated next-call cost and the remaining envelope.",
			"Raise the project's period budget, or wait for the period to roll over.",
		},
	},
	stepoutcome.ClassPromptTokenBudget: {
		Class:        stepoutcome.ClassPromptTokenBudget,
		Scope:        ScopeStep,
		HumanMessage: "The conversation grew too long for this step's configured limit.",
		Cause:        "The agent self-finalized because the next request would have exceeded the configured cumulative prompt-token ceiling for the step. A self-imposed ceiling, not the model's window.",
		Suggestions: []string{
			"Distinct from context_overflow, which is the MODEL's window. Check which ceiling you actually want to move.",
			"A step repeatedly hitting this is usually accumulating context it does not need.",
		},
	},
	stepoutcome.ClassContextOverflow: {
		Class:        stepoutcome.ClassContextOverflow,
		Scope:        ScopeStep,
		HumanMessage: "The conversation grew too large for the model to handle.",
		Cause:        "The agent could not fit the conversation into the model's context window after exhausting its in-container overflow rescues.",
		Suggestions: []string{
			"Distinct from prompt_token_budget (a configured ceiling) and context_timeout (wall clock).",
			"A model with a larger window, or a narrower step, are the two real fixes; more wall clock is not one.",
		},
	},
	stepoutcome.ClassPlausibilityViolation: {
		Class:        stepoutcome.ClassPlausibilityViolation,
		Scope:        ScopeStep,
		HumanMessage: "The assistant's answer was well-formed but did not make sense.",
		Cause:        "A gate-mode plausibility rule fired on an otherwise schema-valid result.json. The SHAPE was right and the CONTENT was not — which is why it is kept apart from the schema classes.",
		Suggestions: []string{
			"The fix differs from a schema failure: tightening the schema will not help, because the schema was satisfied.",
			"Read which plausibility rule fired; it names the implausible content.",
		},
	},
	stepoutcome.ClassEmptyDelegation: {
		Class:        stepoutcome.ClassEmptyDelegation,
		Scope:        ScopeStep,
		HumanMessage: "The step was supposed to create sub-tasks and created none.",
		Cause:        "A step pinning delegated_workflow finished a fresh pass with zero delegatedTasks, so nothing was scheduled. Guarded deliberately — without it the step advances silently and a downstream consumer fails on an empty diff.",
		Suggestions: []string{
			"Read the step's output: it usually decided there was nothing to delegate, which may be correct.",
			"If delegation was expected, check the step's inputs actually contained the work to fan out.",
		},
	},

	// ---- wave-2 step classes: what the residual bucket was made of ----

	stepoutcome.ClassLLMCallFailed: {
		Class:        stepoutcome.ClassLLMCallFailed,
		Scope:        ScopeStep,
		HumanMessage: "The AI model provider could not be reached or returned an error. This is usually temporary — try again.",
		Cause:        "The agent's call to the model provider failed: transport (curl/connect), a gateway error, or an upstream provider error. The step never got a usable answer, so it failed. NOT a fault in the agent's reasoning.",
		Suggestions: []string{
			"Read error_detail: it carries the provider and the status, which decides everything else. A 400 is a request the provider rejected; a connect failure is reachability; a 5xx is theirs.",
			"Check whether the model is a FALLBACK rung. A rung that fails in under a second failed at container startup, not at inference, and has probably never worked — see `container_start_failed`.",
			"`vornikctl doctor` covers endpoint reachability; agenthealth is the per-model circuit breaker and will have opened if one model is failing consistently.",
			"Concentrated on one provider over a window, this is an incident to escalate rather than a task to retry.",
		},
		References: []string{
			"This was 45% of the unclassified bucket before it had a class of its own — the fleet's single most common failure. Treat a spike as a fleet signal, not a task problem.",
		},
	},
	stepoutcome.ClassMissingPrerequisite: {
		Class:        stepoutcome.ClassMissingPrerequisite,
		Scope:        ScopeStep,
		HumanMessage: "This step needed something an earlier step was supposed to produce, and it was not there.",
		Cause:        "The agent stopped because an input it required was absent — typically a file an upstream step should have written. The failure is real but its CAUSE is upstream.",
		Suggestions: []string{
			"Look at the step that was supposed to produce it, not at this one. This class names the consumer; the defect is at the producer.",
			"Check for a `missing_declared_output` on an earlier step — that is the same failure caught one step sooner, where it is actionable.",
			"A relative-vs-absolute path mismatch between producer and consumer is the common shape when the producer did in fact write something.",
		},
	},
	stepoutcome.ClassContainerKilled: {
		Class:        stepoutcome.ClassContainerKilled,
		Scope:        ScopeStep,
		HumanMessage: "The step was stopped by the system before it could finish, most often for using too much memory.",
		Cause:        "The container was terminated by a signal. Overwhelmingly the OOM killer; the exit code on the row distinguishes it (137 = SIGKILL).",
		Suggestions: []string{
			"Check container_exit_code on the row. 137 is SIGKILL, which for an agent container almost always means the memory cgroup limit.",
			"Raise the agent container's memory limit, or narrow the step so it holds less at once. More wall clock does not help a kill.",
			"If it correlates with large tool outputs, the step is accumulating context it does not need.",
		},
	},
	stepoutcome.ClassContainerWaitFailed: {
		Class:        stepoutcome.ClassContainerWaitFailed,
		Scope:        ScopeStep,
		HumanMessage: "The system lost track of this step while it was running.",
		Cause:        "The runtime failed while waiting for the container to finish. An infrastructure fault between daemon and container runtime, not an agent fault — the agent may well have done its work.",
		Suggestions: []string{
			"Check the container runtime's own health (`podman ps`, the runtime's journal) around the step's recorded_at.",
			"Distinct from container_killed, which is a deliberate signal, and from container_start_failed, which never ran at all.",
			"Recurring on one host is a host problem; scattered across hosts, suspect the runtime version or resource pressure.",
		},
	},
	stepoutcome.ClassContainerStartFailed: {
		Class:        stepoutcome.ClassContainerStartFailed,
		Scope:        ScopeStep,
		HumanMessage: "The step could not be started at all.",
		Cause:        "The container never started: a missing or unbuilt image, a bad mount, or runtime configuration. Nothing the agent did or could do — it never ran.",
		Suggestions: []string{
			"Sub-second and deterministic is the signature. Reproduce by hand: run the agent image with that role's env and read stderr.",
			"Check image freshness first — `vornikctl doctor` has an image_freshness check, and an agent image that was never rebuilt is the most common cause.",
			"A FALLBACK rung failing this way has never once worked, and the ladder walks silently past it. A rung that fails 4-of-4 in under a second is not a fallback; treat it as absent.",
		},
	},
}
