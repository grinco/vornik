package budget

import (
	"context"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/registry"
)

// Forecast estimates the USD cost of a task before it's created. The
// forecast is the sum of per-step estimates: each agent / plan step
// in the workflow contributes its predicted spend, gates and
// approval steps contribute zero. Per-step spend comes from
// historical (role, model) averages when available; cold-start
// (no recorded history for that pair) falls back to the pricing
// table multiplied by a conservative token estimate.
//
// The forecast is *advisory* — operators tune it via the
// LookbackDays / cold-start defaults, but the math is deliberately
// crude:
//   - one step's history is the average cost per step across all
//     prior runs of (role, model) within the lookback window
//   - plan-step children become separate tasks that get separately
//     forecasted and gated, so the parent plan step only contributes
//     its own coordinator cost
//   - workflow loops aren't modelled — a step that visits 3 times
//     in production gets 1× its average in the forecast (the
//     average already accounts for the typical visit count over
//     historical runs)
//
// Refusal logic lives in the caller: the forecast is a number; the
// caller decides whether to gate based on its own remaining-envelope
// math. Keeping it that way means the same Forecast can drive both
// the dispatcher's hard refusal and the autonomy manager's softer
// "this would be expensive — note in the audit log" behaviour.
type Forecast struct {
	// USD is the total predicted task cost.
	USD float64
	// Steps breaks the total down per step so callers can log /
	// surface "the implement step is the expensive one" detail
	// without recomputing.
	Steps []StepForecast
	// LookbackDays is the window the historical per-step averages
	// were drawn from. Reported back so the caller's log line
	// includes the data window.
	LookbackDays int
}

// StepForecast is the per-step contribution to the total forecast.
type StepForecast struct {
	StepID string
	Role   string
	Model  string
	USD    float64
	// Source is "history" when the value came from historical
	// per-step averages, "pricing" when it fell back to
	// pricing-table × conservative token estimate, or "skip" for
	// step types (gate, approval, terminal) that don't bill.
	Source string
	// SampleSize is the historical sample count for the (role,
	// model) combo. Zero on cold-start. Operators use it to know
	// when the forecast is trustworthy.
	SampleSize int
}

// ForecastInput is what ForecastTask needs to walk a workflow and
// estimate cost. Bundled into one struct so the call site doesn't
// have to thread eight positional arguments.
type ForecastInput struct {
	// Workflow is the workflow that's about to run. Its Steps map
	// is the iteration target; types other than agent/plan are
	// skipped (gate / approval / terminal don't bill LLM calls).
	Workflow *registry.Workflow
	// Swarm is the swarm whose roles this workflow targets. Used
	// to resolve each step's effective model (role override → role
	// envVars → daemon default).
	Swarm *registry.Swarm
	// DefaultModel is the daemon-wide VORNIK_LLM_MODEL fallback,
	// used when neither the role nor the swarm pins a model.
	DefaultModel string
	// LookbackDays is the historical window for per-(role,model)
	// averages. 0 → 30 days.
	LookbackDays int
}

// HistoryRepo is the narrow surface ForecastTask needs from the
// usage repository. The production persistence.TaskLLMUsageRepository
// satisfies it; the narrow type lets tests stub without dragging in
// the full repo's other methods.
type HistoryRepo interface {
	AggregateByRoleModel(ctx context.Context, since, until time.Time, limit int, projectID string) ([]persistence.RoleModelSpend, error)
}

// Conservative cold-start tokens-per-step. Picked to be
// representative of a moderately-sized agent step:
//   - ~5 tool-call iterations
//   - ~6000 prompt tokens per iteration (system prompt + tools +
//     conversation grown over the run)
//   - ~800 completion tokens per iteration
//
// The combination is meant to over-estimate cheap models (refusing
// a few cheap tasks early is cheaper than allowing one runaway
// expensive task) and under-estimate truly long agent runs (which
// the mid-execution tripwire catches anyway). Operators can tune
// the lookback window to weight the forecast more toward history.
const (
	coldStartPromptTokens     = 30_000
	coldStartCompletionTokens = 4_000
)

