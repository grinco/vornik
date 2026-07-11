package ui

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewIntegrationsMetrics_RegistersOnGivenRegistry locks the metric
// names the design (§5.8) and scripts/lld-lint-allowlist.txt's now-removed
// [PLANNED] entries both name: vornik_integration_probe_total and
// vornik_integration_save_total. A CounterVec exposes no series in
// Gather() until at least one label combination has been observed, so
// this records one of each before gathering.
func TestNewIntegrationsMetrics_RegistersOnGivenRegistry(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewIntegrationsMetrics(reg)
	require.NotNil(t, m)
	m.RecordProbe("telegram", "ok")
	m.RecordSave("telegram", "ok")

	mfs, err := reg.Gather()
	require.NoError(t, err)
	names := make(map[string]bool, len(mfs))
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}
	assert.True(t, names["vornik_integration_probe_total"], "vornik_integration_probe_total must be registered on the given registry")
	assert.True(t, names["vornik_integration_save_total"], "vornik_integration_save_total must be registered on the given registry")
}

func TestIntegrationsMetrics_RecordProbe_IncrementsLabelledCounter(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewIntegrationsMetrics(reg)
	m.RecordProbe("telegram", "ok")
	m.RecordProbe("telegram", "ok")
	m.RecordProbe("email", "fail")

	assert.Equal(t, float64(2), testutil.ToFloat64(m.ProbeTotal.WithLabelValues("telegram", "ok")))
	assert.Equal(t, float64(1), testutil.ToFloat64(m.ProbeTotal.WithLabelValues("email", "fail")))
}

// TestIntegrationsMetrics_RecordProbe_EmptyOutcomeNoop covers the case
// Save short-circuits before ever probing (e.g. an invalid project id) —
// result.Probe.Outcome stays the Outcome zero value ("") and must not be
// recorded as a spurious labelled series.
func TestIntegrationsMetrics_RecordProbe_EmptyOutcomeNoop(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewIntegrationsMetrics(reg)
	m.RecordProbe("email", "")
	assert.Equal(t, 0, testutil.CollectAndCount(m.ProbeTotal))
}

func TestIntegrationsMetrics_RecordSave_IncrementsLabelledCounter(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewIntegrationsMetrics(reg)
	m.RecordSave("email", "ok")
	m.RecordSave("email", "probe_failed")
	m.RecordSave("email", "probe_failed")

	assert.Equal(t, float64(1), testutil.ToFloat64(m.SaveTotal.WithLabelValues("email", "ok")))
	assert.Equal(t, float64(2), testutil.ToFloat64(m.SaveTotal.WithLabelValues("email", "probe_failed")))
}

// --- nil-safety: every Record* call must be a no-op on a nil *Metrics, so
// handlers never need a "s.integrationsMetrics != nil" guard at every call
// site (design idiom shared with api.APIMetrics's Record* methods). ---

func TestIntegrationsMetrics_RecordProbe_NilReceiverNoPanic(t *testing.T) {
	var m *IntegrationsMetrics
	assert.NotPanics(t, func() { m.RecordProbe("email", "ok") })
}

func TestIntegrationsMetrics_RecordSave_NilReceiverNoPanic(t *testing.T) {
	var m *IntegrationsMetrics
	assert.NotPanics(t, func() { m.RecordSave("email", "ok") })
}

// --- WithIntegrationsMetrics ServerOption ---

func TestWithIntegrationsMetrics_WiresMetrics(t *testing.T) {
	m := NewIntegrationsMetrics(prometheus.NewRegistry())
	s := NewServer(WithIntegrationsMetrics(m))
	assert.Same(t, m, s.integrationsMetrics)
}

func TestWithIntegrationsMetrics_NilIsHarmlessNoop(t *testing.T) {
	s := NewServer(WithIntegrationsMetrics(nil))
	assert.Nil(t, s.integrationsMetrics)
}
