package persistence

import (
	"context"
	"time"
)

// Workflow-healing trigger surface (Autonomy Black Box Phase B).
// The detector sweeper writes rows here when a workflow's last-24h
// roll-up regresses beyond threshold against the prior-7-day
// baseline; operators triage from /ui/admin/blackbox (Triggers tab).
// See https://docs.vornik.io §
// Self-Healing Phase A Detector.

// HealingTriggerClass enumerates the metric classes the detector
// can fire on. The Phase A scope ships failure_rate_spike +
// cost_regression; later phases can extend without a schema
// change (status CHECK doesn't constrain class).
type HealingTriggerClass string

const (
	// HealingTriggerFailureRateSpike fires when the workflow's
	// failure-rate over the comparison window exceeds the
	// baseline by 25%+ relative (default threshold).
	HealingTriggerFailureRateSpike HealingTriggerClass = "failure_rate_spike"
	// HealingTriggerCostRegression fires when the avg cost per
	// run rises by 40%+ relative.
	HealingTriggerCostRegression HealingTriggerClass = "cost_regression"
)

// HealingTriggerStatus is the operator-facing lifecycle column.
type HealingTriggerStatus string

const (
	// HealingTriggerStatusOpen is the initial state — the
	// detector wrote the row; the operator hasn't acted yet.
	// Subject to the partial unique index that keeps a
	// (project, workflow, class) at most-one-open at a time.
	HealingTriggerStatusOpen HealingTriggerStatus = "open"
	// HealingTriggerStatusDismissed means the operator
	// reviewed and rejected — the regression is expected
	// (deploy under load, A/B test, intentional cost lift).
	HealingTriggerStatusDismissed HealingTriggerStatus = "dismissed"
	// HealingTriggerStatusGeneratedCandidate means the operator
	// pushed the trigger's evidence into the memetic architect.
	// proposal_id stamps the resulting proposal row.
	HealingTriggerStatusGeneratedCandidate HealingTriggerStatus = "generated_candidate"
)

// HealingTrigger is one workflow_healing_triggers row. Same field
// shape the migration declares; the API/UI surfaces project
// subsets via DTOs.
type HealingTrigger struct {
	ID                   string
	ProjectID            string
	WorkflowID           string
	TriggerClass         HealingTriggerClass
	BaselineStart        time.Time
	BaselineEnd          time.Time
	ComparisonStart      time.Time
	ComparisonEnd        time.Time
	MetricName           string
	BaselineValue        float64
	ComparisonValue      float64
	ThresholdValue       float64
	EvidenceExecutionIDs []string
	Status               HealingTriggerStatus
	CreatedAt            time.Time
	ResolvedAt           *time.Time
	ProposalID           string // populated when Status=generated_candidate
}

// IsTerminal reports whether the trigger is settled. The
// detector won't dedupe against terminal rows (a fresh
// regression after a dismissal opens a new trigger).
func (s HealingTriggerStatus) IsTerminal() bool {
	return s == HealingTriggerStatusDismissed || s == HealingTriggerStatusGeneratedCandidate
}

// HealingTriggerListFilter drives the operator-facing list query.
type HealingTriggerListFilter struct {
	ProjectID    string
	WorkflowID   string
	Status       HealingTriggerStatus
	TriggerClass HealingTriggerClass
	PageSize     int
}

// WorkflowHealingTriggerRepository persists trigger rows.
// Implementations must enforce the partial unique index — a
// duplicate-class open trigger insert returns ErrAlreadyExists
// (or silently noops, see Insert contract).
type WorkflowHealingTriggerRepository interface {
	// Insert writes a new open trigger. If a (project,
	// workflow, class) row is already open, returns
	// ErrAlreadyExists — callers (detector) treat that as
	// "already triaging, skip".
	Insert(ctx context.Context, t *HealingTrigger) error

	// Get returns one row by id; ErrNotFound when missing.
	Get(ctx context.Context, id string) (*HealingTrigger, error)

	// List returns rows matching the filter, newest first.
	List(ctx context.Context, filter HealingTriggerListFilter) ([]*HealingTrigger, error)

	// Dismiss flips status=dismissed + stamps resolved_at on a
	// row in open state. Returns ErrNotFound on a missing or
	// already-terminal row.
	Dismiss(ctx context.Context, id string) error

	// MarkGenerated stamps status=generated_candidate +
	// resolved_at + proposal_id on an open row. Same
	// not-found contract as Dismiss.
	MarkGenerated(ctx context.Context, id, proposalID string) error
}

