package postmortem

import (
	"context"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/playbook"
)

// Renderer produces a deterministic, operator-friendly explanation
// for a terminal task — no LLM call. It joins the failure class
// playbook entry with the structured evidence (step outcomes, tool
// audit, container-log tail) and emits a short plain-text summary
// plus the structured inputs the caller can render however they
// like.
//
// Use Renderer.Render for the routine "why did this fail" question
// (API /explain endpoint, CLI). Use the existing Explainer.Generate
// when an operator explicitly wants the LLM to weave a prose
// narrative around the evidence (UI "Post-mortem" button) — that
// path persists + bills.
//
// The split solves the user-facing problem twice:
//   - Render is free and instant; never burns tokens.
//   - Generate is opt-in and idempotent (cached in task_post_mortems).
//
// The structured Inputs shape is intentionally aligned with what
// the prior internal/explain package returned so the JSON wire
// format stays compatible.
type Renderer struct {
	Tasks      persistence.TaskRepository
	Executions persistence.ExecutionRepository
	Outcomes   persistence.ExecutionStepOutcomeRepository
	Audits     persistence.ToolAuditRepository
	Logs       LogTailFetcher
}

// NewRenderer constructs a Renderer with the given dependencies.
// nil repos are tolerated — Render degrades to a partial view
// instead of failing when one source is unavailable.
func NewRenderer(
	taskRepo persistence.TaskRepository,
	execRepo persistence.ExecutionRepository,
	outcomeRepo persistence.ExecutionStepOutcomeRepository,
	auditRepo persistence.ToolAuditRepository,
	logs LogTailFetcher,
) *Renderer {
	return &Renderer{
		Tasks:      taskRepo,
		Executions: execRepo,
		Outcomes:   outcomeRepo,
		Audits:     auditRepo,
		Logs:       logs,
	}
}

// RenderResult is what Render returns: the deterministic summary
// paragraph + the structured inputs that fed it. Inputs is exposed
// so the caller (API, CLI, UI) can re-render it in its own format
// — the prose Summary is just one view of the same data.
type RenderResult struct {
	Summary string         `json:"summary"`
	Inputs  RenderedInputs `json:"inputs"`
}

// RenderedInputs is the structured failure-context view. Mirrors
// what the prior internal/explain package emitted so the wire
// format is preserved across the determinism collapse.
type RenderedInputs struct {
	TaskID         string          `json:"task_id"`
	ProjectID      string          `json:"project_id"`
	WorkflowID     string          `json:"workflow_id,omitempty"`
	TaskStatus     string          `json:"task_status"`
	LastError      string          `json:"last_error,omitempty"`
	LastErrorClass string          `json:"last_error_class,omitempty"`
	StepOutcomes   []OutcomeBrief  `json:"step_outcomes,omitempty"`
	RecentTools    []ToolBrief     `json:"recent_tools,omitempty"`
	LogTail        string          `json:"log_tail,omitempty"`
	Playbook       *playbook.Entry `json:"playbook,omitempty"`
}

// OutcomeBrief is one step's outcome as the renderer sees it.
type OutcomeBrief struct {
	StepID      string `json:"step_id"`
	Role        string `json:"role"`
	Model       string `json:"model,omitempty"`
	Outcome     string `json:"outcome"`
	ErrorClass  string `json:"error_class,omitempty"`
	ErrorDetail string `json:"error_detail,omitempty"`
}

