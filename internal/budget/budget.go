// Package budget enforces per-project LLM spend caps. It's called from three
// gates — autonomy evaluation, the dispatcher create_task tool, and the API
// POST /tasks handler — which all read the same TaskLLMUsage aggregates and
// apply the same policy so projects can't dodge budgets by routing through
// an alternate entry point.
package budget

import (
	"context"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Decision is the outcome of a budget check.
type Decision struct {
	// Blocked is true when a hard cap is exceeded. Callers should refuse work.
	Blocked bool
	// SoftBreached is true when a soft cap is exceeded but the hard cap is not.
	// Callers should still allow the work but log/emit a warning.
	SoftBreached bool
	// Reason is a human-readable explanation, safe to surface in API errors
	// and chat responses.
	Reason string
	// DailyUSD is the project's current daily spend as seen at check time.
	DailyUSD float64
	// MonthlyUSD is the project's current month-to-date spend.
	MonthlyUSD float64
}

// Repo is the subset of persistence.TaskLLMUsageRepository that budget checks
// need. Narrowing it here keeps test doubles simple and avoids the import
// graph of the full repo.
type Repo interface {
	SumCostByProject(ctx context.Context, projectID string, since, until time.Time) (float64, error)
}

// Notifier is the sink for budget-breach alerts. The three gates
// (api.CreateTask, autonomy.tick, dispatcher.create_task) all call
// Notify when a Check returns a Blocked or SoftBreached decision;
// the implementation is responsible for deduplicating so operators
// don't get hit with one alert per blocked task in the same period.
// Level is "soft" or "hard"; period is "daily" or "monthly".
//
// A nil Notifier is a valid configuration — callers no-op when
// unset, same pattern as CompletionNotifier on the executor.
type Notifier interface {
	NotifyBudgetBreach(ctx context.Context, projectID, level, period string, decision Decision)
}

// Period returns which envelope was tripped by the decision
// ("daily", "monthly") along with the level ("hard", "soft"). Returns
// "", "" when the decision didn't trip anything — callers check
// Blocked / SoftBreached before calling this. The Reason string is
// the canonical signal of which cap fired; we match on the literal
// prefix written by Check above.
func (d Decision) Period() (period, level string) {
	switch {
	case d.Blocked && strings.Contains(d.Reason, "monthly"):
		return "monthly", "hard"
	case d.Blocked:
		return "daily", "hard"
	case d.SoftBreached && strings.Contains(d.Reason, "monthly"):
		return "monthly", "soft"
	case d.SoftBreached:
		return "daily", "soft"
	}
	return "", ""
}

// Check reads current daily + month-to-date spend and compares them to the
// project's budget. Zero budget values mean "no cap on that dimension" and
// are skipped. A missing repo short-circuits to an unblocked decision —
// budgets require usage data; absent that, we don't guess.
func Check(ctx context.Context, repo Repo, project *registry.Project, now time.Time) (Decision, error) {
	if project == nil || repo == nil {
		return Decision{}, nil
	}
	b := project.Budget
	// Fast path: nothing configured.
	if b.DailySoftUSD == 0 && b.DailyHardUSD == 0 && b.MonthlySoftUSD == 0 && b.MonthlyHardUSD == 0 {
		return Decision{}, nil
	}

	if now.IsZero() {
		now = time.Now().UTC()
	}
	dayStart, monthStart := windowStarts(b.Timezone, now)

	daily, err := repo.SumCostByProject(ctx, project.ID, dayStart, time.Time{})
	if err != nil {
		return Decision{}, fmt.Errorf("sum daily spend: %w", err)
	}
	monthly, err := repo.SumCostByProject(ctx, project.ID, monthStart, time.Time{})
	if err != nil {
		return Decision{}, fmt.Errorf("sum monthly spend: %w", err)
	}

	d := Decision{DailyUSD: daily, MonthlyUSD: monthly}

	if b.DailyHardUSD > 0 && daily >= b.DailyHardUSD {
		d.Blocked = true
		d.Reason = fmt.Sprintf("daily budget exceeded: $%.2f spent of $%.2f hard cap", daily, b.DailyHardUSD)
		return d, nil
	}
	if b.MonthlyHardUSD > 0 && monthly >= b.MonthlyHardUSD {
		d.Blocked = true
		d.Reason = fmt.Sprintf("monthly budget exceeded: $%.2f spent of $%.2f hard cap", monthly, b.MonthlyHardUSD)
		return d, nil
	}
	if b.DailySoftUSD > 0 && daily >= b.DailySoftUSD {
		d.SoftBreached = true
		d.Reason = fmt.Sprintf("daily soft budget breached: $%.2f spent of $%.2f soft cap", daily, b.DailySoftUSD)
		return d, nil
	}
	if b.MonthlySoftUSD > 0 && monthly >= b.MonthlySoftUSD {
		d.SoftBreached = true
		d.Reason = fmt.Sprintf("monthly soft budget breached: $%.2f spent of $%.2f soft cap", monthly, b.MonthlySoftUSD)
		return d, nil
	}
	return d, nil
}

// windowStarts returns the UTC instants of the current day-start and
// month-start, computed in the project's configured timezone. This matters
// most at the edges: an Europe/Prague midnight reset happens at 22:00 or
// 23:00 UTC, not at 00:00 UTC.
func windowStarts(tz string, now time.Time) (dayStart, monthStart time.Time) {
	loc := resolveLocation(tz)
	nowLocal := now.In(loc)
	dayStart = time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, loc).UTC()
	monthStart = time.Date(nowLocal.Year(), nowLocal.Month(), 1, 0, 0, 0, 0, loc).UTC()
	return dayStart, monthStart
}

