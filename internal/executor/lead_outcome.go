package executor

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// LeadOutcomeKind is one of the four shapes the lead can emit at
// the end of an execution under the conversational task lifecycle.
// See https://docs.vornik.io §6.2.
type LeadOutcomeKind string

const (
	// LeadOutcomeContinue is the legacy / default shape: lead has
	// a plan, the executor spawns child role steps. Backwards
	// compatible with the pre-Phase-25 envelope (`{"plan":...,
	// "message":...}`).
	LeadOutcomeContinue LeadOutcomeKind = "continue"

	// LeadOutcomeCheckpoint blocks the task on operator input.
	// Executor: write a checkpoint task_message + transition the
	// task to AWAITING_INPUT + skip child step spawning.
	LeadOutcomeCheckpoint LeadOutcomeKind = "checkpoint"

	// LeadOutcomeExternalWait blocks the task on a real-world event
	// (vendor email, scheduled date, etc.). Executor: persist
	// expected_by + transition the task to AWAITING_EXTERNAL.
	LeadOutcomeExternalWait LeadOutcomeKind = "external_wait"

	// LeadOutcomeClosureRequest signals the lead believes the work
	// is done and recommends operator-confirmed closure. Executor:
	// write a closure_request task_message; status stays COMPLETED
	// (the task awaits operator close).
	LeadOutcomeClosureRequest LeadOutcomeKind = "closure_request"
)

// CheckpointKind enumerates the three operator-actionable
// checkpoint variants. See LLD §9.1.
type CheckpointKind string

const (
	CheckpointKindDecision       CheckpointKind = "decision"
	CheckpointKindActionRequired CheckpointKind = "action_required"
	CheckpointKindReview         CheckpointKind = "review"
)

// CheckpointOptionActionType enumerates the structured actions a
// recovery `decision` checkpoint option can carry. See LLD §9
// "Structured recovery-checkpoint actions". The action is OPTIONAL;
// an option without one behaves exactly as today (a prose hint fed
// back to the retried step). An unknown/invalid type is demoted to a
// plain prose option at parse time (fail-safe), never a hard failure.
type CheckpointOptionActionType string

const (
	// CheckpointActionRerouteWorkflow re-runs the work on a different
	// workflow, applied via delegateSelectedWorkflow. Bounded by the
	// project's AdaptiveCandidateWorkflows allow-list; requires a
	// non-empty Workflow.
	CheckpointActionRerouteWorkflow CheckpointOptionActionType = "reroute_workflow"
	// CheckpointActionModelFallback retries with every role swapped onto
	// its configured modelFallback, applied via ApplyFallbackModelOverride
	// (writes the replay-free operator_model_override key).
	CheckpointActionModelFallback CheckpointOptionActionType = "model_fallback"
	// CheckpointActionRetry retries the failed step as-is — the existing
	// prompt-hint behaviour, no new seam.
	CheckpointActionRetry CheckpointOptionActionType = "retry"
	// CheckpointActionSkip skips the failed step — the existing
	// prompt-hint behaviour, no new seam.
	CheckpointActionSkip CheckpointOptionActionType = "skip"
)

// CheckpointOptionAction is the OPTIONAL structured action attached to
// a decision-checkpoint option. When the operator approves an option
// carrying one, the executor APPLIES it structurally before the retry
// (instead of merely feeding the label back as prose). See LLD §9.
type CheckpointOptionAction struct {
	Type CheckpointOptionActionType `json:"type"`
	// Workflow is the target workflow id for reroute_workflow. Required
	// for that type; ignored for the others.
	Workflow string `json:"workflow,omitempty"`
}

// CheckpointOption is one selectable answer for a `decision`
// checkpoint. UI / Telegram render these as buttons.
type CheckpointOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Action, when non-nil, is the structured action applied on operator
	// approval (LLD §9). nil = today's prose-hint behaviour.
	Action *CheckpointOptionAction `json:"action,omitempty"`
}

// valid reports whether the action is well-formed per §9: the type is
// one of the four constants and reroute_workflow carries a non-empty
// workflow. Anything else is demoted to a prose option by the parser.
func (a *CheckpointOptionAction) valid() bool {
	if a == nil {
		return false
	}
	switch a.Type {
	case CheckpointActionRerouteWorkflow:
		return strings.TrimSpace(a.Workflow) != ""
	case CheckpointActionModelFallback, CheckpointActionRetry, CheckpointActionSkip:
		return true
	default:
		return false
	}
}

