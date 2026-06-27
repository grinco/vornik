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
)

// Entry is the per-class remediation record.
type Entry struct {
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
		HumanMessage: "This project's workflow configuration has an error. Contact your administrator.",
		Cause:        "The workflow YAML referenced a step ID that doesn't exist (typo in on_success / on_fail / gate target) or the workflow file was malformed.",
		Suggestions: []string{
			"Run `vornikctl doctor` — config_validation surfaces YAML syntax errors and unresolved step references.",
			"Open the workflow YAML and grep for the unresolved step ID; it's almost always a typo in on_success / on_fail / gates[].target.",
		},
	},
	persistence.TaskFailureClassWorkflowDrift: {
		Class:        persistence.TaskFailureClassWorkflowDrift,
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
		HumanMessage: "The service was restarted while your task was running. It will retry automatically — no action needed.",
		Cause:        "The scheduler's recovery loop found a leased task whose lease had expired without the executor finishing — usually a daemon restart or crash mid-execution.",
		Suggestions: []string{
			"This is normal recovery. The task should re-queue automatically; check that it's progressing now.",
			"If it keeps recurring on the same task, the underlying failure repeats deterministically — combine with `vornikctl task explain <id>` to diagnose.",
		},
	},
	persistence.TaskFailureClassRuntimeError: {
		Class:        persistence.TaskFailureClassRuntimeError,
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
		HumanMessage: "This task was cancelled. Start a new one if you'd like to try again.",
		Cause:        "Operator-initiated stop or context cancellation. Not a fault.",
		Suggestions: []string{
			"No remediation needed. If the task was cancelled unintentionally, retry via `vornikctl task retry <id>` or schedule a fresh task.",
		},
	},
	persistence.TaskFailureClassOrphaned: {
		Class:        persistence.TaskFailureClassOrphaned,
		HumanMessage: "An internal cleanup process tidied up a stale record from this task. No action needed.",
		Cause:        "An execution row had no matching task — schema integrity issue.",
		Suggestions: []string{
			"`vornikctl doctor --fix` includes an orphan_fk_rows pass that cleans these up.",
		},
	},
	persistence.TaskFailureClassUnknown: {
		Class:        persistence.TaskFailureClassUnknown,
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
}
