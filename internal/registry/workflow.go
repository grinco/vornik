// Package registry provides in-memory registries for projects, swarms, and workflows.
package registry

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// jsonCanonical marshals v through encoding/json, which sorts map keys
// alphabetically. That makes the output deterministic and therefore
// hash-stable across processes and Go rebuilds. Pulled out so the
// Hash() behaviour is obvious at a glance.
func jsonCanonical(v any) ([]byte, error) {
	return json.Marshal(v)
}

// Hash returns a stable SHA-256-prefix hash of this workflow's content.
// Used by the executor to pin an execution to the workflow revision
// that launched it (see ExecutionState). On resume, a mismatch means
// the operator edited the YAML mid-execution and the snapshot no
// longer matches the live definition — we fail WORKFLOW_DRIFT rather
// than silently running a hybrid of old state and new structure.
//
// Relies on encoding/json sorting map keys alphabetically (stable
// since Go 1.12). The prefix is short enough to be readable in logs
// and the DB column but long enough to be injection-resistant.
func (w *Workflow) Hash() string {
	if w == nil {
		return ""
	}
	buf, err := jsonCanonical(w)
	if err != nil {
		// Marshalling a struct with plain fields and string-keyed maps
		// shouldn't fail. If it does, fall back to a deterministic-ish
		// fingerprint so revision pinning still catches trivial edits.
		return w.ID + ":" + w.Version
	}
	sum := sha256.Sum256(buf)
	return fmt.Sprintf("%x", sum[:8])
}

// WorkflowDescriptionMaxLen is the validator cap on the workflow
// description field. Long descriptions belong in the body of the
// WORKFLOW.md file or in a dedicated docs/ page; the YAML field
// is for a one-paragraph summary surfaced in dashboards, doctor
// reports, and the workflow picker. The cap keeps the table view
// readable and bounds the hash payload — anything longer is a
// strong signal the operator wants prose, which belongs in the
// Markdown body, not the frontmatter.
const WorkflowDescriptionMaxLen = 1024

// Workflow represents a workflow definition loaded from workflows/*.yaml
type Workflow struct {
	// ID is the unique identifier for the workflow (required)
	ID string `yaml:"workflowId"`
	// DisplayName is a human-readable name for the workflow
	DisplayName string `yaml:"displayName"`
	// Description is a short, free-form summary of the workflow's
	// intent. Required by the workflow_md_shape doctor check so
	// every shipped workflow carries enough context for the
	// dashboard / picker / doctor report to render meaningfully
	// without forcing the operator to crack open the Markdown
	// body. Hard-capped at WorkflowDescriptionMaxLen characters
	// — see Validate for the rationale.
	Description string `yaml:"description"`
	// Version is the workflow version (for tracking)
	Version string `yaml:"version"`
	// Entrypoint is the first step to execute (required)
	Entrypoint string `yaml:"entrypoint"`
	// Steps defines all workflow steps
	Steps map[string]WorkflowStep `yaml:"steps"`
	// Terminals define end states for the workflow
	Terminals map[string]WorkflowTerminal `yaml:"terminals"`
	// ResumeAfterChildren opts a custom workflow into the strict-adaptive
	// resume guard: when a step delegates child task(s) (selected_workflow)
	// and the parent pauses on WAITING_FOR_CHILDREN, the resumed execution
	// detects the existing children and advances to the step's OnSuccess
	// (e.g. a publish step) instead of re-running the delegate. The built-in
	// `adaptive` workflow gets this implicitly; custom workflows must opt in.
	ResumeAfterChildren bool `yaml:"resume_after_children"`
	// MaxStepVisits limits how many times a single step can be visited
	// before the workflow fails. Prevents infinite rework loops. Default 3.
	MaxStepVisits int `yaml:"maxStepVisits"`
	// MaxIterations limits the total number of step transitions in the
	// workflow loop before the execution is terminated. This acts as a
	// global circuit breaker complementing the per-step MaxStepVisits.
	// Default 20.
	MaxIterations int `yaml:"maxIterations"`
	// MaxWallClock is the hard ceiling on a single execution's
	// wall-clock duration. The executor cancels the run when this
	// elapses regardless of what the agents are doing — protects
	// against agents that pass per-step timeouts but loop slowly
	// just under the watchdog's no-progress threshold for hours.
	// Go duration string (e.g. "30m", "1h"). Empty = no cap (the
	// pre-feature behaviour). The cap should be generous enough not
	// to kill legitimate long autonomous runs (researcher scrapes,
	// scout walks); 1h is a sensible global default for code/dev
	// workflows, longer for research.
	MaxWallClock string `yaml:"maxWallClock"`
	// CleanupArtifacts lists workspace-relative paths that the
	// executor MUST delete at workflow start, before the entrypoint
	// step runs. Use this for canonical artifacts the workflow's
	// agents are supposed to OVERWRITE — when an early-failing step
	// fails to do so, a downstream step reads the prior task's stale
	// content. The defense-in-depth pre-clean closes that gap.
	//
	// Authoritative on disk; round-trips through the workflow
	// editor verbatim. The 2026-05-18 incident traced silent loss
	// of this field to the editor stripping unknown keys on save —
	// every form-driven edit zeroed it. The editor now surfaces a
	// textarea fed from this field.
	//
	// Each entry is treated as a path relative to the project
	// workspace root (effective worktree dir when worktrees are
	// active). Missing files are silently OK; per-file delete errors
	// are logged but do not fail the workflow. Paths must stay
	// inside the workspace — absolute paths and `..` traversal are
	// rejected by the cleanup helper. Glob patterns are NOT
	// expanded; list each file explicitly.
	//
	// Example:
	//   cleanup_artifacts:
	//     - artifacts/out/research.md
	//     - artifacts/out/deliverable.md
	//     - artifacts/out/summary.txt
	CleanupArtifacts []string `yaml:"cleanup_artifacts,omitempty"`
	// Pedantic, when set true, disables the swarm-recovery flow for
	// every task running this workflow: on_fail routing falls
	// straight through to the configured terminal failure target
	// instead of surfacing a `decision` checkpoint with proposed
	// alternatives. Wins over the project-level pedantic flag (the
	// narrower scope), but is itself overridden by the task-level
	// pedantic flag in the task payload. Pointer so an absent field
	// reads as nil (defer to project / task scope). See
	// https://docs.vornik.io §6.
	Pedantic *bool `yaml:"pedantic,omitempty"`
	// A2A controls the agent-to-agent boundary exposure for this
	// workflow. Default-off: an existing workflow doesn't
	// accidentally become a public A2A agent on a daemon upgrade.
	// See https://docs.vornik.io
	A2A WorkflowA2A `yaml:"a2a,omitempty"`
	// RequireInputArtifacts, when true, makes inputArtifacts
	// mandatory for companion delegate() calls targeting this
	// workflow. The delegate handler rejects artifact-less
	// delegations up front — a workflow that reads exclusively
	// from context.inputArtifacts otherwise no-ops silently
	// (2026-06-05 rag-ingest incident).
	RequireInputArtifacts bool `yaml:"require_input_artifacts,omitempty"`
	// IngestInputArtifacts, when true, makes the executor ingest the
	// task's staged input artifacts DIRECTLY into project RAG memory
	// after the workflow completes — no agent in the copy loop. This
	// is the deterministic bulk-ingest path: a weak rag-ingester model
	// used to claim it had copied files it never wrote, failing the
	// run (the 2026-06 ingest incidents). With this flag the workflow
	// needs no agent step at all (entrypoint can be a terminal); the
	// `handleSuccess` ingest hook enqueues each input artifact by ID,
	// preserving repo_scope and the full ingest pipeline. Gate is
	// essential: without it, every task with uploaded attachments
	// (Telegram/email/research inputs) would be dumped into RAG.
	IngestInputArtifacts bool `yaml:"ingest_input_artifacts,omitempty"`
}