// CheckpointPayload is the structured body the lead emits when
// outcome=checkpoint. Persisted as task_message metadata so the
// UI / Telegram / CLI render the right affordance.
type CheckpointPayload struct {
	Kind                CheckpointKind     `json:"kind"`
	Question            string             `json:"question,omitempty"`
	Options             []CheckpointOption `json:"options,omitempty"`         // decision only
	TaskForHuman        string             `json:"task_for_human,omitempty"`  // action_required only
	ExpectedFormat      string             `json:"expected_format,omitempty"` // action_required hint
	Draft               string             `json:"draft,omitempty"`           // review only
	ExpectedBy          *time.Time         `json:"expected_by,omitempty"`
	DefaultIfNoResponse string             `json:"default_if_no_response,omitempty"`
	DefaultReason       string             `json:"default_reason,omitempty"`
}

// ExternalWaitPayload carries the deadline + optional event
// matcher for outcome=external_wait. event_match is consumed by
// Phase 30's webhook event matcher.
type ExternalWaitPayload struct {
	ExpectedBy *time.Time      `json:"expected_by,omitempty"`
	EventMatch json.RawMessage `json:"event_match,omitempty"` // tiny predicate language; opaque here
	Reason     string          `json:"reason,omitempty"`
}

// ClosureRequestPayload is the lead's recommended-final summary.
type ClosureRequestPayload struct {
	Summary             string   `json:"summary,omitempty"`
	ArtifactsToArchive  []string `json:"artifacts_to_archive,omitempty"`
	FinalCostUSD        float64  `json:"final_cost_usd,omitempty"`
	DurationDescription string   `json:"duration_description,omitempty"`
}

// PhaseTransition is one entry in the lead's phase log.
type PhaseTransition struct {
	Phase  string `json:"phase"`
	Status string `json:"status"` // enter | exit | skip
}

// ScratchpadUpdate is the partial-update shape the lead emits.
// Each field is optional; the executor merges what's present onto
// the existing task_scratchpad row.
type ScratchpadUpdate struct {
	Summary       string          `json:"summary,omitempty"`
	Facts         json.RawMessage `json:"facts,omitempty"` // arbitrary object
	OpenQuestions []string        `json:"open_questions,omitempty"`
	CurrentPhase  string          `json:"current_phase,omitempty"`
}

// LeadOutcome is the parsed envelope. Exactly one of Plan,
// Checkpoint, ExternalWait, ClosureRequest is populated based on
// Outcome.
type LeadOutcome struct {
	Outcome          LeadOutcomeKind        `json:"outcome,omitempty"`
	Version          int                    `json:"version,omitempty"` // envelope schema version; default 1
	Message          string                 `json:"message,omitempty"`
	Plan             *PlanShape             `json:"plan,omitempty"`
	Checkpoint       *CheckpointPayload     `json:"checkpoint,omitempty"`
	ExternalWait     *ExternalWaitPayload   `json:"external_wait,omitempty"`
	ClosureRequest   *ClosureRequestPayload `json:"closure_request,omitempty"`
	ScratchpadUpdate *ScratchpadUpdate      `json:"scratchpad_update,omitempty"`
	PhaseTransitions []PhaseTransition      `json:"phase_transitions,omitempty"`
	// Complexity is the lead's coarse complexity verdict for the task
	// (trivial|standard|complex|open_ended). Optional; empty/unknown is
	// treated as standard. Drives dynamic per-role tool-iteration
	// budgets. See https://docs.vornik.io
	Complexity string `json:"complexity,omitempty"`
}

// PlanShape mirrors the legacy plan envelope; kept here so the
// lead outcome can carry it through the same parser. The phase
// field is new in Phase 25 — the lead hints which phase this
// execution advances.
type PlanShape struct {
	Steps     []string `json:"steps"`
	Rationale string   `json:"rationale,omitempty"`
	Phase     string   `json:"phase,omitempty"`
}

