package memoryfirewall

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestNewMetrics_RegistersAndObserves confirms the six LLD-promised
// firewall series register against a registry and that the observe
// helpers move their counters/gauges. Before metrics.go existed
// (drift-mitigation §8.3) these series were registered nowhere and
// read flat zero.
func TestNewMetrics_RegistersAndObserves(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.ObserveDecision("proj-a", DecisionAllow)
	m.ObserveDecision("proj-a", DecisionAllow)
	m.ObserveDecision("proj-a", DecisionBlockExpired)
	m.ObserveEval("proj-a", 2*time.Millisecond)
	m.SetChunksBySensitivity("proj-a", "internal", 42)

	if got := testutil.ToFloat64(m.decisions.WithLabelValues("proj-a", "allow")); got != 2 {
		t.Fatalf("decisions_total{allow} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.decisions.WithLabelValues("proj-a", "block_expired")); got != 1 {
		t.Fatalf("decisions_total{block_expired} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.chunksBySens.WithLabelValues("proj-a", "internal")); got != 42 {
		t.Fatalf("chunks_by_sensitivity = %v, want 42", got)
	}

	// AuditMetrics hooks move the audit-writer series.
	am := m.AuditMetrics()
	if am == nil {
		t.Fatal("AuditMetrics returned nil")
	}
	am.BufferDepth(7)
	am.WritesTotal("ok")
	am.WritesTotal("ok")
	am.WriteLatency(5 * time.Millisecond)
	am.DroppedTotal()

	if got := testutil.ToFloat64(m.auditBufferDep); got != 7 {
		t.Fatalf("audit_buffer_depth = %v, want 7", got)
	}
	if got := testutil.ToFloat64(m.auditWrites.WithLabelValues("ok")); got != 2 {
		t.Fatalf("audit_writes_total{ok} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.auditWrites.WithLabelValues("dropped")); got != 1 {
		t.Fatalf("audit_writes_total{dropped} = %v, want 1", got)
	}
}

// TestMetrics_NilSafe confirms every observe helper is a no-op on a
// nil *Metrics so the recall hot path can call unconditionally when
// metrics aren't wired (SQLite / tests).
func TestMetrics_NilSafe(t *testing.T) {
	var m *Metrics
	m.ObserveDecision("p", DecisionAllow)
	m.ObserveEval("p", time.Second)
	m.SetChunksBySensitivity("p", "internal", 1)
	if am := m.AuditMetrics(); am != nil {
		t.Fatal("nil *Metrics.AuditMetrics() must be nil")
	}
}
