// Package persistence — budget-reservation domain interface.
//
// The budget-reservation ledger (trading-hardening §1) closes the
// read-then-spend TOCTOU in the LLM hard-cap admission check: a
// reservation is an in-flight claim on budget, inserted atomically at
// task admission and settled when the task terminates. The admission
// transaction sums unsettled reservations together with committed spend
// so N concurrent admissions can't all observe the same headroom and
// overshoot the hard cap.
package persistence

import (
	"context"
	"time"
)

// BudgetReservation is one in-flight claim on a project's LLM budget.
// SettledAt is nil while the reservation counts against the cap; set when
// the task terminates (or the watchdog reaps it).
type BudgetReservation struct {
	ID           string
	ProjectID    string
	TaskID       string
	EstimatedUSD float64
	ReservedAt   time.Time
	SettledAt    *time.Time
}

// ReserveRequest carries everything the atomic admission needs. The
// committed sums are read by the caller (budget.Reserve) from
// task_llm_usage and passed in as a snapshot — a slight cross-table TOCTOU
// on committed is acceptable because it only ever errs safe-side
// (a settling reservation briefly double-counts as both reserved and
// committed, refusing sooner, never later). A hard-cap field of 0 means
// "no cap on that dimension" and is skipped.
type ReserveRequest struct {
	ProjectID           string
	TaskID              string
	EstimateUSD         float64
	DailyCommittedUSD   float64
	DailyHardUSD        float64
	MonthlyCommittedUSD float64
	MonthlyHardUSD      float64
	Now                 time.Time
}

// ReserveResult reports the outcome. Reserved=true means a row was
// inserted (the caller owns settling it). Blocked=true means a hard cap
// would be exceeded and NO row was inserted; Period names which cap
// ("daily" or "monthly"). ReservedUSD is the unsettled reserved total the
// admission observed (excluding this estimate) — for logging/observability.
type ReserveResult struct {
	Reserved    bool
	Blocked     bool
	Period      string
	ReservedUSD float64
}

// BudgetReservationRepository is the ledger's persistence port.
type BudgetReservationRepository interface {
	// Reserve atomically sums the project's unsettled reservations and, if
	// committed + reserved + estimate stays under every configured hard cap,
	// inserts a new reservation. Serialized per-project (Postgres advisory
	// lock) so concurrent admissions can't both claim the same headroom.
	Reserve(ctx context.Context, req ReserveRequest) (ReserveResult, error)

	// SettleByTask marks every unsettled reservation for a task as settled.
	// Idempotent — a second call (or a sweep that already settled it) affects
	// zero rows. Returns rows settled.
	SettleByTask(ctx context.Context, taskID string, now time.Time) (int64, error)

	// SweepTerminalAndStale settles unsettled reservations whose task has
	// reached a terminal state (settlement missed, e.g. a crash between the
	// terminal transition and SettleByTask) AND any unsettled reservation
	// older than staleCutoff (a task row that never materialised, or a stuck
	// task). The stale bound is the backstop that guarantees a leaked
	// reservation can't block a project forever. Returns rows settled.
	SweepTerminalAndStale(ctx context.Context, staleCutoff, now time.Time) (int64, error)

	// UnsettledSumByProject returns the current unsettled reserved total for a
	// project (read-only — observability and non-admission checks).
	UnsettledSumByProject(ctx context.Context, projectID string) (float64, error)
}