// ParseLeadOutcome attempts to parse the lead's raw output as a
// LeadOutcome envelope. Backwards compatible:
//
//   - When the JSON has an explicit `outcome` field, the new
//     envelope is used.
//   - When `outcome` is absent but `plan` is present, the result
//     is treated as outcome=continue (legacy shape). This lets
//     swarms that haven't been re-prompted keep working through
//     the rollout.
//
// Returns (outcome, true, nil) on success; (nil, false, err) when
// the JSON is malformed or doesn't fit either shape.
//
// "Strict mode" is left for follow-up: today, when both `outcome`
// and `plan` are present and inconsistent (outcome=checkpoint but
// also plan.steps non-empty), the explicit `outcome` wins and the
// stray plan is ignored with a warning logged by the caller.
func ParseLeadOutcome(data []byte) (*LeadOutcome, bool, error) {
	if len(data) == 0 {
		return nil, false, fmt.Errorf("empty lead output")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, false, fmt.Errorf("invalid lead JSON: %w", err)
	}
	hasOutcome := raw["outcome"] != nil
	hasPlan := raw["plan"] != nil

	// Always re-marshal+unmarshal to coerce types into the
	// LeadOutcome shape. Both the new envelope and the legacy
	// shape share the same field names, so a single decode works.
	var out LeadOutcome
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, false, fmt.Errorf("lead envelope decode: %w", err)
	}

	switch {
	case hasOutcome:
		// Explicit outcome — validate it's one we know.
		out.Outcome = LeadOutcomeKind(strings.ToLower(string(out.Outcome)))
		switch out.Outcome {
		case LeadOutcomeContinue, LeadOutcomeCheckpoint,
			LeadOutcomeExternalWait, LeadOutcomeClosureRequest:
			// ok
		default:
			return nil, false, fmt.Errorf("unknown outcome %q", out.Outcome)
		}
	case hasPlan:
		// Legacy shape — treat as continue.
		out.Outcome = LeadOutcomeContinue
	default:
		return nil, false, fmt.Errorf("lead output has neither 'outcome' nor 'plan'")
	}

	if out.Version == 0 {
		out.Version = 1
	}

	// Fail-safe normalization (LLD §9): demote any invalid/unknown
	// checkpoint-option action to a plain prose option (drop the
	// action) BEFORE shape validation, so a malformed structured
	// action never hard-fails the recovery checkpoint.
	normalizeCheckpointOptionActions(&out)

	// Per-outcome shape validation. Surfaces the obvious bugs
	// (checkpoint with no question, external_wait with no
	// expected_by) early so they don't sneak past the executor.
	if err := validateLeadOutcome(&out); err != nil {
		return nil, false, err
	}
	return &out, true, nil
}

// normalizeCheckpointOptionActions enforces the §9 fail-safe: any
// checkpoint-option action that doesn't validate (unknown type, or
// reroute_workflow with no workflow) is DEMOTED to a plain prose
// option by dropping the action. The option's id/label are preserved
// so the operator still sees the choice; it just loses the structural
// behaviour and falls back to today's prose-hint path. Never errors.
func normalizeCheckpointOptionActions(o *LeadOutcome) {
	if o == nil || o.Checkpoint == nil {
		return
	}
	for i := range o.Checkpoint.Options {
		if a := o.Checkpoint.Options[i].Action; a != nil && !a.valid() {
			o.Checkpoint.Options[i].Action = nil
		}
	}
}

// validateLeadOutcome checks the per-outcome required fields. The
// executor refuses to act on a malformed outcome — better to fail
// the lead step than to write a half-formed checkpoint.
func validateLeadOutcome(o *LeadOutcome) error {
	switch o.Outcome {
	case LeadOutcomeContinue:
		if o.Plan == nil || len(o.Plan.Steps) == 0 {
			return fmt.Errorf("outcome=continue requires plan.steps")
		}
	case LeadOutcomeCheckpoint:
		if o.Checkpoint == nil {
			return fmt.Errorf("outcome=checkpoint requires checkpoint payload")
		}
		switch o.Checkpoint.Kind {
		case CheckpointKindDecision:
			if o.Checkpoint.Question == "" || len(o.Checkpoint.Options) < 2 {
				return fmt.Errorf("decision checkpoint needs question + ≥2 options")
			}
		case CheckpointKindActionRequired:
			if o.Checkpoint.TaskForHuman == "" {
				return fmt.Errorf("action_required checkpoint needs task_for_human")
			}
		case CheckpointKindReview:
			if o.Checkpoint.Draft == "" {
				return fmt.Errorf("review checkpoint needs draft")
			}
		default:
			return fmt.Errorf("unknown checkpoint.kind %q", o.Checkpoint.Kind)
		}
	case LeadOutcomeExternalWait:
		if o.ExternalWait == nil || o.ExternalWait.ExpectedBy == nil {
			return fmt.Errorf("external_wait requires expected_by")
		}
	case LeadOutcomeClosureRequest:
		if o.ClosureRequest == nil || o.ClosureRequest.Summary == "" {
			return fmt.Errorf("closure_request requires summary")
		}
	}
	return nil
}

// SerializeCheckpointMetadata renders a CheckpointPayload as
// task_message metadata JSON. The UI/Telegram/CLI all read this
// shape to render the appropriate affordance.
func SerializeCheckpointMetadata(c *CheckpointPayload) ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("nil checkpoint")
	}
	return json.Marshal(c)
}