// ErrAlreadyExists is returned by Insert when the partial
// unique index would be violated — i.e. the (project, workflow,
// class) tuple already has an open trigger. Wrapping sentinel
// so callers can errors.Is and route cleanly.
var ErrAlreadyExists = errorAlreadyExists{}

type errorAlreadyExists struct{}

func (errorAlreadyExists) Error() string { return "row already exists" }

// HealingTriggerOverride is one workflow_healing_overrides row.
// Captures the operator's preference for a specific (project,
// workflow, trigger_class) tuple: a custom threshold (replaces the
// detector's default) and/or a mute window (detector skips writing
// new triggers while MutedUntil > now). Both fields are optional;
// a row with neither set is meaningless but the schema permits it.
//
// Migration 81 adds the table. The detector reads the row before
// evaluating each (workflow, class) pair so operators can:
//   - Quiet a noisy detector ("we know failure_rate is 25%; the
//     real threshold for this workflow should be 50%")
//   - Snooze during a known incident ("we're rolling forward
//     anyway; don't open new triggers for 24h")
type HealingTriggerOverride struct {
	ProjectID    string
	WorkflowID   string
	TriggerClass HealingTriggerClass
	// ThresholdOverride replaces the detector's default
	// relative-delta threshold (0.25 for failure_rate, 0.40 for
	// cost) when non-nil. Stored as a relative delta, NOT a
	// percentage — operators set "0.50" via the UI knowing it
	// means "50% lift". Nil = use detector default.
	ThresholdOverride *float64
	// MutedUntil silences the detector for the (project, workflow,
	// class) tuple until the given UTC instant. Nil = not muted.
	// Past timestamps are treated as not-muted (the detector
	// doesn't proactively clear them; the UI shows them as
	// "expired").
	MutedUntil *time.Time
	// Notes is operator-facing free text — why the override
	// exists. Surfaces in the audit trail.
	Notes string
	// CreatedBy / UpdatedAt stamp the last operator who touched
	// the row. UpdatedAt is bumped on every Upsert.
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsMutedAt reports whether the override mutes the detector at
// the given instant. Past MutedUntil values fail this check so a
// stale mute doesn't keep the detector quiet indefinitely.
func (o *HealingTriggerOverride) IsMutedAt(t time.Time) bool {
	if o == nil || o.MutedUntil == nil {
		return false
	}
	return o.MutedUntil.After(t)
}

// HealingTriggerOverrideRepository persists per-(project, workflow,
// class) detector overrides. Postgres-only — same Phase B
// discipline as the trigger ledger; the SQLite branch returns a
// stub that signals "unsupported".
type HealingTriggerOverrideRepository interface {
	// Upsert creates or updates the row for the (project,
	// workflow, class) tuple. CreatedBy / Notes / fields are
	// overwritten on each call; CreatedAt is preserved on
	// update.
	Upsert(ctx context.Context, o *HealingTriggerOverride) error
	// Get returns the row by tuple key. Returns ErrNotFound
	// when no override exists.
	Get(ctx context.Context, projectID, workflowID string, class HealingTriggerClass) (*HealingTriggerOverride, error)
	// List returns all overrides, newest-updated first. Used by
	// the admin index page. PageSize caps the result; 0 = default.
	List(ctx context.Context, pageSize int) ([]*HealingTriggerOverride, error)
	// Delete removes the row. Idempotent: missing rows are not an
	// error (returns nil) so a stale UI click doesn't 404.
	Delete(ctx context.Context, projectID, workflowID string, class HealingTriggerClass) error
}