// WorkflowA2A is the per-workflow A2A protocol surface
// configuration. Operator-opt-in; default zero (no publish).
type WorkflowA2A struct {
	// Publish, when true, makes this workflow discoverable via
	// the daemon's agent card index and reachable via POST
	// /a2a/v1/agents/<project>/<workflow>/tasks. The workflow's
	// project still gates access via the existing API-key auth.
	Publish bool `yaml:"publish,omitempty"`
}

// WorkflowStep represents a single step in a workflow
type WorkflowStep struct {
	// Type of step: "agent", "gate", "approval", "plan", "call_project" (required)
	Type string `yaml:"type"`
	// Role specifies which swarm role performs this step (for agent type)
	Role string `yaml:"role"`
	// Prompt is the instruction given to the agent
	Prompt string `yaml:"prompt"`
	// OnSuccess is the next step to transition to on success
	OnSuccess string `yaml:"on_success"`
	// OnFail is the step to transition to when the agent fails.
	// If empty, a failure causes the entire execution to fail.
	OnFail string `yaml:"on_fail"`
	// Gates define conditional transitions
	Gates []WorkflowGate `yaml:"gates"`
	// Timeout for this step (e.g., "30m")
	Timeout string `yaml:"timeout"`
	// RetryPolicy defines retry behavior
	RetryPolicy WorkflowRetryPolicy `yaml:"retryPolicy"`

	// Handler is the SystemHandler name for `system`-typed steps.
	// Looked up at dispatch in the executor's handler registry
	// (e.g. "rag.extract", "rag.index"). Required when Type ==
	// "system"; ignored for other types. B-7.
	Handler string `yaml:"handler,omitempty"`

	// GatingReviews, on a `forge.post_review` step, opts the change-request
	// review into a REAL forge review state: the reviewer's verdict is posted
	// as an APPROVE / REQUEST_CHANGES review (which can satisfy branch
	// protection and trigger auto-merge) instead of a plain non-gating comment.
	// The verdict comes from the reviewer's explicit `event` field if present,
	// else its structured `review.approved` bool (true → APPROVE, false →
	// REQUEST_CHANGES). Default false keeps the safe legacy behavior — the
	// review prose (incl. the "✅ Approved" header) is posted as a COMMENT and
	// never gates the PR. Ignored for non-`forge.post_review` steps.
	GatingReviews bool `yaml:"gating_reviews,omitempty"`

	// DelegatedWorkflow pins the workflow that `delegatedTasks` emitted by THIS
	// step run under, when a delegation spec doesn't set its own. It makes
	// subtask routing deterministic instead of trusting the LLM to emit the
	// per-task `workflow` field (incident 2026-06-13: issue-fix's decompose lead
	// omitted it, so subtasks fell back to the project default `dev-pipeline`).
	// Empty = the spec's own workflow, else the project default.
	DelegatedWorkflow string `yaml:"delegated_workflow,omitempty"`

	// RequireOutputGlob declares a file-output contract for an agent step:
	// after a successful run, at least one file matching this glob must
	// have been written during the step, or the step fails with a
	// "schema violation:" error (which the shape-retry layer corrects
	// once before giving up). Globs starting with "project/" resolve
	// against the task's worktree / persistent project dir; anything else
	// resolves against the step's ephemeral staging workspace. Incident
	// task_20260712143854_429a3500d692d23c: 7 of 8 deep-research subtasks
	// COMPLETED without writing their promised findings file, and the
	// chain "succeeded" all the way to a publisher holding no deliverable.
	RequireOutputGlob string `yaml:"require_output_glob,omitempty"`

	// StageChildArtifacts, when true on the step that runs AFTER a
	// `delegatedTasks` fan-out (a resume_after_children consumer step),
	// makes the executor deterministically gather this step's delegated
	// children's output artifacts from the durable store and stage them
	// into the step's `artifacts/in/` on resume — plus inject an
	// `inputArtifactsSummary` (expected/staged/missing/empty) into the
	// step's agent input context. The gate is a GRAPH property keyed on the
	// declaring step: staging fires ONLY for that consumer step, ONLY on a
	// resume-after-children re-entry, and ONLY when ≥1 delegation-engine
	// child exists — NEVER on the initial decompose pass (no children yet)
	// and NEVER on a non-declaring step. Default false → behaviour is
	// byte-identical to today (I2). Workflow-agnostic: deep-research is the
	// first adopter, but any aggregating workflow opts in with this one flag.
	// See https://docs.vornik.io §3.1–§3.5.
	StageChildArtifacts bool `yaml:"stage_child_artifacts,omitempty"`

	// MaxVisits optionally bounds how many times THIS step may be entered,
	// tighter than the workflow-global MaxStepVisits. On the (MaxVisits+1)-th
	// entry the executor routes to the step's on_fail (preserving the prior
	// step's result so the terminal carries it), instead of the global cap's
	// hard error. 0 / unset = no per-step cap (global cap applies). Used to
	// bound rework loopbacks — e.g. issue-fix's `remediate` caps the
	// review→remediate loop at 2 rounds (design
	// https://docs.vornik.io).
	MaxVisits int `yaml:"maxVisits,omitempty"`

	// --- call_project step fields (Phase A of inter-project
	// orchestration; LLD https://docs.vornik.io
	// orchestration-design.md §6.1) ---

	// TargetProject is the callee project's ID. Required when
	// Type == "call_project".
	TargetProject string `yaml:"target_project,omitempty"`
	// TargetWorkflow is the workflow ID the callee project
	// should run for this call. Required when Type ==
	// "call_project".
	TargetWorkflow string `yaml:"target_workflow,omitempty"`
	// Payload is the typed input passed to the callee task.
	// Keys are arbitrary strings (matched to the callee
	// workflow's expected inputs); values can contain
	// ${outputs.<step>.<field>} references that the executor
	// interpolates at step entry.
	Payload map[string]any `yaml:"payload,omitempty"`
	// Expect declares the result envelope schema the caller
	// requires. Required when Type == "call_project"; the
	// schema name must exist in the schema_registry.
	Expect WorkflowCallExpect `yaml:"expect,omitempty"`

	// CancelOnTimeout, when true, instructs the timeout
	// scanner to cascade-cancel the callee task in addition
	// to flipping the CPC to status=timed_out. Default false
	// (LLD §8.1 — preserves the "callee work may still be
	// useful" default). Set to true for expensive long-running
	// workflows where the operator would rather kill the
	// callee than have it consume more budget after the
	// caller has moved on.
	CancelOnTimeout bool `yaml:"cancel_on_timeout,omitempty"`

	// --- spawn_project step fields (Phase B; LLD §6.2) ---

	// Template is the project-template slug to materialise from.
	// Required when Type == "spawn_project". Must be in the
	// project-templates catalog AND the spawning project's
	// AllowSpawn.Templates allowlist.
	Template string `yaml:"template,omitempty"`
	// Params are the template parameter values used at render
	// time. The catalog validator rejects unknown keys, missing
	// required fields, regex / enum violations, etc. The "name"
	// param is conventionally the spawned project's slug — the
	// LLD recommends workflows interpolate a uniqueness suffix
	// (date / ulid) to avoid PROJECT_EXISTS collisions.
	Params map[string]any `yaml:"params,omitempty"`
	// InitialTask, when set, drops a seed task into the spawned
	// project's queue. Optional; spawning a project without an
	// initial_task is valid (the spawned project's autonomy
	// loop or a later call_project will drive work into it).
	InitialTask *WorkflowInitialTask `yaml:"initial_task,omitempty"`

	// --- a2a_call step fields (A2A Phase B; LLD docs/low-level-
	// design/a2a-protocol-design.md "Outbound A2A client") ---

	// AgentURL is the partner agent's endpoint, typically
	// `<host>/a2a/v1/agents/<project>/<workflow>`. Required when
	// Type == "a2a_call". The step POSTs `/tasks` to this URL
	// and consumes the resulting SSE stream.
	AgentURL string `yaml:"agent_url,omitempty"`
	// APIKeyEnv names the environment variable carrying the
	// X-API-Key header for outbound calls. Empty → no auth
	// header (use only against open / public endpoints).
	// Reading the value at step time keeps the workflow file
	// free of secrets that could leak through `vornikctl
	// workflow show` or a `git diff`.
	APIKeyEnv string `yaml:"api_key_env,omitempty"`

	// --- parallel step fields (declarative intra-workflow fan-out /
	// fan-in; LLD https://docs.vornik.io
	// fanout-design.md §4.1) ---

	// Branches declares the static legs of a `parallel` fan-out step.
	// Each branch becomes one PARALLEL delegated child task at runtime
	// (via the existing delegation engine), so parallelism lives at the
	// task level and the executor's single-threaded invariants are
	// untouched. Required (≥1) when Type == "parallel"; ignored
	// otherwise. See §4.2.
	Branches []WorkflowBranch `yaml:"branches,omitempty"`
	// Join is the non-`parallel` consumer step the parent resumes at once
	// the fan-out legs terminate (subject to JoinPolicy). Required when
	// Type == "parallel"; it is the parallel step's ONLY forward edge —
	// a parallel step may not set on_success/on_fail/gates (§4.3/§5).
	Join string `yaml:"join,omitempty"`
	// JoinPolicy governs when the parent proceeds to Join after all legs
	// are terminal: `all` (default / empty), `best_effort` (≥1 leg
	// succeeded), or `quorum:<n>` (≥n legs succeeded, 1≤n≤len(branches)).
	// v1 evaluates only AFTER all legs are terminal — no early
	// short-circuit or cancellation. See §4.3.
	JoinPolicy string `yaml:"join_policy,omitempty"`
}

