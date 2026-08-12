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
	// ProposalKindInstinctRetire is a KindApplier-managed kind (2026-07-19-
	// instinct-lift-measurement-design.md §4.5): applying it retires an
	// instinct via InstinctRepository rather than rewriting a config file.
	ProposalKindInstinctRetire = "instinct_retire"
	// ProposalKindObservation is a detector finding with NO applyable change —
	// "p95 latency is high on this project; investigate". It is browsable and
	// auditable like any other row, but it is NOT decidable: there is nothing to
	// approve or apply, so the hub renders it read-only and Approve/Apply refuse
	// it.
	//
	// Introduced 2026-08-10 after a live-ledger audit found 19 proposals sitting
	// in APPROVED with apply_target, apply_ops and apply_content all empty.
	// Approving them accomplished nothing, and because approval moved them out
	// of DRAFT — the only status the title dedup consulted — the detector
	// re-filed the identical title days later. 10 of the 19 were re-files of a
	// title already in APPROVED: clearing the inbox was what refilled it.
	ProposalKindObservation = "observation"
)

// IsObservationKind reports whether a proposal is a non-decidable observation.
// Approve/apply paths refuse these: there is no change to act on, and offering
// a decision for something with no action is how the inert-APPROVED pile formed.
func IsObservationKind(kind string) bool { return kind == ProposalKindObservation }

// KindApplierManaged reports whether this kind is applied by a registered
// state-mutating KindApplier (internal/controlplane) rather than the
// file-based apply path — such proposals are applyable even with empty
// ApplyTarget/ApplyOps.
func KindApplierManaged(kind string) bool { return kind == ProposalKindInstinctRetire }

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
	// ProposalStatusRegressed is a strict specialization of ROLLED_BACK:
	// "auto-rolled-back by the cost/quality canary guard because a post-apply
	// regression tripped" (design 2026-07-24 §D). It is a best-effort AUDIT
	// badge written AFTER the durable canary trip record — trip-prevention +
	// cooldown key off the canary row, never this status (C2). MarkRegressed
	// accepts BOTH APPLIED→REGRESSED and ROLLED_BACK→REGRESSED.
	ProposalStatusRegressed = "REGRESSED"
)

// CostAutoApplyActor is the applied_by / approver identity stamped on every
// proposal the cost-auto-apply worker approves, applies, or rejects (auto-apply
// design D5/D6/D9). It is ALSO the M=1 re-seed signal: LastApplyActorForKnob
// returning this value means the knob was last auto-applied, so auto-apply is
// blocked until a human re-seeds. Defined in persistence (not controlplane) so
// both the worker and the repotest suites can reference it without an import
// cycle.
const CostAutoApplyActor = "cost-auto-apply"

// ProposalMaxFieldBytes caps the free-text fields (diff / rationale /
// evidence) at 64 KiB — matches the knowledge-skill body cap. Create rejects
// an oversized field rather than truncating. These are prose rendered into
// the review pane, so a tight bound is the point.
const ProposalMaxFieldBytes = 65536

// ProposalMaxContentBytes caps the WHOLE-FILE fields — apply_content, the
// pre_apply_snapshot taken from the same file, and apply_ops (whose JSON
// embeds per-op content). A config file is not prose and does not belong
// under the free-text bound.
//
// Split out 2026-08-05 after the two shared one 64 KiB cap. The live
// config.yaml had grown to 81 KB, and `apply_content` was the one field
// Create did NOT validate — so the hub cheerfully created a daemon-scope MCP
// proposal, showed a clean diff, and Apply then refused it with
// ErrContentTooLarge before any write. Every whole-file config edit through
// the control-plane hub was un-appliable on that deployment, reported as a
// generic "apply failed". Storage was never the constraint: all three columns
// are TEXT.
const ProposalMaxContentBytes = 1 << 20 // 1 MiB

// ErrProposalFieldTooLarge is returned by Create when a text field exceeds
// ProposalMaxFieldBytes.
const ErrProposalFieldTooLarge RepositoryError = "control-plane proposal field too large"

// ErrProposalSelfApprove is returned by SetStatus when the actor approving a
// proposal is the same identity that proposed it (self-approval is forbidden —
// the human gate must be a different party from the proposing agent).
const ErrProposalSelfApprove RepositoryError = "control-plane proposal self-approval forbidden"

// ErrProposalNotDraft is returned by SetStatus when APPROVE is attempted on a
// proposal that is not in DRAFT (a decided row can't be re-approved).
const ErrProposalNotDraft RepositoryError = "control-plane proposal not in DRAFT"

// ErrProposalNotPending is returned by SetStatus when REJECT/withdraw is
// attempted on a proposal that is neither DRAFT nor APPROVED — i.e. already
// terminal (REJECTED/APPLIED/ROLLED_BACK). Reject is allowed from DRAFT (reject
// a pending proposal) or APPROVED (withdraw an approved-but-unappliable one).
const ErrProposalNotPending RepositoryError = "control-plane proposal not in a pending (DRAFT/APPROVED) state"

// ErrProposalNotDecidable is returned by SetStatus when APPROVE is attempted on
// an OBSERVATION. An observation carries no applyable change, so approving it
// can never do anything — which is precisely how 19 inert rows accumulated in
// APPROVED before 2026-08-10. Reject is still permitted: dismissing an
// observation is the legitimate way to clear one.
const ErrProposalNotDecidable RepositoryError = "control-plane proposal is an observation and cannot be approved"

