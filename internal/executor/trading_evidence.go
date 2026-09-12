package executor

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/trading"
)

// The analysis-evidence gate (2026-09-10). Twelve ibkr-trading-v2 ticks on
// 2026-09-09/10: six made no bars, quote or TA call and returned empty
// proposals; the judge passed every one as a valid skip. Nothing deterministic
// could tell "examined and skipped" from "never examined". This participant
// reads the strategist step's own tool_audit_log rows and refuses the step
// unless every held, benchmark and entry-universe symbol was examined and the
// carried values equal what the tools returned. Pure logic lives in
// internal/trading/evidence.go; this file is the executor adapter.
//
// Design: https://docs.vornik.io §Analysis-
// evidence gate.

// evidenceNow is the clock the gate uses for the session check; tests pin it.
var evidenceNow = time.Now

// evidenceRefusedTotal is nil until RegisterEvidenceMetrics wires it onto the
// served registry, like the floor counter; increments are nil-guarded.
var evidenceRefusedTotal *prometheus.CounterVec

// RegisterEvidenceMetrics registers vornik_trading_evidence_refused_total.
func RegisterEvidenceMetrics(reg prometheus.Registerer) {
	if reg == nil {
		return
	}
	evidenceRefusedTotal = promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
		Namespace: "vornik",
		Subsystem: "trading",
		Name:      "evidence_refused_total",
		Help:      "Strategist steps refused by the analysis-evidence gate, labelled by reason.",
	}, []string{"reason"})
}

func incEvidenceRefused(reason string) {
	if evidenceRefusedTotal != nil {
		evidenceRefusedTotal.WithLabelValues(reason).Inc()
	}
}

// checkTradingEvidence returns the refusal error for a trading strategist
// step that did not prove its analysis, or nil. It is self-gated: non-trading
// tasks, projects without the gate, results without a proposals key, and
// ticks outside the US regular session pass untouched.
func (e *Executor) checkTradingEvidence(ctx context.Context, in *StepOutcome) error {
	if in == nil || in.Task == nil || extractTaskType(in.Task) != "trading" || e.auditRepo == nil {
		return nil
	}
	resolver, ok := e.workflows.(interface {
		GetProject(string) *registry.Project
	})
	if !ok {
		return nil
	}
	p := resolver.GetProject(in.Task.ProjectID)
	if p == nil || !p.Trading.AnalysisEvidence.Enabled {
		return nil
	}
	if open, _ := trading.USRegularSession(evidenceNow()); !open {
		return nil
	}
	if in.Execution == nil || in.Execution.ID == "" {
		return nil
	}
	execID := in.Execution.ID
	filter := persistence.ToolAuditFilter{ExecutionID: &execID, PageSize: 500}
	if in.StepID != "" {
		sid := in.StepID
		filter.StepID = &sid
	}
	entries, err := e.auditRepo.List(ctx, filter)
	if err != nil {
		incEvidenceRefused("audit_unavailable")
		return fmt.Errorf("schema violation: analysis evidence: tool audit unavailable for this step: %w", err)
	}
	calls := make([]trading.ToolCall, 0, len(entries))
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		calls = append(calls, trading.ToolCall{Name: entry.ToolName, Input: entry.ToolInput, Output: entry.ToolOutput, OK: entry.Outcome == outcomeOK})
	}
	refusal := trading.CheckAnalysisEvidence(in.ResultBytes, calls, p.Trading.AnalysisEvidence, p.Trading.EntryPolicy.AllowedSymbols, p.Trading.ProtectedSymbols)
	if refusal == nil {
		return nil
	}
	incEvidenceRefused(refusal.Reason)
	e.logger.Warn().
		Str("task_id", in.Task.ID).
		Str("project_id", in.Task.ProjectID).
		Str("step", in.StepID).
		Str("reason", refusal.Reason).
		Msg("trading analysis-evidence gate refused the step: " + refusal.Detail)
	// "schema violation:" is the prefix the shape-retry layer recognises, so
	// the strategist gets one corrective attempt naming what was missing.
	return fmt.Errorf("schema violation: analysis evidence [%s]: %s", refusal.Reason, refusal.Detail)
}
