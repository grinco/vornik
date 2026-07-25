package api

import (
	"context"
	"time"

	"vornik.io/vornik/internal/budget"
)

// forecastBudgetRefusal runs the pre-flight forecast gate for a create request
// (LLD 2026-07-24 §3.4). It resolves the project's workflow + swarm, forecasts
// the run's USD cost, and returns a non-empty refusal reason when the forecast
// would breach EITHER the project daily/monthly hard cap OR the effective
// per-task budget. Returns "" (allow) when:
//
//   - no usage repo / registry is wired (no history → no forecast),
//   - neither a project hard cap nor a per-task budget is configured
//     (short-circuit — byte-identical to today's behaviour), or
//
// Once a hard cap is configured, missing inputs or a failed spend/forecast
// read return a refusal. Starting work while the preventive gate is blind can
// itself incur unbounded cost before the reactive step-boundary check runs.
//
// This is the seam that wires the two currently-ungated create paths (POST
// /tasks and the webhook) into the uniform forecast+per-task-budget+project-cap
// gate, matching the two already-wired paths (dispatcher, autonomy).
func (s *Server) forecastBudgetRefusal(ctx context.Context, projectID, workflowID string) string {
	if s.llmUsageRepo == nil || s.projectRegistry == nil {
		return ""
	}
	proj := s.projectRegistry.GetProject(projectID)
	if proj == nil {
		return ""
	}
	if workflowID == "" {
		workflowID = proj.DefaultWorkflowID
	}
	if workflowID == "" {
		return ""
	}
	// create via API has NO budget override (admin-only, §3.1) → project default.
	taskBudgetUSD := budget.EffectiveTaskBudgetUSD(proj, nil)
	if proj.Budget.DailyHardUSD <= 0 && proj.Budget.MonthlyHardUSD <= 0 && taskBudgetUSD <= 0 {
		return "" // nothing configured → short-circuit
	}
	wf := s.projectRegistry.GetWorkflow(workflowID)
	swarm := s.projectRegistry.GetSwarm(proj.SwarmID)
	if wf == nil || swarm == nil {
		return "budget forecast unavailable: workflow or swarm configuration is missing"
	}
	current, cerr := budget.Check(ctx, s.llmUsageRepo, proj, time.Now().UTC())
	if cerr != nil {
		s.logger.Warn().Err(cerr).Str("project_id", projectID).Msg("api: budget snapshot for forecast failed — refusing create")
		return "budget forecast unavailable: current spend could not be read"
	}
	forecast, ferr := budget.ForecastTask(ctx, s.llmUsageRepo, s.pricingTableLoaded(), budget.ForecastInput{
		Workflow: wf,
		Swarm:    swarm,
	}, time.Now().UTC())
	if ferr != nil {
		s.logger.Warn().Err(ferr).Str("project_id", projectID).Msg("api: forecast failed — refusing create")
		return "budget forecast unavailable: task cost could not be estimated"
	}
	if d := budget.CheckForecast(proj, forecast, current, taskBudgetUSD); d.Refused {
		s.logger.Info().
			Str("project_id", projectID).
			Str("workflow_id", workflowID).
			Float64("forecast_usd", forecast.USD).
			Str("reason", d.Reason).
			Msg("api: refusing create — forecast would breach budget")
		return d.Reason
	}
	return ""
}
