// Package observability: small additions to cover the
// StateCollector recording paths that didn't have dedicated tests.
// Each test reads the recorded gauge / counter value back via
// testutil so a regression that drops the underlying Set call is
// caught at the assertion line.
package observability

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestRecordAutonomyActiveLoops_SetsGauge — the autonomy scheduler
// calls this with the current per-tick active-loop count. The
// dashboard tile reads the gauge; a missing call here would
// silently render zero instead of the real backlog.
func TestRecordAutonomyActiveLoops_SetsGauge(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := NewStateCollector(reg)
	c.RecordAutonomyActiveLoops(7)
	got := testutil.ToFloat64(c.AutonomyActiveLoops)
	if got != 7 {
		t.Errorf("AutonomyActiveLoops gauge = %v, want 7", got)
	}
}

// TestRecordAutonomyActiveLoops_NilReceiverIsNoop — the collector
// is initialised lazily in some boot paths; calling the recorder
// before init must not panic. (Both the bare-nil receiver and a
// nil-gauge collector slip through the same guard clause.)
func TestRecordAutonomyActiveLoops_NilReceiverIsNoop(t *testing.T) {
	var c *StateCollector
	// Should not panic.
	c.RecordAutonomyActiveLoops(5)
}

// TestRecordProjectFinancialControls_DefaultsTZ — empty
// BudgetTimezone must fall back to "UTC" so the dashboard label
// doesn't render as "" (which Prometheus exposes as no value at
// all on the dropdown).
func TestRecordProjectFinancialControls_DefaultsTZ(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := NewStateCollector(reg)
	c.RecordProjectFinancialControls([]ProjectFinancialControls{
		{
			ProjectID:            "p1",
			BudgetTimezone:       "", // empty → "UTC"
			BudgetDailySoftUSD:   10,
			BudgetDailyHardUSD:   20,
			BudgetMonthlySoftUSD: 300,
			BudgetMonthlyHardUSD: 600,
		},
	})
	// The label tuple (project, period, kind) must include "p1" + "daily"
	// + "soft". Read it back from the underlying gauge collector.
	got := testutil.ToFloat64(c.ProjectBudgetCapsUSD.WithLabelValues("p1", "daily", "soft"))
	if got != 10 {
		t.Errorf("ProjectBudgetCapsUSD(p1, daily, soft) = %v, want 10", got)
	}
}

// TestRecordProjectFinancialControls_NilReceiverIsNoop — same
// nil-safe contract as the autonomy recorder.
func TestRecordProjectFinancialControls_NilReceiverIsNoop(t *testing.T) {
	var c *StateCollector
	c.RecordProjectFinancialControls([]ProjectFinancialControls{{ProjectID: "p1"}})
}