// WorkflowBranch is one statically-declared leg of a `parallel` fan-out
// step. It mirrors the runtime delegatedTaskSpec (inline role+prompt, with
// an optional sub-workflow) so an author who knows the legs up front can
// declare them without routing through an LLM `decompose` step. See
// https://docs.vornik.io §4.1.
type WorkflowBranch struct {
	// ID is a workflow-unique, non-empty identifier for the leg. Surfaced
	// in the fan-in / observability path so a missing leg can be named.
	ID string `yaml:"id"`
	// Role is the swarm role that runs the leg (required).
	Role string `yaml:"role"`
	// Prompt is the instruction handed to the leg's agent (required).
	Prompt string `yaml:"prompt"`
	// Workflow optionally pins the sub-workflow the leg runs under. Empty
	// = a default single-agent leg. Must resolve to a known workflow id
	// (validated at config-set load, cross-workflow).
	Workflow string `yaml:"workflow,omitempty"`
}

// HasExternalSideEffects reports whether running this step mutates state
// OUTSIDE the execution's own workspace/state machine — effects that a
// retry-from-step will NOT replay and cannot roll back. Used by the
// retry-from-step containment guard to warn an operator that preserved
// upstream steps already produced effects (a posted forge review, an
// indexed RAG batch, a spawned callee task) that the re-run treats as
// done.
//
//   - "system" steps invoke a SystemHandler (forge.post_review, rag.index,
//     rag.extract, …) that writes to an external system.
//   - "call_project" steps spawn/await a task in another project.
//
// "agent" steps CAN call mutating tools, but the workflow has no per-step
// idempotency declaration to key on, so they are conservatively NOT flagged
// here (flagging every agent step would make the warning noise). "gate",
// "approval", and "plan" steps are pure control flow.
func (s WorkflowStep) HasExternalSideEffects() bool {
	switch s.Type {
	case "system", "call_project":
		return true
	default:
		return false
	}
}