// ForecastTask walks the workflow and returns a Forecast. Errors
// only on repo failures; an empty workflow returns Forecast{USD: 0}.
// A nil pricing table is fine — cold-start contributions for
// unknown models then return 0 and the caller sees a forecast
// composed solely of historical contributions.
func ForecastTask(ctx context.Context, repo HistoryRepo, table *pricing.Table, input ForecastInput, now time.Time) (Forecast, error) {
	out := Forecast{LookbackDays: input.LookbackDays}
	if out.LookbackDays <= 0 {
		out.LookbackDays = 30
	}
	if input.Workflow == nil || len(input.Workflow.Steps) == 0 {
		return out, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	// One repo call gets us every (role, model) combo's spend +
	// step count over the window. Index by (role, model) for O(1)
	// lookup per step.
	since := now.AddDate(0, 0, -out.LookbackDays)
	stats, err := repo.AggregateByRoleModel(ctx, since, now, 0, "")
	if err != nil {
		return out, fmt.Errorf("forecast: aggregate by role/model: %w", err)
	}
	type key struct {
		role  string
		model string
	}
	idx := make(map[key]persistence.RoleModelSpend, len(stats))
	for _, s := range stats {
		idx[key{s.Role, s.Model}] = s
	}

	// Walk steps in workflow-defined iteration order. Map iteration
	// is unordered; we want a stable Steps slice so test assertions
	// and log lines reproduce. The workflow's Entrypoint + OnSuccess
	// chain is the canonical traversal order for forecast purposes,
	// but workflows can branch — for v1 we just iterate the map and
	// skip steps already seen, accepting some non-determinism in
	// row order (the total USD is order-independent).
	for stepID, step := range input.Workflow.Steps {
		sf := StepForecast{StepID: stepID, Role: step.Role}
		switch step.Type {
		case "agent", "plan":
			// chargeable
		default:
			// gate, approval, terminal etc. don't bill LLM calls.
			sf.Source = "skip"
			out.Steps = append(out.Steps, sf)
			continue
		}

		model := resolveStepModel(step, input.Swarm, input.DefaultModel)
		sf.Model = model

		if hist, ok := idx[key{role: step.Role, model: model}]; ok && hist.StepCount > 0 {
			sf.USD = hist.CostUSD / float64(hist.StepCount)
			sf.Source = "history"
			sf.SampleSize = hist.StepCount
		} else if table != nil && model != "" {
			sf.USD = table.CostUSD(model, coldStartPromptTokens, coldStartCompletionTokens)
			sf.Source = "pricing"
		} else {
			// No history, no pricing entry — leave at $0. Better to
			// pass a refusal-decision through with a clear "could
			// not forecast" flag than to invent a number.
			sf.Source = "unknown"
		}
		out.USD += sf.USD
		out.Steps = append(out.Steps, sf)
	}
	return out, nil
}

// resolveStepModel mirrors executor.effectiveRoleModel: role.Model
// wins, then runtime.envVars["VORNIK_LLM_MODEL"], then the daemon
// default. Duplicated here rather than imported because making the
// budget package depend on the executor would create a cycle.
func resolveStepModel(step registry.WorkflowStep, swarm *registry.Swarm, defaultModel string) string {
	if step.Role == "" || swarm == nil {
		return defaultModel
	}
	for i := range swarm.Roles {
		role := &swarm.Roles[i]
		if role.Name != step.Role {
			continue
		}
		if role.Model != "" {
			return role.Model
		}
		if role.Runtime.EnvVars != nil && role.Runtime.EnvVars["VORNIK_LLM_MODEL"] != "" {
			return role.Runtime.EnvVars["VORNIK_LLM_MODEL"]
		}
	}
	return defaultModel
}

// CheckForecast pairs a Forecast with the project's current spend
// and budget caps to produce a refusal decision. Returns Refused=
// true with a reason when forecast + currentSpend would breach the
// hard cap on either daily or monthly horizons. Soft caps don't
// gate the forecast — they're notification triggers only, same as
// the existing Check semantics.
//
// currentDecision is the result of a fresh Check call — passing it
// in (rather than re-running Check) means callers that already have
// the decision in scope (dispatcher, autonomy manager) don't pay
// for a second SQL aggregate, and the forecast gate sees the same
// snapshot the budget gate just used.
func CheckForecast(project *registry.Project, forecast Forecast, currentDecision Decision) ForecastDecision {
	d := ForecastDecision{Forecast: forecast}
	if project == nil {
		return d
	}
	b := project.Budget
	if b.DailyHardUSD > 0 {
		projected := currentDecision.DailyUSD + forecast.USD
		if projected >= b.DailyHardUSD {
			d.Refused = true
			d.Reason = fmt.Sprintf(
				"forecast $%.2f + spent $%.2f = $%.2f would breach daily hard cap $%.2f",
				forecast.USD, currentDecision.DailyUSD, projected, b.DailyHardUSD,
			)
			return d
		}
	}
	if b.MonthlyHardUSD > 0 {
		projected := currentDecision.MonthlyUSD + forecast.USD
		if projected >= b.MonthlyHardUSD {
			d.Refused = true
			d.Reason = fmt.Sprintf(
				"forecast $%.2f + month-to-date $%.2f = $%.2f would breach monthly hard cap $%.2f",
				forecast.USD, currentDecision.MonthlyUSD, projected, b.MonthlyHardUSD,
			)
			return d
		}
	}
	return d
}

// ForecastDecision is what CheckForecast returns: the underlying
// Forecast plus a refusal decision the caller can surface to the
// operator. Refused=false means the task can proceed; the caller
// might still log the forecast as a soft signal.
type ForecastDecision struct {
	Forecast Forecast
	Refused  bool
	Reason   string
}
