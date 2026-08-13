package persistence

import (
	"context"
	"time"
)

// Per-execution tool grants (02-project-and-swarm-registry §10.1–§10.4).
//
// A grant is the project lead narrowing which MCP tools one step is ADVERTISED,
// bounded by the operator-authored role ceiling. It exists because a project's whole
// tool surface is otherwise re-sent on every iteration of the agent loop — measured
// at 52,430 bytes for a role whose median execution needs 2,256.
//
// The store is APPEND-ONLY. The current grant for a step is the newest row; earlier
// rows are the audit trail. One table serves both because a grant is a privilege
// decision, and splitting "current state" from "what was decided" lets them disagree.

// ExecutionToolGrant is one recorded grant or escalation decision.
type ExecutionToolGrant struct {
	ID          string `json:"id"`
	ExecutionID string `json:"execution_id"`
	ProjectID   string `json:"project_id"`
	StepID      string `json:"step_id"`
	Role        string `json:"role"`
	// RequestedTools is the lead's request VERBATIM, never the resolved effective
	// set. Resolution is recomputed against the live ceiling on every advertise
	// call, so a hot-reloaded tightening takes effect immediately (§10.4).
	RequestedTools []string `json:"requested_tools"`
	// Accepted is false for a refused request. A refused row is still recorded —
	// a rejected privilege request is exactly what an audit trail is for.
	Accepted bool `json:"accepted"`
	// RefusedTools names what fell outside the ceiling. Audit-only: echoing these
	// to the agent would let injected text enumerate the ceiling by probing
	// (§10.3(4)).
	RefusedTools []string `json:"refused_tools"`
	// IsEscalation marks a mid-step request for more tools within the ceiling,
	// counted against the per-step escalation limit.
	IsEscalation bool `json:"is_escalation"`
	// CeilingHash and CeilingModifiedAt let a reviewer separate "never in the
	// ceiling" from "the ceiling was tightened after the grant" — contents alone
	// cannot distinguish those.
	CeilingHash       string     `json:"ceiling_hash"`
	CeilingModifiedAt *time.Time `json:"ceiling_modified_at,omitempty"`
	Actor             string     `json:"actor"`
	CreatedAt         time.Time  `json:"created_at"`
}

// ExecutionToolGrantRepository persists tool grants.
//
// Deliberately has no Update or Delete: the table is append-only, so a narrowing
// decision cannot be quietly rewritten after the fact.
type ExecutionToolGrantRepository interface {
	// Record appends one grant or escalation decision, accepted or refused.
	Record(ctx context.Context, g *ExecutionToolGrant) error

	// Current returns the newest ACCEPTED grant for a step, or nil when the step
	// has none. Nil means "no narrowing" — the ceiling alone applies, which is the
	// pre-feature behaviour and keeps the feature inert until a lead grants.
	//
	// Refused rows are deliberately skipped: a refused request must not narrow
	// anything, or a hostile grant naming one invalid tool could starve a step.
	Current(ctx context.Context, executionID, stepID string) (*ExecutionToolGrant, error)

	// EscalationCount counts escalation rows for a step, including refused ones.
	// Refused attempts count against the limit on purpose: the limit exists to stop
	// an injected prompt forcing unbounded audited cycles, and a refused cycle costs
	// the same audit write as an accepted one.
	EscalationCount(ctx context.Context, executionID, stepID string) (int, error)
}
