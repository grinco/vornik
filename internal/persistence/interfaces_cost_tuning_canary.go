package persistence

import (
	"context"
	"time"
)

// Cost/quality canary + regression auto-rollback guard (§D,
// https://docs.vornik.io). After a
// cost-tuning `config` proposal is APPLIED, the leader-gated CanaryGuardWorker opens
// a canary row on the affected (swarm, role), compares a pre-apply baseline quality
// window to a post-apply window, and on a statistically-guarded regression calls
// ApplyEngine.Rollback + marks the proposal REGRESSED + opens a cooldown. The canary
// row is the SINGLE source of trip-prevention + cooldown (crash-safe, design §4.5):
// the guard writes status/cooldown BEFORE the best-effort MarkRegressed badge, so a
// lost badge never causes a re-propose flip-flop (C2).

// Canary lifecycle statuses (design §4.3). `open` is the only non-terminal state and
// the only one that blocks the detector; `regressed` (with closed_at) doubles as the
// cooldown record. `insufficient_data`/`passed`/`superseded` do NOT block the
// detector (design §7).
const (
	CanaryStatusOpen             = "open"
	CanaryStatusPassed           = "passed"
	CanaryStatusRegressed        = "regressed"
	CanaryStatusInsufficientData = "insufficient_data"
	CanaryStatusSuperseded       = "superseded"
)

// CanaryA2Series is one blast-radius workflow's baseline A2 (task-tier) score,
// captured at open over [baseline_start, applied_at).
type CanaryA2Series struct {
	Rate       float64 `json:"rate"`
	Samples    int64   `json:"samples"`
	Sufficient bool    `json:"sufficient"`
}

// CanaryBaseline is the immutable pre-apply quality snapshot captured when the
// canary opens (design §4.3 `baseline` JSONB). A1 is the (swarm,role) step tier;
// EffCost is prompt-tokens per quality-passing step; A2 maps each blast-radius
// workflow id to its (swarm,workflow) task-tier baseline.
type CanaryBaseline struct {
	A1Rate       float64                   `json:"a1_rate"`
	A1Samples    int64                     `json:"a1_samples"`
	A1Sufficient bool                      `json:"a1_sufficient"`
	EffCost      float64                   `json:"effcost"`
	A2           map[string]CanaryA2Series `json:"a2,omitempty"`
}

// CostTuningCanary is one canary row (design §4.3). proposal_id is the PK — one
// canary per applied cost-tuning proposal.
type CostTuningCanary struct {
	ProposalID    string
	SwarmID       string
	Role          string
	Knob          string
	ProjectIDs    []string
	WorkflowIDs   []string
	AppliedAt     time.Time
	BaselineStart time.Time // clamped so a baseline never spans a prior tuning change (design D1/I3)
	WindowUntil   time.Time // applied_at + W
	Baseline      CanaryBaseline
	Status        string
	Reason        string // which tier + deltas (regressed) / note (insufficient_data)
	OpenedAt      time.Time
	ClosedAt      *time.Time // set on every terminal transition
}

// CostTuningCanaryRepository is the backend-agnostic canary store (postgres +
// sqlite), verified by repotest.RunCostTuningCanarySuite. All queries are cheap +
// indexed (partial index on status='open').
type CostTuningCanaryRepository interface {
	// Open inserts a new canary row with status=open. OpenedAt defaults to now
	// when unset. Fails if a row for the proposal already exists.
	Open(ctx context.Context, c *CostTuningCanary) error

	// GetByProposalID fetches a canary by its proposal id. Returns ErrNotFound
	// when absent (the discovery no-canary-row signal).
	GetByProposalID(ctx context.Context, proposalID string) (*CostTuningCanary, error)

	// ListOpen returns every status='open' canary (the EVALUATE work set),
	// oldest-opened first.
	ListOpen(ctx context.Context) ([]*CostTuningCanary, error)

	// Finalize transitions a canary to a terminal status in ONE row write,
	// stamping reason + closed_at. This single write is BOTH the trip record and
	// (for status=regressed) the cooldown record (design §4.5 C2). Idempotent by
	// construction: it only matches a currently-open row.
	Finalize(ctx context.Context, proposalID, status, reason string, closedAt time.Time) error

	// HasOpenForSwarmRole reports whether a status='open' canary exists on
	// (swarm, role) — the detector's open-canary skip (design §7).
	HasOpenForSwarmRole(ctx context.Context, swarmID, role string) (bool, error)

	// HasActiveCooldown reports whether a status='regressed' canary exists for
	// (swarm, role, knob) with closed_at > notBefore — the detector's cooldown
	// skip (design §7). notBefore is now - CooldownDuration.
	HasActiveCooldown(ctx context.Context, swarmID, role, knob string, notBefore time.Time) (bool, error)

	// LatestWindowUntil returns the greatest window_until among canaries on
	// (swarm, role) whose window_until <= before — the baseline clamp anchor
	// (design D1/I3). ok=false when there is no prior canary.
	LatestWindowUntil(ctx context.Context, swarmID, role string, before time.Time) (t time.Time, ok bool, err error)

	// CountPassedForKnob counts terminal status='passed' canaries for the exact
	// (swarm, role, knob) — the track-record half of the cost-auto-apply trust
	// signal (auto-apply design D1). A count >= K means K prior applies of this
	// exact knob were proven safe by their canary. `passed` is a terminal canary
	// status (no outgoing transitions), so the count only grows.
	CountPassedForKnob(ctx context.Context, swarmID, role, knob string) (int, error)

	// LastApplyActorForKnob returns the applied_by of the PROPOSAL behind the
	// MOST-RECENT canary (by canary applied_at) on (swarm, role, knob) — the M=1
	// re-seed half of the trust signal (auto-apply design D1). It is a join
	// cost_tuning_canaries ⋈ control_plane_proposals on proposal_id, ordered by
	// the canary's applied_at DESC; it is NOT "the last proposal by status". A
	// returned actor == CostAutoApplyActor means the knob was last auto-applied
	// (so auto-apply is blocked until a human re-seeds). ok=false when there is no
	// canary on the knob (never auto-applied; first apply stays human-gated).
	LastApplyActorForKnob(ctx context.Context, swarmID, role, knob string) (actor string, ok bool, err error)
}
