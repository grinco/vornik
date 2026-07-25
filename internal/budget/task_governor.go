package budget

import (
	"context"
	"math"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// ValidTaskBudgetUSD reports whether a caller-supplied task ceiling can be
// persisted and compared safely. NaN disables ordered comparisons and
// infinities make the hard stop unreachable, so both are invalid.
func ValidTaskBudgetUSD(v float64) bool {
	return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0)
}

// DefaultTaskSoftFraction is the fraction of a task's effective budget at which
// the governor emits a warn-only soft-breach signal when the project doesn't
// configure ProjectBudget.TaskSoftFraction. See LLD 2026-07-24 §3.2.
const DefaultTaskSoftFraction = 0.80

// TaskBudgetTier is the coarse governor verdict for a task's cumulative
// lifetime spend against its effective per-task budget.
type TaskBudgetTier int

const (
	// TierOK means spend is below the soft threshold — dispatch normally.
	// A task with no effective budget (0 = disabled) is ALWAYS TierOK.
	TierOK TaskBudgetTier = iota
	// TierSoft means spend has reached the soft fraction but not the budget.
	// Warn-only (Notifier + metric + log); the step still dispatches.
	TierSoft
	// TierHard means spend has reached/exceeded the budget. The step must NOT
	// dispatch — the task parks AWAITING_INPUT for an operator decision.
	TierHard
)

// String renders the tier as the stable metric label value ("ok"/"soft"/"hard").
func (t TaskBudgetTier) String() string {
	switch t {
	case TierSoft:
		return "soft"
	case TierHard:
		return "hard"
	default:
		return "ok"
	}
}

// TaskBudgetDecision is the governor's verdict for one step-boundary
// evaluation. BudgetUSD == 0 means the governor is disabled for this task
// (Tier is then always TierOK).
type TaskBudgetDecision struct {
	SpentUSD  float64
	BudgetUSD float64
	// Fraction is SpentUSD/BudgetUSD, or 0 when the budget is disabled.
	Fraction float64
	Tier     TaskBudgetTier
}

// TaskSpendRepo is the narrow read surface the governor needs — the cumulative
// lifetime spend of one task across all its executions. The production
// persistence.TaskLLMUsageRepository satisfies it; a narrow type keeps test
// doubles tiny.
type TaskSpendRepo interface {
	SumCostByTask(ctx context.Context, taskID string) (float64, error)
}

// EffectiveTaskBudgetUSD resolves the per-task lifetime budget ceiling per the
// LLD §3.1 ladder:
//
//	override (task.budget_usd, non-NULL & > 0)  ⇒ that value
//	else project.Budget.DefaultTaskBudgetUSD    ⇒ project default
//	else 0                                       ⇒ governor + per-task forecast DISABLED
//
// A nil project yields 0 (disabled). The stored override is guaranteed
// NULL-or-positive by the write layer, so the ">0" test is belt-and-braces.
func EffectiveTaskBudgetUSD(project *registry.Project, override *float64) float64 {
	if override != nil && *override > 0 {
		return *override
	}
	if project == nil {
		return 0
	}
	return project.Budget.DefaultTaskBudgetUSD
}

// resolveTaskSoftFraction returns the configured soft fraction, defaulting to
// DefaultTaskSoftFraction when unset (<= 0) or out of range (> 1).
func resolveTaskSoftFraction(project *registry.Project) float64 {
	if project == nil {
		return DefaultTaskSoftFraction
	}
	f := project.Budget.TaskSoftFraction
	if f <= 0 || f > 1 {
		return DefaultTaskSoftFraction
	}
	return f
}

// TaskGovernor evaluates a task's cumulative lifetime spend against its
// effective per-task budget at step boundaries. It holds no state beyond the
// spend repo — dedup/notification are the caller's concern (§3.5).
type TaskGovernor struct {
	repo TaskSpendRepo
}

// NewTaskGovernor builds a governor over the given spend repo.
func NewTaskGovernor(repo TaskSpendRepo) *TaskGovernor {
	return &TaskGovernor{repo: repo}
}

// Check reads the task's cumulative lifetime spend and compares it against the
// effective budget, returning a tiered decision.
//
//   - Effective budget 0 (disabled) ⇒ TierOK unconditionally, no repo read.
//   - Fraction >= 1.0 ⇒ TierHard.
//   - Fraction >= softFraction ⇒ TierSoft.
//   - otherwise TierOK.
//
// A repo error is surfaced verbatim; the step-boundary caller FAIL-CLOSES on it
// (parks AWAITING_INPUT rather than letting a runaway slip past — §3.3).
func (g *TaskGovernor) Check(ctx context.Context, project *registry.Project, task *persistence.Task) (TaskBudgetDecision, error) {
	if g == nil || g.repo == nil || task == nil {
		return TaskBudgetDecision{Tier: TierOK}, nil
	}
	budgetUSD := EffectiveTaskBudgetUSD(project, task.BudgetUSD)
	if budgetUSD <= 0 {
		// Disabled — today's behaviour, no spend read.
		return TaskBudgetDecision{Tier: TierOK}, nil
	}
	spent, err := g.repo.SumCostByTask(ctx, task.ID)
	if err != nil {
		return TaskBudgetDecision{BudgetUSD: budgetUSD}, err
	}
	d := TaskBudgetDecision{
		SpentUSD:  spent,
		BudgetUSD: budgetUSD,
		Fraction:  spent / budgetUSD,
	}
	switch {
	case d.Fraction >= 1.0:
		d.Tier = TierHard
	case d.Fraction >= resolveTaskSoftFraction(project):
		d.Tier = TierSoft
	default:
		d.Tier = TierOK
	}
	return d, nil
}

// ClampTaskBudget applies the optional project MaxTaskBudgetUSD security ceiling
// to a requested per-task budget: min(requested, max) when the project sets a
// positive max, else requested unchanged (LLD §3.1). A nil project or
// non-positive max leaves the request untouched.
func ClampTaskBudget(project *registry.Project, requested float64) float64 {
	if project == nil {
		return requested
	}
	maxUSD := project.Budget.MaxTaskBudgetUSD
	if maxUSD > 0 && requested > maxUSD {
		return maxUSD
	}
	return requested
}