// ToolBrief is one tool-call audit row condensed for rendering.
type ToolBrief struct {
	StepID     string `json:"step_id,omitempty"`
	ToolName   string `json:"tool_name"`
	Input      string `json:"input,omitempty"`
	Output     string `json:"output,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

// Render returns the deterministic summary + structured inputs
// for taskID. Never calls an LLM. Returns persistence.ErrNotFound
// when the task itself doesn't exist; otherwise best-effort with
// missing repos / source errors degrading to empty sections.
func (r *Renderer) Render(ctx context.Context, taskID string) (*RenderResult, error) {
	if r == nil {
		return nil, fmt.Errorf("postmortem: nil renderer")
	}
	if r.Tasks == nil {
		return nil, fmt.Errorf("postmortem: task repo is required")
	}
	if taskID == "" {
		return nil, fmt.Errorf("postmortem: task ID is required")
	}

	task, err := r.Tasks.Get(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("postmortem: task lookup failed: %w", err)
	}
	if task == nil {
		return nil, persistence.ErrNotFound
	}

	in := RenderedInputs{
		TaskID:     taskID,
		ProjectID:  task.ProjectID,
		TaskStatus: string(task.Status),
	}
	if task.LastError != nil {
		in.LastError = truncateRunes(*task.LastError, 1500)
	}
	if task.LastErrorClass != nil {
		in.LastErrorClass = *task.LastErrorClass
		entry := playbook.Lookup(*task.LastErrorClass)
		in.Playbook = &entry
	}
	if task.WorkflowID != nil {
		in.WorkflowID = *task.WorkflowID
	}

	var executionID string
	if r.Executions != nil {
		if exec, eerr := r.Executions.GetByTaskID(ctx, taskID); eerr == nil && exec != nil {
			executionID = exec.ID
			if in.WorkflowID == "" {
				in.WorkflowID = exec.WorkflowID
			}
		}
	}
	if r.Outcomes != nil && executionID != "" {
		filter := persistence.ExecutionStepOutcomeFilter{ExecutionID: &executionID, PageSize: 50}
		if outs, oerr := r.Outcomes.List(ctx, filter); oerr == nil {
			for _, o := range outs {
				if o == nil {
					continue
				}
				in.StepOutcomes = append(in.StepOutcomes, OutcomeBrief{
					StepID:      o.StepID,
					Role:        o.Role,
					Model:       o.Model,
					Outcome:     o.Outcome,
					ErrorClass:  o.ErrorClass,
					ErrorDetail: truncateRunes(o.ErrorDetail, 400),
				})
			}
		}
	}
	if r.Audits != nil {
		filter := persistence.ToolAuditFilter{TaskID: &taskID, PageSize: 10}
		if entries, aerr := r.Audits.List(ctx, filter); aerr == nil {
			for _, a := range entries {
				if a == nil {
					continue
				}
				in.RecentTools = append(in.RecentTools, ToolBrief{
					StepID:     a.StepID,
					ToolName:   a.ToolName,
					Input:      truncateRunes(a.ToolInput, 200),
					Output:     truncateRunes(a.ToolOutput, 400),
					DurationMS: a.DurationMs,
				})
			}
		}
	}
	if r.Logs != nil {
		if logs, lerr := r.Logs.TaskLogs(ctx, taskID, 100); lerr == nil {
			in.LogTail = truncateRunes(logs, 4000)
		}
	}

	return &RenderResult{
		Summary: renderSummary(&in),
		Inputs:  in,
	}, nil
}

// renderSummary builds the operator-facing paragraph deterministically.
// Structure:
//  1. Headline — what failed and where (uses class + last step's role/model).
//  2. Proximate cause — first non-empty signal from step outcomes / tool
//     audit / last_error. Conservative: never invents context the
//     evidence doesn't carry.
//  3. Suggested next action — first playbook suggestion when present,
//     otherwise a generic pointer to the failure-class doc.
//
// The output is stable for a given input — useful for diffing the same
// task's failure shape over time, and for tests.
func renderSummary(in *RenderedInputs) string {
	if in == nil {
		return ""
	}
	var b strings.Builder

	// 1. Headline
	b.WriteString(headline(in))

	// 2. Proximate cause
	if cause := proximateCause(in); cause != "" {
		b.WriteString(" ")
		b.WriteString(cause)
	}

	// 3. Next action
	if next := nextAction(in); next != "" {
		b.WriteString(" ")
		b.WriteString(next)
	}

	return strings.TrimSpace(b.String())
}

// headline returns the first sentence of the summary: terse, role-
// aware, class-aware. Operators reading at 11pm want the headline
// to fit on one line.
func headline(in *RenderedInputs) string {
	status := strings.ToLower(in.TaskStatus)
	if status == "" {
		status = "terminal"
	}

	// Prefer "class on step role" when we have both. Otherwise fall
	// back to plain class, then plain status.
	if in.LastErrorClass != "" {
		humanClass := humanizeClass(in.LastErrorClass)
		if step := lastFailingStep(in); step != nil {
			role := step.Role
			if role == "" {
				role = "unnamed step"
			}
			if step.Model != "" {
				return fmt.Sprintf("Task %s failed (%s): the %s step on %s hit %s.",
					in.TaskID, status, role, step.Model, humanClass)
			}
			return fmt.Sprintf("Task %s failed (%s): the %s step hit %s.",
				in.TaskID, status, role, humanClass)
		}
		return fmt.Sprintf("Task %s failed (%s): %s.", in.TaskID, status, humanClass)
	}
	return fmt.Sprintf("Task %s ended in %s with no classified failure.", in.TaskID, status)
}

// proximateCause picks the most-informative non-empty evidence line.
// Order:
//  1. The last step outcome's error_detail (closest to the failure).
//  2. The last tool audit's output prefix.
//  3. task.LastError text.
//
// Returns "" if no evidence is available — the headline + next action
// still give the operator something to act on.
func proximateCause(in *RenderedInputs) string {
	if step := lastFailingStep(in); step != nil && step.ErrorDetail != "" {
		return fmt.Sprintf("Proximate signal: %s", truncateRunes(step.ErrorDetail, 240))
	}
	// Look back through recent tools for one that carries an output —
	// usually the last tool call before failure dropped a stderr-like
	// message into its output.
	for i := len(in.RecentTools) - 1; i >= 0; i-- {
		t := in.RecentTools[i]
		if t.Output != "" {
			return fmt.Sprintf("Last tool %s output: %s",
				t.ToolName, truncateRunes(t.Output, 240))
		}
	}
	if in.LastError != "" {
		return fmt.Sprintf("Error text: %s", truncateRunes(in.LastError, 240))
	}
	return ""
}

// nextAction returns one concrete operator-facing suggestion. Pulls
// from the playbook when available (first entry — cheapest-first
// ordering is the playbook's contract). Falls back to a generic
// pointer when the failure class is unrecognised or the playbook
// entry has no suggestions.
func nextAction(in *RenderedInputs) string {
	if in.Playbook != nil && len(in.Playbook.Suggestions) > 0 {
		return "Try first: " + in.Playbook.Suggestions[0]
	}
	if in.LastErrorClass != "" {
		return fmt.Sprintf("No playbook entry for class %s — read the task's last_error and container log tail in full.",
			in.LastErrorClass)
	}
	return "No classified failure — inspect the task's executions and tool-audit log directly."
}

// lastFailingStep walks step outcomes in reverse to find the first
// non-success row. Returns nil when there are no outcomes or every
// row is success-coloured.
func lastFailingStep(in *RenderedInputs) *OutcomeBrief {
	for i := len(in.StepOutcomes) - 1; i >= 0; i-- {
		o := in.StepOutcomes[i]
		if isFailureOutcome(o.Outcome) {
			return &o
		}
	}
	return nil
}

// isFailureOutcome returns true when the outcome string is one of
// the failure-coloured values (parse_error, schema_violation,
// refused, hallucination, etc.). pending_validation and used count
// as success/in-flight, not failures.
func isFailureOutcome(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "pending_validation", "used":
		return false
	default:
		return true
	}
}

// humanizeClass converts an UPPER_SNAKE failure class string to a
// short lowercase phrase. The class strings are stable wire
// identifiers; this function exists so the headline reads like
// English rather than yelled enum names.
func humanizeClass(class string) string {
	switch class {
	case persistence.TaskFailureClassLLMError:
		return "an LLM/gateway error"
	case persistence.TaskFailureClassTimeout:
		return "a timeout"
	case persistence.TaskFailureClassToolError:
		return "a tool error"
	case persistence.TaskFailureClassToolIterationLimit:
		return "the tool-iteration cap"
	case persistence.TaskFailureClassInvalidOutput:
		return "an invalid-output gate"
	case persistence.TaskFailureClassMergeFailed:
		return "a merge failure"
	case persistence.TaskFailureClassGateFailed:
		return "a gate refusal"
	case persistence.TaskFailureClassBudgetBlocked:
		return "the project budget cap"
	case persistence.TaskFailureClassRateLimited:
		return "a rate limit"
	case persistence.TaskFailureClassWorkflowRole:
		return "a missing workflow role"
	case persistence.TaskFailureClassWorkflowCfg:
		return "a workflow config error"
	case persistence.TaskFailureClassWorkflowDrift:
		return "workflow drift"
	case persistence.TaskFailureClassStuckExecution:
		return "the stuck-execution watchdog"
	case persistence.TaskFailureClassLeaseExpired:
		return "an expired lease"
	case persistence.TaskFailureClassRuntimeError:
		return "a runtime error"
	case persistence.TaskFailureClassCancelled:
		return "operator cancellation"
	case persistence.TaskFailureClassOrphaned:
		return "an orphaned-task scan"
	case persistence.TaskFailureClassSecretLeak:
		return "the secret-leak guard"
	case persistence.TaskFailureClassUnknown:
		return "an unclassified error"
	default:
		// Future class the renderer doesn't know yet — return
		// the raw class lowercased so the wire identifier is
		// still visible.
		return strings.ToLower(strings.ReplaceAll(class, "_", " "))
	}
}

// truncateRunes clips a string to at most maxRunes user-visible
// characters, trimming whitespace. Rune-aware so multi-byte UTF-8
// content (user prompts, model output) doesn't get split mid-glyph.
func truncateRunes(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}