// WorkflowCallExpect carries the call_project step's
// schema-validation contract. The runtime resolves Schema
// against the schema_registry and validates the callee's
// result envelope before resolving the CPC; a mismatch
// resolves the CPC as rejected and fires the caller's
// on_fail branch.
type WorkflowCallExpect struct {
	Schema string `yaml:"schema,omitempty"`
}

// --- spawn_project step fields (Phase B of inter-project
// orchestration; LLD §6.2) ---
//
// The fields live on WorkflowStep below alongside the
// call_project fields so existing YAML shape stays uniform.
// They're all omitempty so a workflow that doesn't use
// spawn_project parses unchanged.

// WorkflowInitialTask describes the optional seed task created
// in a newly-spawned project. Lets the spawning workflow drop
// the spawned project straight into a useful first action
// (e.g. "run the kickoff workflow with this brief") rather
// than leaving it idle until the operator manually creates
// the first task.
//
// Workflow is the workflow ID inside the spawned project the
// initial task should run; defaults to the spawned project's
// defaultWorkflowId when empty. Payload is the JSON body the
// task starts with — interpolated against the spawning step's
// params + outputs (Phase B v1 = pass-through; Phase C will
// add ${outputs.x.y} resolution).
type WorkflowInitialTask struct {
	Workflow string         `yaml:"workflow,omitempty"`
	Payload  map[string]any `yaml:"payload,omitempty"`
}

// WorkflowGate defines a conditional transition
type WorkflowGate struct {
	// Condition is the expression to evaluate (e.g., "review.approved == true")
	Condition string `yaml:"condition"`
	// Target is the step or terminal to transition to if condition is true
	Target string `yaml:"target"`
}

// WorkflowRetryPolicy defines retry behavior for a step
type WorkflowRetryPolicy struct {
	// MaxRetries is the maximum number of retry attempts
	MaxRetries int `yaml:"maxRetries"`
	// Backoff is the delay between retries (e.g., "1m", "exponential")
	Backoff string `yaml:"backoff"`
}

// WorkflowTerminal defines an end state for the workflow
type WorkflowTerminal struct {
	// Status is the final status: "COMPLETED", "FAILED", "CANCELLED"
	Status string `yaml:"status"`
	// Message is an optional message for the terminal state
	Message string `yaml:"message"`
	// Recovery marks a COMPLETED terminal that is an INTENTIONAL
	// graceful-recovery exit reached via an on_fail route (e.g.
	// dev-pipeline's `checkpoint`: a hard step failure routes to a
	// recovery step that parks a partial result and exits here so the
	// next autonomy tick resumes from the next subtask instead of
	// dead-ending). The workflow_onfail_masking doctor check skips such
	// terminals — reaching them via on_fail is by design, not failures
	// silently masquerading as success.
	Recovery bool `yaml:"recovery"`
}

// WorkflowValidationError represents a validation error for a workflow
type WorkflowValidationError struct {
	File    string
	Field   string
	Message string
}

func (e WorkflowValidationError) Error() string {
	return fmt.Sprintf("workflow validation error in %s: %s - %s", e.File, e.Field, e.Message)
}

