package persistence

import (
	"context"
	"time"
)

// Control-plane proposal ledger (LLD 2026-07-07-control-plane-design +
// 2026-07-08-control-plane-implementation-plan, Phase 1).
//
// A proposal is a REVIEWABLE, human-gated change the operator agent (or a
// Tune detector) suggests — a config diff, a model swap, a project scaffold.
// Phase 1 only WRITES proposals (DRAFT) and lets a human APPROVE/REJECT them;
// the *apply* of an approved proposal is Phase 2, so no code here mutates
// daemon config. The gate is the whole safety model: an agent may propose but
// never approve, and a proposer can never approve its own proposal.

// Proposal kinds. CHECK-constrained in both backends' schema.
const (
	ProposalKindConfig   = "config"
	ProposalKindModel    = "model"
	ProposalKindScaffold = "scaffold"
)

// Proposal blast-radius levels (model ⊂ project ⊂ swarm ⊂ daemon). A
// daemon-scope change needs an explicit second operator acknowledgement at
// apply time (Phase 2). CHECK-constrained.
const (
	ProposalScopeModel   = "model"
	ProposalScopeProject = "project"
	ProposalScopeSwarm   = "swarm"
	ProposalScopeDaemon  = "daemon"
)

// Proposal lifecycle states. DRAFT is the only mutable-into state; a decided
// row (APPROVED/REJECTED) is terminal for editing — a new proposal supersedes
// it rather than an in-place edit. APPLIED/ROLLED_BACK are Phase-2 states,
// defined now so the schema is stable.
const (
	ProposalStatusDraft      = "DRAFT"
	ProposalStatusApproved   = "APPROVED"
	ProposalStatusRejected   = "REJECTED"
	ProposalStatusApplied    = "APPLIED"
	ProposalStatusRolledBack = "ROLLED_BACK"
)

// ProposalMaxFieldBytes caps the free-text fields (diff / rationale / evidence
// / pre_apply_snapshot) at 64 KiB — matches the knowledge-skill body cap.
// Create rejects an oversized field rather than truncating.
const ProposalMaxFieldBytes = 65536

// ErrProposalFieldTooLarge is returned by Create when a text field exceeds
// ProposalMaxFieldBytes.
const ErrProposalFieldTooLarge RepositoryError = "control-plane proposal field too large"

// ErrProposalSelfApprove is returned by SetStatus when the actor approving a
// proposal is the same identity that proposed it (self-approval is forbidden —
// the human gate must be a different party from the proposing agent).
const ErrProposalSelfApprove RepositoryError = "control-plane proposal self-approval forbidden"

// ErrProposalNotDraft is returned by SetStatus when the proposal is not in
// DRAFT (a decided row is terminal for approve/reject; supersede instead).
const ErrProposalNotDraft RepositoryError = "control-plane proposal not in DRAFT"

// ControlPlaneProposal is one ledger row.
type ControlPlaneProposal struct {
	ID string
	// ProjectID is the affected project, or "" for a daemon-scope proposal
	// (persisted NULL).
	ProjectID   string
	Kind        string // config | model | scaffold
	BlastRadius string // model | project | swarm | daemon
	Title       string
	Diff        string // the proposed change (unified diff / structured patch)
	Rationale   string
	Evidence    string // JSON blob of supporting evidence (logs/metrics refs)
	Status      string
	// ProposedBy / Approver are identity strings (agent execution id or
	// human operator handle). Approver is empty until decided; it must differ
	// from ProposedBy on APPROVED (enforced by SetStatus).
	ProposedBy string
	Approver   string
	// PreApplySnapshot holds the pre-apply config snapshot for rollback. Set
	// at apply time (Phase 2); unused/empty in Phase 1 but persisted so the
	// schema is stable.
	PreApplySnapshot string

	CreatedAt time.Time
	DecidedAt *time.Time
	AppliedAt *time.Time
}

// ProposalListFilter narrows List; the zero value lists every proposal
// newest-first.
type ProposalListFilter struct {
	// ProjectID restricts to one project ("" = no project constraint; note
	// this is a filter, distinct from a daemon-scope proposal's NULL project).
	ProjectID string
	// Statuses restricts to these lifecycle states; empty = any.
	Statuses []string
	// Limit caps the result count; <= 0 = unbounded.
	Limit int
}

// ProposalRepository is the backend-agnostic control-plane proposal ledger.
// Implemented by internal/persistence/{postgres,sqlite} and verified by
// repotest.RunProposalSuite.
type ProposalRepository interface {
	// Create inserts a new proposal. Defaults Status to DRAFT and CreatedAt
	// to now when unset. Rejects a text field over ProposalMaxFieldBytes with
	// ErrProposalFieldTooLarge.
	Create(ctx context.Context, p *ControlPlaneProposal) error

	// GetByID fetches a proposal by id. Returns ErrNotFound if absent.
	GetByID(ctx context.Context, id string) (*ControlPlaneProposal, error)

	// List returns proposals matching the filter, newest-created first.
	List(ctx context.Context, f ProposalListFilter) ([]*ControlPlaneProposal, error)

	// SetStatus transitions a proposal's status and stamps decided_at. Only a
	// DRAFT proposal may go to APPROVED or REJECTED (ErrProposalNotDraft
	// otherwise). Approving with actor == ProposedBy is rejected with
	// ErrProposalSelfApprove. actor is recorded as the approver. Returns
	// ErrNotFound for an unknown id.
	SetStatus(ctx context.Context, id, status, actor string) error
}