// DefaultReservationEstimateUSD is the per-task budget headroom claimed at
// admission when a project doesn't set ReservationEstimateUSD. Sized as a
// conservative single-task LLM cost — over-estimating only refuses sooner
// (safe-side); the reservation is dropped when the task settles and the real
// cost is what counts.
const DefaultReservationEstimateUSD = 1.00

// ReservationRepo is the subset of persistence.BudgetReservationRepository
// the admission + settlement paths need. Narrowed here so callers and test
// doubles don't pull the full repo surface.
type ReservationRepo interface {
	Reserve(ctx context.Context, req persistence.ReserveRequest) (persistence.ReserveResult, error)
	SettleByTask(ctx context.Context, taskID string, now time.Time) (int64, error)
}

// Reserve performs the atomic HARD-cap admission for one task: it reads the
// project's committed daily + monthly spend, then asks the reservation ledger
// to (atomically, per-project-serialized) sum in-flight reservations and
// insert a new one iff committed + reserved + estimate stays under every
// configured hard cap. Returns Decision.Blocked=true (no reservation written)
// when a cap would be crossed.
//
// Scope is the hard cap ONLY — soft caps stay advisory and are handled by the
// upstream Check. A nil repo/project, an empty taskID, or a project with no
// hard cap short-circuits to an unblocked, un-reserved decision.
//
// IMPORTANT: callers MUST fail OPEN on a non-nil error — a reservation-ledger
// problem must never block a project's legitimate work. The committed-spend
// Check remains the backstop.
func Reserve(ctx context.Context, resRepo ReservationRepo, usageRepo Repo, project *registry.Project, taskID string, now time.Time) (Decision, error) {
	if project == nil || resRepo == nil || usageRepo == nil || taskID == "" {
		return Decision{}, nil
	}
	b := project.Budget
	if b.DailyHardUSD == 0 && b.MonthlyHardUSD == 0 {
		return Decision{}, nil // no hard cap configured → nothing to reserve against
	}
	estimate := b.ReservationEstimateUSD
	if estimate <= 0 {
		estimate = DefaultReservationEstimateUSD
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	dayStart, monthStart := windowStarts(b.Timezone, now)
	daily, err := usageRepo.SumCostByProject(ctx, project.ID, dayStart, time.Time{})
	if err != nil {
		return Decision{}, fmt.Errorf("sum daily spend: %w", err)
	}
	monthly, err := usageRepo.SumCostByProject(ctx, project.ID, monthStart, time.Time{})
	if err != nil {
		return Decision{}, fmt.Errorf("sum monthly spend: %w", err)
	}
	out, err := resRepo.Reserve(ctx, persistence.ReserveRequest{
		ProjectID:           project.ID,
		TaskID:              taskID,
		EstimateUSD:         estimate,
		DailyCommittedUSD:   daily,
		DailyHardUSD:        b.DailyHardUSD,
		MonthlyCommittedUSD: monthly,
		MonthlyHardUSD:      b.MonthlyHardUSD,
		Now:                 now,
	})
	if err != nil {
		return Decision{}, fmt.Errorf("reserve budget: %w", err)
	}
	d := Decision{DailyUSD: daily, MonthlyUSD: monthly}
	if out.Blocked {
		d.Blocked = true
		if out.Period == "monthly" {
			d.Reason = fmt.Sprintf("monthly budget would be exceeded: $%.2f committed + $%.2f reserved + $%.2f estimate over $%.2f hard cap", monthly, out.ReservedUSD, estimate, b.MonthlyHardUSD)
		} else {
			d.Reason = fmt.Sprintf("daily budget would be exceeded: $%.2f committed + $%.2f reserved + $%.2f estimate over $%.2f hard cap", daily, out.ReservedUSD, estimate, b.DailyHardUSD)
		}
	}
	return d, nil
}

// resolveLocation parses an IANA timezone name, falling back to UTC if the
// name is empty or invalid. A warning for invalid zones is emitted by the
// caller — budget.Check logs it inline to avoid a dedicated error channel
// for what is effectively a config-typo case.
func resolveLocation(tz string) *time.Location {
	if tz == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.UTC
	}
	return loc
}

// Ensure the persistence repo satisfies our narrowed interface. Compile-time
// guard; purely for documentation.
var _ Repo = (persistence.TaskLLMUsageRepository)(nil)