// Validate validates a Workflow struct
func (w *Workflow) Validate(filename string) error {
	if w.ID == "" {
		return WorkflowValidationError{File: filename, Field: "workflowId", Message: "workflowId is required"}
	}
	if w.Entrypoint == "" {
		return WorkflowValidationError{File: filename, Field: "entrypoint", Message: "entrypoint is required"}
	}
	// Description is optional for backward compatibility — the
	// workflow_md_shape doctor check flags a missing description
	// without making the workflow unloadable. The length cap is
	// enforced here so a runaway paste (e.g. an operator dumping
	// a design doc into the YAML field) fails fast at load time.
	if len(w.Description) > WorkflowDescriptionMaxLen {
		return WorkflowValidationError{
			File:    filename,
			Field:   "description",
			Message: fmt.Sprintf("description must be ≤%d characters (got %d)", WorkflowDescriptionMaxLen, len(w.Description)),
		}
	}
	// A workflow normally needs at least one agent step. The exception
	// is a deterministic ingest workflow (ingest_input_artifacts): the
	// executor deposits the staged input artifacts directly in
	// handleSuccess, so the workflow body is just an entrypoint that
	// routes straight to a terminal — no agent step required.
	if len(w.Steps) == 0 && !w.IngestInputArtifacts {
		return WorkflowValidationError{File: filename, Field: "steps", Message: "at least one step is required"}
	}

	// Validate entrypoint exists
	if _, exists := w.Steps[w.Entrypoint]; !exists {
		// Check if entrypoint is a terminal
		if _, isTerminal := w.Terminals[w.Entrypoint]; !isTerminal {
			return WorkflowValidationError{
				File:    filename,
				Field:   "entrypoint",
				Message: fmt.Sprintf("entrypoint '%s' not found in steps or terminals", w.Entrypoint),
			}
		}
	}

	// Validate each step
	for stepID, step := range w.Steps {
		if step.Type == "" {
			return WorkflowValidationError{
				File:    filename,
				Field:   fmt.Sprintf("steps.%s.type", stepID),
				Message: "step type is required",
			}
		}

		// Validate step type. Two drift catches landed 2026-05-28:
		// `a2a_call` shipped on 2026-05-25 (A2A Phase B) but was
		// never added to the allowlist; `system` is the new B-7
		// type powering the document-ingest workflow.
		validTypes := map[string]bool{
			"agent":         true,
			"gate":          true,
			"approval":      true,
			"plan":          true,
			"call_project":  true,
			"spawn_project": true,
			"a2a_call":      true,
			"system":        true,
			"parallel":      true,
		}
		if !validTypes[step.Type] {
			return WorkflowValidationError{
				File:    filename,
				Field:   fmt.Sprintf("steps.%s.type", stepID),
				Message: fmt.Sprintf("invalid step type '%s', must be one of: agent, gate, approval, plan, call_project, spawn_project, a2a_call, system, parallel", step.Type),
			}
		}

		// parallel steps carry their own structural contract (branches,
		// join, join_policy) and forbid the ordinary success/fail edges —
		// their only forward edge is `join`. Validate the whole shape in
		// one place; the transition/gate checks below are skipped for a
		// well-formed parallel step because it sets none of those fields.
		if step.Type == "parallel" {
			if err := w.validateParallelStep(filename, stepID, step); err != nil {
				return err
			}
		}

		// Agent and plan steps require a role
		if (step.Type == "agent" || step.Type == "plan") && step.Role == "" {
			return WorkflowValidationError{
				File:    filename,
				Field:   fmt.Sprintf("steps.%s.role", stepID),
				Message: "role is required for agent and plan steps",
			}
		}

		// system steps require a handler name (the executor's
		// SystemHandlerRegistry resolves this at dispatch). Without
		// the field set the workflow is unrunnable; catch it here.
		if step.Type == "system" && step.Handler == "" {
			return WorkflowValidationError{
				File:    filename,
				Field:   fmt.Sprintf("steps.%s.handler", stepID),
				Message: "handler is required for system steps (e.g. 'rag.extract' / 'rag.index')",
			}
		}

		// spawn_project steps require template. Params is optional
		// (some templates have no parameters); initial_task is
		// optional (spawned project can be created idle).
		if step.Type == "spawn_project" {
			if step.Template == "" {
				return WorkflowValidationError{
					File:    filename,
					Field:   fmt.Sprintf("steps.%s.template", stepID),
					Message: "template is required for spawn_project steps",
				}
			}
		}

		// call_project steps require target_project, target_workflow,
		// and expect.schema. Payload + on_fail are optional but
		// strongly recommended (LLD §6.1).
		if step.Type == "call_project" {
			if step.TargetProject == "" {
				return WorkflowValidationError{
					File:    filename,
					Field:   fmt.Sprintf("steps.%s.target_project", stepID),
					Message: "target_project is required for call_project steps",
				}
			}
			if step.TargetWorkflow == "" {
				return WorkflowValidationError{
					File:    filename,
					Field:   fmt.Sprintf("steps.%s.target_workflow", stepID),
					Message: "target_workflow is required for call_project steps",
				}
			}
			if step.Expect.Schema == "" {
				return WorkflowValidationError{
					File:    filename,
					Field:   fmt.Sprintf("steps.%s.expect.schema", stepID),
					Message: "expect.schema is required for call_project steps (the result envelope JSON-Schema id)",
				}
			}
		}

		// Plan steps require on_success
		if step.Type == "plan" && step.OnSuccess == "" {
			return WorkflowValidationError{
				File:    filename,
				Field:   fmt.Sprintf("steps.%s.on_success", stepID),
				Message: "on_success is required for plan steps",
			}
		}

		// Validate on_success references
		if step.OnSuccess != "" {
			if err := w.validateTransition(stepID, "on_success", step.OnSuccess, filename); err != nil {
				return err
			}
		}
		if step.OnFail != "" {
			if err := w.validateTransition(stepID, "on_fail", step.OnFail, filename); err != nil {
				return err
			}
		}

		// Validate gates
		for i, gate := range step.Gates {
			if gate.Condition == "" {
				return WorkflowValidationError{
					File:    filename,
					Field:   fmt.Sprintf("steps.%s.gates[%d].condition", stepID, i),
					Message: "gate condition is required",
				}
			}
			if gate.Target == "" {
				return WorkflowValidationError{
					File:    filename,
					Field:   fmt.Sprintf("steps.%s.gates[%d].target", stepID, i),
					Message: "gate target is required",
				}
			}
			if err := w.validateTransition(stepID, fmt.Sprintf("gates[%d].target", i), gate.Target, filename); err != nil {
				return err
			}
		}

		// Agent steps route via on_success first: the executor does
		// `nextStepID := step.OnSuccess` and only evaluates inline gates
		// when on_success is empty (internal/executor/workflow.go ~L791).
		// So an agent step that sets BOTH has dead gates — a silent
		// footgun that FAILED a real task with no PR opened (incident
		// 2026-06-13, issue-fix resume gate). Gate-type steps are exempt:
		// they evaluate gates first and use on_success as the legitimate
		// default/fallback (e.g. trading.md `maybe_execute`).
		if step.Type == "agent" && step.OnSuccess != "" && len(step.Gates) > 0 {
			return WorkflowValidationError{
				File:    filename,
				Field:   fmt.Sprintf("steps.%s.on_success", stepID),
				Message: "an agent step must not set both on_success and gates — on_success shadows the gates, leaving them dead; remove on_success and let the gates route (use on_fail as the catch-all)",
			}
		}
	}

	// Validate terminals
	for termID, term := range w.Terminals {
		if term.Status == "" {
			return WorkflowValidationError{
				File:    filename,
				Field:   fmt.Sprintf("terminals.%s.status", termID),
				Message: "terminal status is required",
			}
		}
		validStatuses := map[string]bool{"COMPLETED": true, "FAILED": true, "CANCELLED": true}
		if !validStatuses[term.Status] {
			return WorkflowValidationError{
				File:    filename,
				Field:   fmt.Sprintf("terminals.%s.status", termID),
				Message: fmt.Sprintf("invalid terminal status '%s', must be one of: COMPLETED, FAILED, CANCELLED", term.Status),
			}
		}
	}

	// Check for reachability (all steps should be reachable from entrypoint)
	if err := w.validateReachability(filename); err != nil {
		return err
	}

	// Structural placement guard for stage_child_artifacts (§3.3).
	if err := w.validateStageChildArtifacts(filename); err != nil {
		return err
	}

	// Cumulative fan-out load check across parallel steps on any single
	// root-to-terminal path (parallel-fanout LLD §5). Partial approximation
	// — the runtime N4 guard is authoritative.
	if err := w.validateCumulativeFanOut(filename); err != nil {
		return err
	}

	return nil
}

