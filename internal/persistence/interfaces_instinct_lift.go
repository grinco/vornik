// Package persistence — instinct true-lift measurement layer interfaces.
//
// The lift layer (migration 128) measures whether surfacing an instinct
// actually helps: applied-success-rate minus a matched concurrent
// complement baseline, per domain (Recovery / Budget / Architect). This
// file defines the repository contract both backends (Postgres, SQLite)
// implement; the shared behaviour tests live in
// internal/persistence/repotest.
//
// See https://docs.vornik.io
package persistence

import (
	"context"
	"time"
)

// Lift verdict values (2026-07-19-instinct-lift-measurement-design.md §4.2).
const (
	LiftVerdictNotMeasurable = "not_measurable"
	LiftVerdictUnknown       = "unknown"
	LiftVerdictHelping       = "helping"
	LiftVerdictLowLift       = "low_lift"
)

// LiftOutcomes is one side (treatment or baseline) of a lift measurement.
type LiftOutcomes struct {
	N         int
	Successes int
}

// SuccRate returns Successes/N, or 0 when N==0.
func (o LiftOutcomes) SuccRate() float64 {
	if o.N == 0 {
		return 0
	}
	return float64(o.Successes) / float64(o.N)
}

// InstinctLiftSnapshot is one row of instinct_lift — the LATEST lift
// measurement for one instinct (snapshot, not event log).
type InstinctLiftSnapshot struct {
	InstinctID    string    `json:"instinct_id"`
	Domain        string    `json:"domain"`
	Lift          float64   `json:"lift"`
	TreatmentN    int       `json:"treatment_n"`
	TreatmentSucc int       `json:"treatment_succ"`
	BaselineN     int       `json:"baseline_n"`
	BaselineSucc  int       `json:"baseline_succ"`
	StdError      float64   `json:"std_error"`
	Verdict       string    `json:"verdict"`
	ComputedAt    time.Time `json:"computed_at"`
}

// InstinctLiftRepository persists lift snapshots and runs the per-domain
// treatment/complement outcome queries over the audit spine. Read-mostly:
// the only write is the snapshot upsert.
type InstinctLiftRepository interface {
	// UpsertLiftSnapshot writes/replaces the latest snapshot (PK instinct_id).
	UpsertLiftSnapshot(ctx context.Context, s *InstinctLiftSnapshot) error
	// GetLiftSnapshots batch-fetches snapshots for the given instinct IDs.
	// Missing IDs are simply absent from the map. Empty input → empty map, no SQL.
	GetLiftSnapshots(ctx context.Context, instinctIDs []string) (map[string]*InstinctLiftSnapshot, error)

	// Recovery domain (surface lead_recovery). Applied = resolved
	// instinct_applications rows; success = result 'succeeded'.
	RecoveryAppliedOutcomes(ctx context.Context, instinctID string, since time.Time) (LiftOutcomes, error)
	// Complement = failed steps in the same (project, role, error_class)
	// context with no application of THIS instinct on that (execution, step);
	// success = a later 'ok' outcome on the same (execution, step).
	// projectID "" (a global-scope instinct) drops the project constraint.
	RecoveryComplementOutcomes(ctx context.Context, instinctID, projectID, role, errorClass string, since time.Time) (LiftOutcomes, error)

	// Budget domain (surface tool_budget). Applied = distinct terminal tasks
	// with an application of this instinct; success = task COMPLETED.
	BudgetAppliedOutcomes(ctx context.Context, instinctID string, since time.Time) (LiftOutcomes, error)
	// Complement = distinct terminal tasks seen in the same (project, role)
	// step-outcome context with NO tool_budget application of this instinct.
	BudgetComplementOutcomes(ctx context.Context, instinctID, projectID, role string, since time.Time) (LiftOutcomes, error)

	// Architect domain (surface architect_evidence). Applied = DECIDED
	// workflow proposals whose instinct_ids contains this instinct;
	// success = status != 'rejected'. SQLite: always zero (no architect).
	ArchitectAppliedOutcomes(ctx context.Context, instinctID string, since time.Time) (LiftOutcomes, error)
	// Complement = decided proposals for the SAME workflow_ids and kinds as
	// the treatment set, whose instinct_ids does NOT contain this instinct.
	ArchitectComplementOutcomes(ctx context.Context, instinctID string, since time.Time) (LiftOutcomes, error)
}
