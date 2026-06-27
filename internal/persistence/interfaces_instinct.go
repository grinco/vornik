// Package persistence — continuous-learning instinct layer interfaces.
//
// The instinct layer (migrations 85/86) is a confidence-scored
// learned-pattern substrate mined from the audit spine by the
// leader-elected extraction worker in internal/instinct. This file
// defines the repository contract both backends (Postgres, SQLite)
// implement; the shared behaviour tests live in
// internal/persistence/repotest.
//
// See https://docs.vornik.io
package persistence

import (
	"context"
	"time"
)

// InstinctRepository persists instincts and their evidence /
// application provenance.
//
// The write path is driven by the extraction worker, which is
// designed to re-scan overlapping time windows; every mutating method
// is therefore idempotent:
//
//   - Upsert keys on (scope, project_id, trigger_key) and never
//     spawns a duplicate for a recurring situation.
//   - AddEvidence keys on (instinct_id, outcome_id) and is a no-op on
//     a re-seen outcome, so support/contradict counts can't drift.
//   - RecomputeConfidence derives support_count / contradict_count /
//     confidence from the evidence rows, so the counters always
//     reflect the evidence and never accumulate on replay.
type InstinctRepository interface {
	// Upsert inserts a new instinct or, when one already exists for
	// the same (scope, project_id, trigger_key), updates its mutable
	// fields (action, trigger_json, domain, source, distill_model)
	// and bumps last_seen_at + updated_at. It returns the resolved
	// row ID — the freshly generated ID for an insert, or the
	// existing row's ID for an update — so the caller can attach
	// evidence without a follow-up lookup.
	//
	// Upsert does NOT touch support_count / contradict_count /
	// confidence; those are owned by RecomputeConfidence.
	Upsert(ctx context.Context, in *Instinct) (id string, err error)

	// AddEvidence records one corroborating/contradicting outcome for
	// an instinct. Idempotent on (instinct_id, outcome_id): a re-seen
	// outcome is silently ignored. Returns true when a new evidence
	// row was actually inserted (the caller uses this to decide
	// whether a RecomputeConfidence is warranted).
	//
	// ev.Action (W6 per-action evidence partitioning) records the
	// instinct action this outcome corroborates. When the caller leaves
	// it empty, the implementation resolves it from the instinct's
	// current action so evidence is always action-tagged.
	AddEvidence(ctx context.Context, ev *InstinctEvidence) (inserted bool, err error)

	// RecordActionVersion appends one action-transition history row
	// (W6 versioning), snapshotting a displaced action's final state.
	// Append-only; never updates. Used by the worker when an instinct's
	// action changes (project-scoped churn or a cross-project "replace").
	RecordActionVersion(ctx context.Context, v *InstinctActionVersion) error

	// ListActionHistory returns an instinct's action-transition history,
	// newest first, capped at limit (<=0 → 100). Powers audit / rollback
	// surfaces. Empty when the instinct never changed action.
	ListActionHistory(ctx context.Context, instinctID string, limit int) ([]*InstinctActionVersion, error)

	// RecomputeConfidence recounts the instinct's support / contradict
	// evidence, recomputes the materialised confidence via the
	// supplied scorer, and persists all three plus the status
	// transition derived from the confidence model. The scorer is
	// injected (rather than hardcoded in SQL) so the Wilson +
	// decay math lives in one place in internal/instinct and the
	// repository stays storage-only.
	RecomputeConfidence(ctx context.Context, instinctID string, score InstinctScorer) error

	// Get returns one instinct by ID, or ErrNotFound.
	Get(ctx context.Context, id string) (*Instinct, error)

	// List returns instincts matching the filter, highest confidence
	// first. Powers the API / CLI / UI surfaces.
	List(ctx context.Context, filter InstinctFilter) ([]*Instinct, error)

	// CountActiveProjects returns the number of distinct project_ids
	// holding an instinct with the given trigger_key whose status is
	// active or promoted. It backs the active→promoted (cross-project)
	// transition: the worker passes the result as
	// InstinctScoreInput.ProjectCount so the scorer can decide whether a
	// trigger has corroborated across enough projects to be promoted to
	// a global instinct. Deterministic and read-only.
	CountActiveProjects(ctx context.Context, triggerKey string) (int, error)

	// CountByDomainStatus returns the live instinct population grouped by
	// (domain, status) — one row per non-empty bucket. Backs the
	// vornik_instinct_total gauge, which the extraction worker refreshes
	// each tick. Read-only and cheap (a single GROUP BY over a small
	// table); empty buckets are simply absent from the result.
	CountByDomainStatus(ctx context.Context) ([]InstinctDomainStatusCount, error)

	// Retire flips an instinct to status='retired'. Used by the
	// operator retire endpoint and by the confidence model when an
	// instinct decays below the retire floor. Returns ErrNotFound
	// when no row matches.
	Retire(ctx context.Context, id string) error

	// RecordApplication appends one application/feedback row. No
	// consumer writes this in slice 1; the method lands with the
	// schema so the consumer slices build on a stable contract.
	RecordApplication(ctx context.Context, app *InstinctApplication) error

	// ListApplications returns application rows for one instinct,
	// newest first.
	ListApplications(ctx context.Context, instinctID string, limit int) ([]*InstinctApplication, error)

	// ListPendingRecoveryApplications returns lead_recovery applications
	// that were surfaced but not yet resolved — rows with
	// surface='lead_recovery' AND result='ignored' AND execution_id != ''
	// — oldest first (applied_at ASC), capped at limit. limit <= 0 means
	// 500. These are the rows the RecoveryResolver re-evaluates each tick
	// (slice 7): it matches each against the step's later outcome and
	// flips it to succeeded/failed in place via ResolveApplication.
	ListPendingRecoveryApplications(ctx context.Context, limit int) ([]*InstinctApplication, error)

	// ResolveApplication flips a surfaced-but-unresolved application
	// in place: UPDATE result=result WHERE id=id AND result='ignored'.
	// It is guarded on result='ignored' so a row already resolved by a
	// prior tick (or a concurrent resolver) is never re-flipped. Returns
	// ErrNotFound when no ignored row matches (already resolved or
	// missing). There is no updated_at column on instinct_applications;
	// only result is written.
	ResolveApplication(ctx context.Context, id string, result string) error

	// ListApplicationCounts returns the per-instinct application-feedback
	// tally for a batch of instinct IDs, in a single GROUP BY query. The
	// result buckets are: Succeeded ('succeeded'), Failed ('failed' +
	// 'rejected' collapsed), Ignored ('ignored'); 'accepted' is excluded.
	// Instincts with no application rows are simply absent from the map.
	// An empty/nil instinctIDs slice returns an empty map and runs no SQL
	// (an IN () clause is a syntax error on both backends).
	ListApplicationCounts(ctx context.Context, instinctIDs []string) (map[string]*InstinctApplicationCounts, error)
}