// parallelCumulativeFanOutLimit is the static bound used by the load-time
// cumulative fan-out check. It mirrors the delegation engine's default
// per-parent fan-out limit (defaultDelegationFanOutLimit = 20 in the
// executor). The registry has no access to runtime config, so this is a
// fail-fast convenience for the static-only case; the runtime N4 guard —
// which counts actual cumulative children including sub-workflow
// delegations the static check cannot see — is the authoritative bound
// (parallel-fanout LLD §5).
const parallelCumulativeFanOutLimit = 20

// ParseJoinPolicy validates and decodes a parallel step's join_policy.
// Empty is treated as "all". Returns the normalized kind ("all",
// "best_effort", or "quorum") and, for quorum, the threshold n. branchCount
// bounds a quorum threshold to 1 ≤ n ≤ branchCount. A malformed policy
// returns an error. Shared by the registry validator and the executor's
// wake-path policy evaluation so the two never drift (parallel-fanout LLD
// §4.3/§5).
func ParseJoinPolicy(policy string, branchCount int) (kind string, n int, err error) {
	switch {
	case policy == "" || policy == "all":
		return "all", 0, nil
	case policy == "best_effort":
		return "best_effort", 0, nil
	case strings.HasPrefix(policy, "quorum:"):
		raw := strings.TrimPrefix(policy, "quorum:")
		q, convErr := strconv.Atoi(raw)
		if convErr != nil {
			return "", 0, fmt.Errorf("join_policy quorum threshold %q is not an integer", raw)
		}
		if q < 1 {
			return "", 0, fmt.Errorf("join_policy quorum threshold must be ≥1, got %d", q)
		}
		if branchCount > 0 && q > branchCount {
			return "", 0, fmt.Errorf("join_policy quorum threshold %d exceeds branch count %d", q, branchCount)
		}
		return "quorum", q, nil
	default:
		return "", 0, fmt.Errorf("join_policy %q is not one of: all, best_effort, quorum:<n>", policy)
	}
}

// validateParallelStep enforces the structural contract for a `parallel`
// step (parallel-fanout LLD §5): ≥1 branch each with a unique non-empty id
// + role + prompt; a non-empty `join` resolving to an existing non-parallel
// step; a well-formed `join_policy`; and NO on_success/on_fail/gates (the
// only forward edge is `join`; proceed-false is handled by the runtime
// child-failure bubble-up, not a graph edge). Branch `workflow` references
// are checked cross-workflow at config-set load (validateConfigSet), not
// here, because a single-file Validate cannot see other workflows.
func (w *Workflow) validateParallelStep(filename, stepID string, step WorkflowStep) error {
	// A `parallel` step MUST be the workflow entrypoint (parallel-fanout LLD
	// v0.6 §1). The whole resume model depends on it: a WAITING_FOR_CHILDREN →
	// QUEUED parent is re-dispatched as a FRESH execution at the entrypoint with
	// empty state (executor.go), so the parallel step must be that entrypoint to
	// (a) be re-entered on resume, (b) have no predecessor steps to re-run, and
	// (c) have no predecessor-created children to poison first-pass detection.
	// This also makes a second parallel step impossible (one entrypoint), which
	// eliminates the "second parallel sees the first's children" bug (C2).
	if stepID != w.Entrypoint {
		return WorkflowValidationError{
			File:    filename,
			Field:   fmt.Sprintf("steps.%s", stepID),
			Message: fmt.Sprintf("a parallel step must be the workflow entrypoint (entrypoint is '%s'); the resume model requires it (LLD v0.6 §1)", w.Entrypoint),
		}
	}
	if len(step.Branches) == 0 {
		return WorkflowValidationError{
			File:    filename,
			Field:   fmt.Sprintf("steps.%s.branches", stepID),
			Message: "a parallel step requires at least one branch",
		}
	}
	// A parallel step's only forward edge is `join`. Mirrors the agent-step
	// on_success-vs-gates footgun guard.
	if step.OnSuccess != "" || step.OnFail != "" || len(step.Gates) > 0 {
		return WorkflowValidationError{
			File:    filename,
			Field:   fmt.Sprintf("steps.%s", stepID),
			Message: "a parallel step must not set on_success, on_fail, or gates — its only edge is `join`; a proceed-false join is handled by the runtime child-failure bubble-up",
		}
	}
	if step.Join == "" {
		return WorkflowValidationError{
			File:    filename,
			Field:   fmt.Sprintf("steps.%s.join", stepID),
			Message: "join is required for parallel steps (the non-parallel consumer step to resume at)",
		}
	}
	if step.Join == stepID {
		// A self-join (join == the parallel step, which is the entrypoint) would
		// resume the parent right back into the fan-out — an infinite loop.
		return WorkflowValidationError{
			File:    filename,
			Field:   fmt.Sprintf("steps.%s.join", stepID),
			Message: "join target must not be the parallel step itself (a self-join would resume back into the fan-out)",
		}
	}
	if _, isStep := w.Steps[step.Join]; !isStep {
		return WorkflowValidationError{
			File:    filename,
			Field:   fmt.Sprintf("steps.%s.join", stepID),
			Message: fmt.Sprintf("join target '%s' not found in steps (a parallel step must join a non-parallel step, not a terminal)", step.Join),
		}
	}
	// Note: a join target cannot itself be a parallel step — the entrypoint
	// rule above guarantees at most one parallel step (the entrypoint), and
	// join != entrypoint, so any second parallel step is already rejected as
	// non-entrypoint. No separate join-type check is needed (LLD v0.6 §1).
	if _, _, err := ParseJoinPolicy(step.JoinPolicy, len(step.Branches)); err != nil {
		return WorkflowValidationError{
			File:    filename,
			Field:   fmt.Sprintf("steps.%s.join_policy", stepID),
			Message: err.Error(),
		}
	}
	return validateParallelBranches(filename, stepID, step.Branches)
}

