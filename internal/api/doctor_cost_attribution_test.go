package api

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestCheckCostAttribution_MetricsUnwiredOK confirms the check
// doesn't panic when no APIMetrics is wired — fresh-deployment
// safety.
func TestCheckCostAttribution_MetricsUnwiredOK(t *testing.T) {
	h := &DoctorHandlers{}
	got := h.checkCostAttribution()
	if got.Status != "OK" {
		t.Errorf("status = %q, want OK", got.Status)
	}
	if !strings.Contains(got.Message, "not wired") {
		t.Errorf("message should explain why; got %q", got.Message)
	}
}

// TestCheckCostAttribution_BelowFloorOK: a daemon that's only
// served three external API calls shouldn't WARN — sample's
// too small.
func TestCheckCostAttribution_BelowFloorOK(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewAPIMetrics(reg)
	m.RecordCostAttribution(AttributionFromHeader)
	m.RecordCostAttribution(AttributionFromHeader)
	m.RecordCostAttribution(AttributionAnonymous)
	h := &DoctorHandlers{apiMetrics: m}
	got := h.checkCostAttribution()
	if got.Status != "OK" {
		t.Errorf("status = %q, want OK (below floor of %d)", got.Status, costAttributionMinTotal)
	}
}

// TestCheckCostAttribution_KeyBoundDominant_OK is the happy
// path — operators are mostly on DB-backed keys.
func TestCheckCostAttribution_KeyBoundDominant_OK(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewAPIMetrics(reg)
	for i := 0; i < 95; i++ {
		m.RecordCostAttribution(AttributionFromDBKey)
	}
	for i := 0; i < 5; i++ {
		m.RecordCostAttribution(AttributionFromHeader)
	}
	h := &DoctorHandlers{apiMetrics: m}
	got := h.checkCostAttribution()
	if got.Status != "OK" {
		t.Errorf("status = %q, want OK; msg=%q", got.Status, got.Message)
	}
	if !strings.Contains(got.Message, "95%") {
		t.Errorf("message should report 95%%; got %q", got.Message)
	}
}

// TestCheckCostAttribution_LegacyDominant_Warns: the failure
// mode the operator-backlog item describes — most cost rows
// come from the legacy paths, not DB-bound keys.
func TestCheckCostAttribution_LegacyDominant_Warns(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewAPIMetrics(reg)
	for i := 0; i < 20; i++ {
		m.RecordCostAttribution(AttributionFromHeader)
	}
	for i := 0; i < 5; i++ {
		m.RecordCostAttribution(AttributionFromDBKey)
	}
	h := &DoctorHandlers{apiMetrics: m}
	got := h.checkCostAttribution()
	if got.Status != "WARNING" {
		t.Errorf("status = %q, want WARNING; msg=%q", got.Status, got.Message)
	}
	if !strings.Contains(got.Message, "20%") {
		t.Errorf("message should report 20%% key-bound; got %q", got.Message)
	}
	if len(got.Items) == 0 {
		t.Errorf("WARNING should include remediation Items")
	}
}

// TestReadCostAttributionCounts: per-source extraction from
// the prometheus Counter.Write protobuf.
func TestReadCostAttributionCounts(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewAPIMetrics(reg)
	m.RecordCostAttribution(AttributionFromDBKey)
	m.RecordCostAttribution(AttributionFromDBKey)
	m.RecordCostAttribution(AttributionFromHeader)
	m.RecordCostAttribution(AttributionAnonymous)
	c := readCostAttributionCounts(m)
	if c.keyBound != 2 || c.header != 1 || c.fallback != 0 || c.anonymous != 1 {
		t.Errorf("counts = %+v, want {keyBound:2 header:1 fallback:0 anonymous:1}", c)
	}
	if c.total() != 4 {
		t.Errorf("total = %d, want 4", c.total())
	}
}