// ErrProposalNotApproved is returned by MarkApplied when the proposal is not
// APPROVED (only an APPROVED proposal can be applied; idempotent single-apply).
const ErrProposalNotApproved RepositoryError = "control-plane proposal not APPROVED"

// ErrProposalNotApplied is returned by MarkRolledBack when the proposal is not
// APPLIED (only an applied change can be rolled back).
const ErrProposalNotApplied RepositoryError = "control-plane proposal not APPLIED"

// ErrProposalNotRegressable is returned by MarkRegressed when the proposal is
// neither APPLIED nor ROLLED_BACK (the two states the audit badge accepts —
// APPLIED→REGRESSED and, the transition that actually executes in the trip
// path, ROLLED_BACK→REGRESSED). Design 2026-07-24 §4.4.
const ErrProposalNotRegressable RepositoryError = "control-plane proposal not in APPLIED or ROLLED_BACK"

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
	// PreApplySnapshot holds the pre-apply config file bytes for rollback,
	// captured at apply time (Phase 2). Option A: the REAL bytes (not
	// redacted at rest) so a rollback can restore exactly; the row is
	// operator-scope-only. Never surfaced by read handlers.
	PreApplySnapshot string

	// ApplyTarget / ApplyContent make a proposal APPLYABLE (Phase 2,
	// op=replace_file): ApplyTarget is the config file path (relative to the
	// deployed config dir; path-traversal-guarded at apply) and ApplyContent
	// is the full new file contents. Empty ApplyTarget = review-only (apply
	// refuses it) — e.g. Tune-detector proposals that describe a problem, not
	// a specific rewrite.
	ApplyTarget  string
	ApplyContent string
	// ApplyOps carries a MULTI-FILE apply (Phase 2b scaffold): a JSON array of
	// {op:create|replace, path, content}. When empty, the engine falls back to
	// the single (ApplyTarget, ApplyContent) as a one-element replace op
	// (back-compat with Phase-2a proposals). Persisted as the raw JSON string.
	ApplyOps string
	// AppliedBy is the operator who applied it — set ONLY on a successful
	// apply (a failed/aborted apply leaves it empty).
	AppliedBy string
	// LiveApply, when true, lets ApplyEngine.Apply skip ONLY the all-projects
	// busy gate — the change is declared non-disruptive to in-flight tasks by
	// the proposer (e.g. the MCP-hub add/remove proposer: the MCP catalog is
	// injected into agent containers at start, so a running task never sees a
	// mid-flight catalog change). Every other invariant (APPROVED, daemon-ack,
	// base-hash, validate, atomic write, reload+rollback) still runs. Default
	// false ⇒ current behavior (busy-gated). See
	// https://docs.vornik.io
	LiveApply bool

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

	// MarkApplied transitions an APPROVED proposal to APPLIED, stamping
	// applied_at + applied_by and storing the pre-apply snapshot (the real
	// file bytes for rollback). Only APPROVED → APPLIED is allowed
	// (ErrProposalNotApproved otherwise). Returns ErrNotFound for an unknown
	// id. Caller writes the snapshot BEFORE the config file (design §Apply).
	MarkApplied(ctx context.Context, id, appliedBy, snapshot string) error

	// StagePreApplySnapshot writes pre_apply_snapshot on an APPROVED proposal
	// WITHOUT changing status — the cost-auto-apply worker pre-stages the
	// pre-image before ApplyEngine.Apply writes any file, so the
	// atomicWrite→MarkApplied crash window is recoverable by the startup
	// reconciler (auto-apply design D8). Only APPROVED is accepted
	// (ErrProposalNotApproved otherwise); ErrNotFound for an unknown id. Safe to
	// overwrite — MarkApplied later writes the identical pre-image.
	StagePreApplySnapshot(ctx context.Context, id, snapshot string) error

	// MarkRolledBack transitions an APPLIED proposal to ROLLED_BACK. Only
	// APPLIED → ROLLED_BACK is allowed (ErrProposalNotApplied otherwise).
	MarkRolledBack(ctx context.Context, id string) error

	// RefreshObservation updates an OBSERVATION row's rationale + evidence in
	// place, leaving its status and id untouched. Observations recur, and a
	// recurrence is an update to an existing finding rather than a new one —
	// re-filing instead is what produced the 2026-08-10 inert-APPROVED pile.
	// Leaving status alone is deliberate: an observation the operator has
	// already dismissed stays dismissed even while it keeps happening; the
	// evidence's occurrence count is what records that it is still live.
	RefreshObservation(ctx context.Context, id, rationale, evidence string) error

	// MarkRegressed stamps the "auto-rolled-back due to regression" audit badge
	// (REGRESSED), recording the trip reason. It accepts BOTH APPLIED→REGRESSED
	// and ROLLED_BACK→REGRESSED (the latter is the transition the canary guard's
	// trip path actually executes: it Rollbacks — APPLIED→ROLLED_BACK — then
	// best-effort MarkRegressed — design §4.4/§4.5). Any other source status
	// returns ErrProposalNotRegressable. This is a BEST-EFFORT badge applied
	// after the durable canary trip record; its failure never re-opens a trip.
	MarkRegressed(ctx context.Context, id, reason string) error
}