// validateParallelBranches checks each declared leg has a workflow-unique,
// non-empty id plus a role and prompt (parallel-fanout LLD §5). Split out of
// validateParallelStep to keep that function within the lint length budget.
func validateParallelBranches(filename, stepID string, branches []WorkflowBranch) error {
	seen := make(map[string]bool, len(branches))
	for i, b := range branches {
		if b.ID == "" {
			return WorkflowValidationError{
				File:    filename,
				Field:   fmt.Sprintf("steps.%s.branches[%d].id", stepID, i),
				Message: "branch id is required and must be non-empty",
			}
		}
		if seen[b.ID] {
			return WorkflowValidationError{
				File:    filename,
				Field:   fmt.Sprintf("steps.%s.branches[%d].id", stepID, i),
				Message: fmt.Sprintf("duplicate branch id '%s'", b.ID),
			}
		}
		seen[b.ID] = true
		if b.Role == "" {
			return WorkflowValidationError{
				File:    filename,
				Field:   fmt.Sprintf("steps.%s.branches[%d].role", stepID, i),
				Message: fmt.Sprintf("branch '%s' role is required", b.ID),
			}
		}
		if b.Prompt == "" {
			return WorkflowValidationError{
				File:    filename,
				Field:   fmt.Sprintf("steps.%s.branches[%d].prompt", stepID, i),
				Message: fmt.Sprintf("branch '%s' prompt is required", b.ID),
			}
		}
	}
	return nil
}

// validateBranchWorkflowRefs checks that every `parallel` branch's optional
// `workflow` sub-workflow reference resolves to a known workflow id. Run at
// config-set load (validateConfigSet) where the full workflow set is
// available; a single-file Validate cannot see other workflows. v1 does NOT
// recursively inspect the referenced sub-workflow for a nested parallel step
// — that nesting is bounded at runtime by the delegation depth guard
// (parallel-fanout LLD §5).
func (w *Workflow) validateBranchWorkflowRefs(filename string, known map[string]*Workflow) error {
	for stepID, step := range w.Steps {
		if step.Type != "parallel" {
			continue
		}
		for i, b := range step.Branches {
			ref := strings.TrimSpace(b.Workflow)
			if ref == "" {
				continue
			}
			if _, ok := known[ref]; !ok {
				return WorkflowValidationError{
					File:    filename,
					Field:   fmt.Sprintf("steps.%s.branches[%d].workflow", stepID, i),
					Message: fmt.Sprintf("branch '%s' references non-existent workflow '%s'", b.ID, ref),
				}
			}
		}
	}
	return nil
}

// validateCumulativeFanOut rejects a `parallel` step whose static branch count
// exceeds the fan-out limit at load. Because a parallel step MUST be the
// workflow entrypoint (LLD v0.6 §1), there is at most one per workflow, so the
// old cross-parallel path-sum is gone — a single branch-count check suffices.
// This is a fail-fast convenience for the obvious static breach; the runtime N4
// guard (which counts actual cumulative children incl. sub-workflow
// delegations, and across retries) remains the authoritative bound
// (parallel-fanout LLD §5).
func (w *Workflow) validateCumulativeFanOut(filename string) error {
	for stepID, step := range w.Steps {
		if step.Type != "parallel" {
			continue
		}
		if len(step.Branches) > parallelCumulativeFanOutLimit {
			return WorkflowValidationError{
				File:    filename,
				Field:   fmt.Sprintf("steps.%s.branches", stepID),
				Message: fmt.Sprintf("parallel fan-out declares %d branches, exceeding the static limit of %d (the runtime N4 guard is authoritative, but this obvious breach is rejected at load)", len(step.Branches), parallelCumulativeFanOutLimit),
			}
		}
	}
	return nil
}

// validateStageChildArtifacts enforces the structural placement guard for the
// stage_child_artifacts step flag (delegated-child-artifact-handoff design
// §3.3; Task 3 Part B). It is defense-in-depth on top of the step-level opt-in.
//
// WHY a STRUCTURAL lint and NOT a "git-merge-back-typed" refusal: the design and
// review originally framed the guard as "refuse stage_child_artifacts on
// git-merge-back workflows" — to protect issue-fix's diff-based review from
// being perturbed by staged child artifacts. But EVERY project workspace is a
// git repo (ensureGitRepo) and every task runs in a per-task worktree merged
// back onto the single default branch, so ALL workflows are effectively
// "git-merge-back-typed". It is NOT a clean, statically detectable workflow
// property, and any heuristic approximating it would be fragile and misleading.
// The real, implementable protection is two-fold: (1) the flag is a step-level
// opt-in, so a git/diff workflow like issue-fix is protected simply by never
// declaring it; (2) this lint refuses the flag anywhere it is structurally
// nonsensical. Together they make it impossible for a future author to
// accidentally perturb a git workflow by mis-placing the flag.
//
// The flag is valid ONLY on a step that is the post-delegation resume consumer:
//   - the workflow must be resume_after_children — the only mode in which a
//     parent pauses for delegated children and later resumes to consume them
//     (this alone refuses leaf / non-aggregating workflows); AND
//   - the declaring step must be reachable AFTER a fan-out origin (the
//     resume_after_children entrypoint, or any step pinning delegated_workflow)
//     and must NOT itself be a fan-out origin — a decompose / router step spawns
//     the children, it is never the consumer that reads them back.
//
// This refuses the three real footguns — the flag on a leaf workflow, on the
// decompose/delegator step, and on the entrypoint — while imposing nothing on
// the git workflows that never declare it.
func (w *Workflow) validateStageChildArtifacts(filename string) error {
	// Cheap pre-scan: skip all graph work unless some step opts in.
	var declared bool
	for _, step := range w.Steps {
		if step.StageChildArtifacts {
			declared = true
			break
		}
	}
	if !declared {
		return nil
	}

	origins := w.fanOutOrigins()
	reachedAfterFanOut := w.stepsReachableAfterOrigins(origins)

	for stepID, step := range w.Steps {
		if !step.StageChildArtifacts {
			continue
		}
		if !w.ResumeAfterChildren {
			return WorkflowValidationError{
				File:    filename,
				Field:   fmt.Sprintf("steps.%s.stage_child_artifacts", stepID),
				Message: "stage_child_artifacts is only valid on a resume_after_children workflow (it stages a resuming step's delegated children's artifacts; a leaf/non-aggregating workflow has no such children)",
			}
		}
		if origins[stepID] {
			return WorkflowValidationError{
				File:    filename,
				Field:   fmt.Sprintf("steps.%s.stage_child_artifacts", stepID),
				Message: "stage_child_artifacts must not be set on a fan-out step (the entrypoint / a delegated_workflow decompose step spawns the children) — set it on the post-delegation consumer step that runs after the children complete",
			}
		}
		if !reachedAfterFanOut[stepID] {
			return WorkflowValidationError{
				File:    filename,
				Field:   fmt.Sprintf("steps.%s.stage_child_artifacts", stepID),
				Message: "stage_child_artifacts must be set on a step reachable after a delegation fan-out (the post-delegation resume consumer), not on a step outside the delegation path",
			}
		}
	}
	return nil
}