// InstinctScorer computes the materialised confidence and the derived
// status for an instinct given its current support/contradict counts.
// Implemented by internal/instinct so the confidence math has a single
// home; the repository calls it inside RecomputeConfidence.
//
// The scorer receives the existing status so it can honour
// transition rules (e.g. a promoted instinct shouldn't silently drop
// back to candidate on one stale tick) and returns the next status.
type InstinctScorer interface {
	// Score returns the confidence in [0,1] and the next status given
	// the support/contradict counts and the instinct's last
	// corroboration time (which drives recency decay) and current
	// status.
	Score(in InstinctScoreInput) (confidence float64, status string)
}

// InstinctScoreInput is the snapshot passed to an InstinctScorer.
type InstinctScoreInput struct {
	SupportCount    int
	ContradictCount int
	LastSeenAt      time.Time
	// CreatedAt is the instinct row's creation time. It backs the W1
	// evidence-freshness promotion gate: an instinct mined from HISTORICAL
	// evidence has an old LastSeenAt at creation and would otherwise be gated
	// out immediately, so the scorer grants a creation grace period (keyed on
	// CreatedAt) before the staleness gate can bite. Zero → treated as "no
	// grace" (gate keys on LastSeenAt alone). The gate itself is off unless
	// Thresholds.MaxEvidenceAge is set, so a zero CreatedAt is harmless in
	// production until an operator opts in.
	CreatedAt     time.Time
	CurrentStatus string
	// ProjectCount is the number of distinct projects in which the
	// same trigger_key is currently active; the scorer uses it for
	// the active→promoted transition. Zero/one for slice 1 (no
	// cross-project promotion wired yet).
	ProjectCount int
	// Distilled marks an LLM-generalised instinct (distill_model set).
	// Its support evidence is failure-recurrence, not an observed fix,
	// so the scorer caps it at 'candidate' on recurrence alone — it may
	// only graduate to active once it has accrued AppSucceeded
	// application-success feedback (see below).
	Distilled bool

	// AppSucceeded / AppFailed / AppIgnored are the application-feedback
	// tallies (slice 7): how often this instinct, once surfaced, led to a
	// successful outcome vs. a failed one vs. was surfaced-but-unresolved.
	// They are a SEPARATE evidence class from the support/contradict
	// recurrence counts. The scorer grades only AppSucceeded vs AppFailed
	// into a multiplicative "lift" that can erode confidence when surfacing
	// didn't help, and gates a distilled instinct's graduation on
	// AppSucceeded. AppIgnored is recorded for observability but carries no
	// efficacy signal (it is logged at surface time, before any outcome) so
	// the scorer excludes it from the lift. All zero (no surfacing yet) →
	// no effect, identical to pre-slice-7 scoring. Aggregated from
	// instinct_applications in RecomputeConfidence.
	AppSucceeded int
	AppFailed    int
	AppIgnored   int
}
