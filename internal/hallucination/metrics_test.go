package hallucination

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestNewMetrics_RegistersOnRegistry(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	if m == nil {
		t.Fatal("NewMetrics returned nil")
	}
	if m.JudgeVerdictsTotal == nil || m.JudgeConfidence == nil ||
		m.JudgeCostUSDTotal == nil || m.JudgeEvaluationsTotal == nil ||
		m.SignalsTotal == nil {
		t.Fatal("NewMetrics produced nil collectors")
	}
	// Touch each counter once so they materialise in Gather output;
	// counter-vecs without any observed labels are invisible to Gather.
	m.JudgeVerdictsTotal.WithLabelValues("p", "r", "pass").Inc()
	m.JudgeCostUSDTotal.WithLabelValues("p", "r", "model").Add(0.01)
	m.JudgeEvaluationsTotal.WithLabelValues("p", "ok").Inc()
	m.SignalsTotal.WithLabelValues("p", string(SeverityHigh), "d").Inc()
	m.JudgeConfidence.WithLabelValues("p", "r", "pass").Observe(0.5)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	want := []string{
		"vornik_judge_verdicts_total",
		"vornik_judge_confidence",
		"vornik_judge_cost_usd_total",
		"vornik_judge_evaluations_total",
		"vornik_hallucination_signals_total",
	}
	got := map[string]bool{}
	for _, mf := range mfs {
		got[mf.GetName()] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("metric %s not registered (got: %v)", w, got)
		}
	}
}

func TestNewMetrics_NilRegistererSkipsRegistration(t *testing.T) {
	m := NewMetrics(nil)
	if m == nil {
		t.Fatal("NewMetrics(nil) returned nil")
	}
	if m.SignalsTotal == nil {
		t.Fatal("collectors must still be constructed even when reg is nil")
	}
}

func TestObserveSignals_IncrementsPerSignal(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	signals := []Signal{
		{Detector: "url_not_fetched", Severity: SeverityHigh, ClaimType: "url"},
		{Detector: "url_not_fetched", Severity: SeverityHigh, ClaimType: "url"},
		{Detector: "path_missing", Severity: SeverityWarn, ClaimType: "path"},
	}
	m.ObserveSignals("proj-1", signals)

	if got := testutil.ToFloat64(m.SignalsTotal.WithLabelValues("proj-1", string(SeverityHigh), "url_not_fetched")); got != 2 {
		t.Errorf("url_not_fetched/high count = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.SignalsTotal.WithLabelValues("proj-1", string(SeverityWarn), "path_missing")); got != 1 {
		t.Errorf("path_missing/warn count = %v, want 1", got)
	}
}

func TestObserveSignals_NilMetricsIsNoOp(t *testing.T) {
	var m *Metrics
	// Must not panic on nil receiver.
	m.ObserveSignals("proj", []Signal{{Detector: "x", Severity: SeverityInfo}})
}

func TestObserveSignals_EmptySignalsIsNoOp(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.ObserveSignals("proj", nil)
	m.ObserveSignals("proj", []Signal{})
	// No assertion on counts — both must be no-ops; only checks no panic.
}

func TestSeverity_BlockTrueForHighOnly(t *testing.T) {
	cases := []struct {
		s    Severity
		want bool
	}{
		{SeverityHigh, true},
		{SeverityWarn, false},
		{SeverityInfo, false},
		{Severity(""), false},
	}
	for _, tc := range cases {
		if got := tc.s.Block(); got != tc.want {
			t.Errorf("Severity(%q).Block() = %v, want %v", tc.s, got, tc.want)
		}
	}
}

func TestNewMetrics_HelpStringsArePresent(t *testing.T) {
	reg := prometheus.NewRegistry()
	NewMetrics(reg)
	mfs, _ := reg.Gather()
	for _, mf := range mfs {
		if strings.TrimSpace(mf.GetHelp()) == "" {
			t.Errorf("metric %s has empty help", mf.GetName())
		}
	}
}