// fanOutOrigins returns the set of step IDs that spawn delegated children: the
// entrypoint of a resume_after_children workflow (which is the strict-adaptive
// router / decompose, per isStrictRouteStep) and any step that pins
// delegated_workflow (the deterministic delegator marker, per isDelegatorStep).
// These are the origins from which a post-delegation consumer must be reachable
// — and are themselves never valid consumers.
func (w *Workflow) fanOutOrigins() map[string]bool {
	origins := make(map[string]bool)
	if w.ResumeAfterChildren && w.Entrypoint != "" {
		if _, ok := w.Steps[w.Entrypoint]; ok {
			origins[w.Entrypoint] = true
		}
	}
	for id, step := range w.Steps {
		if strings.TrimSpace(step.DelegatedWorkflow) != "" {
			origins[id] = true
		}
		// A `parallel` step is a static fan-out origin: its `join` step is
		// the legal post-delegation consumer (where stage_child_artifacts
		// belongs), and the parallel step itself is never a valid consumer
		// (parallel-fanout LLD §5).
		if step.Type == "parallel" {
			origins[id] = true
		}
	}
	return origins
}

// stepsReachableAfterOrigins returns the set of step IDs reachable via a
// forward transition FROM any fan-out origin (i.e. the origins' successors and
// everything downstream), which is exactly the set of "post-delegation" steps.
// An origin itself is included only if it is reachable from another origin;
// callers additionally exclude origins from being valid consumers.
func (w *Workflow) stepsReachableAfterOrigins(origins map[string]bool) map[string]bool {
	reached := make(map[string]bool)
	for originID := range origins {
		step, ok := w.Steps[originID]
		if !ok {
			continue
		}
		// Follow every forward edge INCLUDING a parallel step's join edge
		// (stepSuccessors), so the join consumer of a parallel fan-out
		// origin is correctly recognised as a post-delegation step.
		for _, next := range w.stepSuccessors(step) {
			w.reachableFrom(next, reached)
		}
	}
	return reached
}

// validateTransition checks that a transition target exists
func (w *Workflow) validateTransition(fromStep, field, target, filename string) error {
	if _, isStep := w.Steps[target]; !isStep {
		if _, isTerminal := w.Terminals[target]; !isTerminal {
			return WorkflowValidationError{
				File:    filename,
				Field:   fmt.Sprintf("steps.%s.%s", fromStep, field),
				Message: fmt.Sprintf("transition target '%s' not found in steps or terminals", target),
			}
		}
	}
	return nil
}

// validateReachability ensures all steps are reachable from the entrypoint
func (w *Workflow) validateReachability(filename string) error {
	visited := make(map[string]bool)
	w.reachableFrom(w.Entrypoint, visited)

	for stepID := range w.Steps {
		if !visited[stepID] {
			return WorkflowValidationError{
				File:    filename,
				Field:   fmt.Sprintf("steps.%s", stepID),
				Message: "step is not reachable from entrypoint",
			}
		}
	}

	return nil
}

// stepSuccessors returns the forward-transition targets of a step: the
// ordinary on_success / on_fail / gate edges, PLUS a `parallel` step's
// `join` edge (a parallel step has no on_success — without this its join
// consumer would be mis-read as unreachable and cycle/fan-out walks would
// skip through it). Shared by reachableFrom, stepsReachableAfterOrigins, and
// validateCumulativeFanOut so all graph walkers follow the same rule
// (parallel-fanout LLD §5).
func (w *Workflow) stepSuccessors(step WorkflowStep) []string {
	var out []string
	if step.Type == "parallel" {
		if step.Join != "" {
			out = append(out, step.Join)
		}
		return out
	}
	if step.OnSuccess != "" {
		out = append(out, step.OnSuccess)
	}
	if step.OnFail != "" {
		out = append(out, step.OnFail)
	}
	for _, gate := range step.Gates {
		if gate.Target != "" {
			out = append(out, gate.Target)
		}
	}
	return out
}

// reachableFrom performs a DFS to find all reachable steps
func (w *Workflow) reachableFrom(current string, visited map[string]bool) {
	if visited[current] {
		return
	}
	visited[current] = true

	step, exists := w.Steps[current]
	if !exists {
		return // It's a terminal
	}

	for _, next := range w.stepSuccessors(step) {
		w.reachableFrom(next, visited)
	}
}

// LoadWorkflows loads all workflow YAML files from the specified directory
func LoadWorkflows(dir string) (map[string]*Workflow, error) {
	workflows := make(map[string]*Workflow)

	workflowsDir := filepath.Join(dir, "workflows")
	entries, err := os.ReadDir(workflowsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return workflows, nil // No workflows directory is ok
		}
		return nil, fmt.Errorf("failed to read workflows directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		// WORKFLOW.md is the only supported workflow file format
		// (2026-05-17 — YAML removed). Stale `.yaml` / `.yml`
		// files left over from the migration are silently
		// ignored, same as any unrelated file type.
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		// A README.md documenting the workflows/ dir is not a workflow
		// definition; skip it (mirrors rolelibrary.Load + LoadSwarms). Without
		// this, a plain-prose README would fail frontmatter parse and abort the
		// ENTIRE registry reload (keep-last-good) — and since 2026-07-17 the
		// config watcher tracks .md, so a stray README edit would trigger that
		// abort and block a real concurrent workflow edit from applying.
		if strings.EqualFold(name, "README.md") {
			continue
		}

		path := filepath.Join(workflowsDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to read workflow file %s: %w", name, err)
		}

		parsed, err := ParseWorkflowMarkdown(data, name)
		if err != nil {
			return nil, err
		}
		workflow := *parsed

		// Validate the workflow
		if err := workflow.Validate(name); err != nil {
			return nil, err
		}

		// Check for duplicate IDs
		if _, exists := workflows[workflow.ID]; exists {
			return nil, WorkflowValidationError{
				File:    name,
				Field:   "workflowId",
				Message: fmt.Sprintf("duplicate workflowId: %s", workflow.ID),
			}
		}

		workflows[workflow.ID] = &workflow
	}

	return workflows, nil
}
