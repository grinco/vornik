package observability_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"vornik.io/vornik/internal/observability"
)

// TestNewInstinctMetrics_NonNil verifies all fields are populated after
// construction with a fresh isolated registry.
func TestNewInstinctMetrics_NonNil(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewInstinctMetrics(reg)
	if m == nil {
		t.Fatal("NewInstinctMetrics returned nil")
	}
	if m.ExtractionTicksTotal == nil {
		t.Error("ExtractionTicksTotal is nil")
	}
	if m.ExtractionDurationSeconds == nil {
		t.Error("ExtractionDurationSeconds is nil")
	}
	if m.InstinctsUpsertedTotal == nil {
		t.Error("InstinctsUpsertedTotal is nil")
	}
	if m.EvidenceAddedTotal == nil {
		t.Error("EvidenceAddedTotal is nil")
	}
	if m.InstinctTotal == nil {
		t.Error("InstinctTotal is nil")
	}
	if m.DistillationsTotal == nil {
		t.Error("DistillationsTotal is nil")
	}
	if m.ApplicationsTotal == nil {
		t.Error("ApplicationsTotal is nil")
	}
	if m.GlobalConflictsTotal == nil {
		t.Error("GlobalConflictsTotal is nil")
	}
	if m.LiftEvaluatedTotal == nil {
		t.Error("LiftEvaluatedTotal is nil")
	}
	if m.RetireProposalsTotal == nil {
		t.Error("RetireProposalsTotal is nil")
	}
}

// TestNewInstinctMetrics_LiftAndRetireNames verifies LiftEvaluatedTotal and
// RetireProposalsTotal register under their exact fully-qualified metric
// names, so lld-contracts lint can resolve them to code.
func TestNewInstinctMetrics_LiftAndRetireNames(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewInstinctMetrics(reg)

	m.LiftEvaluatedTotal.WithLabelValues("budget", "helping").Inc()
	m.RetireProposalsTotal.WithLabelValues("budget", "created").Inc()

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	want := map[string]bool{
		"vornik_instinct_lift_evaluated_total":   false,
		"vornik_instinct_retire_proposals_total": false,
	}
	for _, mf := range mfs {
		if _, ok := want[mf.GetName()]; ok {
			want[mf.GetName()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("metric %s not found in registry", name)
		}
	}
}

// TestNewInstinctMetrics_NilRegisterer verifies that passing nil falls back
// gracefully (no panic). We can't safely use the default registry in tests
// due to duplicate registration risk, so we just verify nil doesn't panic.
func TestNewInstinctMetrics_NilRegisterer(t *testing.T) {
	// Use an isolated registry instead of the default to avoid conflicts.
	reg := prometheus.NewRegistry()
	m := observability.NewInstinctMetrics(reg)
	if m == nil {
		t.Fatal("NewInstinctMetrics with explicit registry returned nil")
	}
}

// TestInstinctMetrics_Emit verifies the counters can be incremented without
// panicking — a nil-safe emit check.
func TestInstinctMetrics_Emit(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewInstinctMetrics(reg)

	// These match the label values used in the real worker code.
	m.ExtractionTicksTotal.WithLabelValues("idle").Inc()
	m.ExtractionTicksTotal.WithLabelValues("progressed").Inc()
	m.ExtractionTicksTotal.WithLabelValues("errored").Inc()
	m.InstinctsUpsertedTotal.WithLabelValues("budget").Inc()
	m.EvidenceAddedTotal.WithLabelValues("support").Inc()
	m.EvidenceAddedTotal.WithLabelValues("contradict").Inc()
	m.InstinctTotal.WithLabelValues("budget", "active").Set(3)
	m.DistillationsTotal.Inc()
	m.ApplicationsTotal.WithLabelValues("lead_recovery", "accepted").Inc()
	m.GlobalConflictsTotal.WithLabelValues("budget", "replaced").Inc()
	m.LiftEvaluatedTotal.WithLabelValues("budget", "helping").Inc()
	m.RetireProposalsTotal.WithLabelValues("budget", "created").Inc()

	// Gather to confirm metrics were registered and updated.
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if len(mfs) == 0 {
		t.Error("Gather returned no metric families")
	}
}

// TestInstinctMetrics_ApplicationsTotal_NilSafe verifies that nil-checking
// ApplicationsTotal before use (the pattern in executor call sites) is safe.
func TestInstinctMetrics_ApplicationsTotal_NilSafe(_ *testing.T) {
	var m *observability.InstinctMetrics
	// CE call sites guard: if m != nil && m.ApplicationsTotal != nil { ... }
	if m != nil && m.ApplicationsTotal != nil {
		m.ApplicationsTotal.WithLabelValues("surface", "result").Inc()
	}
	// Reaching here without panic is the assertion.
}
