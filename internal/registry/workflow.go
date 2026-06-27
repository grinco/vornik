// Package registry provides in-memory registries for projects, swarms, and workflows.
package registry

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
		}
		if !validTypes[step.Type] {
			return WorkflowValidationError{
				File:    filename,
				Field:   fmt.Sprintf("steps.%s.type", stepID),
				Message: fmt.Sprintf("invalid step type '%s', must be one of: agent, gate, approval, plan, call_project, spawn_project, a2a_call, system", step.Type),
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

	return nil
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

	if step.OnSuccess != "" {
		w.reachableFrom(step.OnSuccess, visited)
	}
	if step.OnFail != "" {
		w.reachableFrom(step.OnFail, visited)
	}
	for _, gate := range step.Gates {
		w.reachableFrom(gate.Target, visited)
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
