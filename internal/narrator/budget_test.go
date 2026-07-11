package narrator

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

func counterVecValue(t *testing.T, cv *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	c, err := cv.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return m.GetCounter().GetValue()
}

// TestBudget_LineCap_StopsNarrationEntirely pins design §8's precise
// wording: the line cap is a HARD STOP — once hit, no further lines
// (not even a template) are produced for that execution, and the
// capped metric fires exactly once (reason=lines).
func TestBudget_LineCap_StopsNarrationEntirely(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := newTestHarness(t, func(n *Narrator) {
		n.Metrics = NewMetrics(reg)
		n.MaxLines = 2
	})
	seedRunningExecution(h)

	// Two steps that each start+complete, spaced beyond
	// MinLineInterval so both land distinctly.
	for i, stepID := range []string{"s1", "s2", "s3"} {
		h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: stepID, Role: "worker"})
		h.Sub.push(testExecID, livepubsub.KindStepCompleted, livepubsub.StepCompletedPayload{StepID: stepID, Outcome: "ok"})
		if i < 2 {
			h.awaitLine(2 * time.Second)
			time.Sleep(20 * time.Millisecond) // clear min_line_interval before the next step
		}
	}
	// The 3rd step's completion must NOT produce a line — the cap
	// (2) was already hit by the first two.
	h.expectNoLine(100 * time.Millisecond)

	if got := counterVecValue(t, h.N.Metrics.BudgetCappedTotal, "lines"); got != 1 {
		t.Errorf("budget_capped_total{reason=lines} = %v, want 1", got)
	}
	rows := h.Store.all()
	if len(rows) != 2 {
		t.Fatalf("expected exactly 2 persisted rows once line-capped, got %d", len(rows))
	}
}

// TestBudget_CostCap_FlipsToTemplateOnly pins the OTHER half of §8:
// once the per-execution LLM spend budget is exhausted, narration
// KEEPS PRODUCING LINES but stops calling the LLM — every subsequent
// line is a deterministic, Degraded=true template.
func TestBudget_CostCap_FlipsToTemplateOnly(t *testing.T) {
	reg := prometheus.NewRegistry()
	fp := &fakeProvider{replies: []string{"Reading the documents you gave me."}}
	h := newTestHarness(t, func(n *Narrator) {
		n.Metrics = NewMetrics(reg)
		n.MaxCostUSD = 0.05
		n.MaxLines = 100 // isolate the cost cap from the line cap
		n.Client = fp
		n.Pricing = fakePricing{perCall: 0.05} // first call alone crosses the budget
	})
	seedRunningExecution(h)

	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s1", Role: "researcher"})
	first := h.awaitLine(2 * time.Second)
	if first.Degraded {
		t.Errorf("first line should be LLM-composed (not yet capped), got Degraded=true text=%q", first.Text)
	}

	time.Sleep(20 * time.Millisecond) // clear min_line_interval
	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s2", Role: "researcher"})
	h.Sub.push(testExecID, livepubsub.KindStepCompleted, livepubsub.StepCompletedPayload{StepID: "s2", Outcome: "ok"})
	second := h.awaitLine(2 * time.Second)
	if !second.Degraded {
		t.Errorf("second line should be template-only after the cost cap, got Degraded=false text=%q", second.Text)
	}

	if got := counterVecValue(t, h.N.Metrics.BudgetCappedTotal, "cost"); got != 1 {
		t.Errorf("budget_capped_total{reason=cost} = %v, want 1", got)
	}
	// Only ONE LLM call should have happened — the capped path never
	// calls the provider again.
	if fp.calls != 1 {
		t.Errorf("provider Complete calls = %d, want exactly 1 (capped after)", fp.calls)
	}
}

// TestBudget_LinesTotal_LabelsKindAndDegraded — the lines_total
// counter carries both labels the design specifies.
func TestBudget_LinesTotal_LabelsKindAndDegraded(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := newTestHarness(t, func(n *Narrator) {
		n.Metrics = NewMetrics(reg)
	})
	seedRunningExecution(h)

	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s1", Role: "worker"})
	h.awaitLine(2 * time.Second)

	if got := counterVecValue(t, h.N.Metrics.LinesTotal, persistence.ExecutionNarrationKindStep, "true"); got != 1 {
		t.Errorf("lines_total{kind=step,degraded=true} = %v, want 1", got)
	}
}
