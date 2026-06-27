package executor

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/pricing"
)

// metricsCov_loadTable writes a one-entry pricing YAML and loads it so the
// executor tests can get a *pricing.Table whose Lookup returns "known" for a
// specific model (the Table struct has unexported fields, so Load is the only
// way to populate one from this package).
func metricsCov_loadTable(t *testing.T, model string, e pricing.Entry) *pricing.Table {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	ftoa := func(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
	yaml := "models:\n  " + model + ":\n" +
		"    input: " + ftoa(e.InputUSDPerMillion) + "\n" +
		"    output: " + ftoa(e.OutputUSDPerMillion) + "\n" +
		"    cache_creation: " + ftoa(e.CacheCreationPerMillion) + "\n" +
		"    cache_read: " + ftoa(e.CacheReadPerMillion) + "\n"
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	tab, err := pricing.Load(path)
	require.NoError(t, err)
	return tab
}

// TestMetricsCov_CrossProjectAndSpawnCounters drives the inter-project
// orchestration counters that had no coverage. Each is a thin
// WithLabelValues().Inc(); calling exercises the nil-guard + the body.
func TestMetricsCov_CrossProjectAndSpawnCounters(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())

	assert.NotPanics(t, func() {
		m.RecordCrossProjectCallStarted("caller-p", "callee-p")
		m.RecordCrossProjectCallResolved("caller-p", "callee-p", "completed", 12.5)
		// durationSeconds <= 0 skips the observe branch.
		m.RecordCrossProjectCallResolved("caller-p", "callee-p", "failed", 0)
		m.RecordProjectSpawn("caller-p", "research-template")
	})

	// nil receiver is a safe no-op for all three.
	var nilM *Metrics
	assert.NotPanics(t, func() {
		nilM.RecordCrossProjectCallStarted("a", "b")
		nilM.RecordCrossProjectCallResolved("a", "b", "completed", 1)
		nilM.RecordProjectSpawn("a", "t")
	})
}

// TestMetricsCov_ModelLabelBucketing covers modelLabel's three branches:
// nil receiver, no catalog set, and catalog-present (known vs unknown).
func TestMetricsCov_ModelLabelBucketing(t *testing.T) {
	// nil receiver returns the raw model.
	var nilM *Metrics
	assert.Equal(t, "gpt-x", nilM.modelLabel("gpt-x"))

	m := NewMetrics(prometheus.NewRegistry())
	// No catalog set yet → raw model passes through.
	assert.Equal(t, "raw-model", m.modelLabel("raw-model"))

	// Build a catalog containing exactly one known model.
	tab := metricsCov_loadTable(t, "known-model", pricing.Entry{
		InputUSDPerMillion:  1,
		OutputUSDPerMillion: 2,
	})
	m.SetModelCatalog(tab)
	assert.Equal(t, "known-model", m.modelLabel("known-model"))
	// Unknown model collapses to the "other" bucket label.
	assert.Equal(t, modelLabelOther, m.modelLabel("never-heard-of-it"))
}

// TestMetricsCov_RecordLLMUsageWithCache_NoPricing exercises the token
// counters with cache-creation/cache-read tokens and no pricing table
// (the cost branch is skipped). Pairs with the existing 96.3% cover to
// hit the no-pricing path.
func TestMetricsCov_RecordLLMUsageWithCache_NoPricing(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	assert.NotPanics(t, func() {
		m.RecordLLMUsageWithCache("p", "writer", "model-z", 100, 50, 3, 20, 40, nil)
	})

	// nil receiver no-op.
	var nilM *Metrics
	assert.NotPanics(t, func() {
		nilM.RecordLLMUsageWithCache("p", "r", "m", 1, 1, 1, 1, 1, nil)
	})
}

// TestMetricsCov_RecordLLMUsageWithCache_WithPricingCacheSavings drives the
// pricing branch including the cache-savings sub-branch so both the cost
// add and the savings add execute.
func TestMetricsCov_RecordLLMUsageWithCache_WithPricingCacheSavings(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	tab := metricsCov_loadTable(t, "priced", pricing.Entry{
		InputUSDPerMillion:      3.0,
		OutputUSDPerMillion:     15.0,
		CacheReadPerMillion:     0.3,
		CacheCreationPerMillion: 3.75,
	})
	assert.NotPanics(t, func() {
		m.RecordLLMUsageWithCache("p", "writer", "priced", 1000, 500, 2, 200, 800, tab)
	})
}

// TestMetricsCov_RecordFinalOutcome_AllBuckets walks every outcome string
// through RecordFinalOutcome so each switch arm and the gauge recompute run,
// then confirms the guard branches (nil receiver, empty fields).
func TestMetricsCov_RecordFinalOutcome_AllBuckets(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	outcomes := []string{
		"ok", "failed", "timeout", "cancelled", "parse_error",
		"schema_violation", "refused", "iteration_exhausted",
		"degenerate_loop", "downstream_rejected", "gate_failed",
		"unrecognised-outcome", // default arm: no counter bumped, gauges still recomputed
	}
	for _, o := range outcomes {
		assert.NotPanics(t, func() {
			m.RecordFinalOutcome("writer", "model-a", o)
		})
	}

	// Guard branches: empty role / model / outcome and nil receiver.
	var nilM *Metrics
	assert.NotPanics(t, func() {
		nilM.RecordFinalOutcome("r", "m", "ok")
		m.RecordFinalOutcome("", "m", "ok")
		m.RecordFinalOutcome("r", "", "ok")
		m.RecordFinalOutcome("r", "m", "")
	})
}

// TestMetricsCov_ShapeRetryRecoveredAndOutcome covers the kind-default
// backfill on RecordShapeRetryRecovered and the allow/deny switch on
// RecordShapeRetryOutcome.
func TestMetricsCov_ShapeRetryRecoveredAndOutcome(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())

	assert.NotPanics(t, func() {
		m.RecordShapeRetryRecovered("writer", "model-a", "schema_violation")
		// empty kind → defaulted to "shape_failure".
		m.RecordShapeRetryRecovered("writer", "model-a", "")
	})

	// Allowed outcomes increment; an unknown outcome is a no-op (cardinality guard).
	assert.NotPanics(t, func() {
		m.RecordShapeRetryOutcome("writer", "attempted")
		m.RecordShapeRetryOutcome("writer", "recovered")
		m.RecordShapeRetryOutcome("writer", "failed")
		m.RecordShapeRetryOutcome("writer", "bogus") // default arm → drop
	})

	// Guard branches.
	var nilM *Metrics
	assert.NotPanics(t, func() {
		nilM.RecordShapeRetryRecovered("r", "m", "k")
		m.RecordShapeRetryRecovered("", "m", "k")
		nilM.RecordShapeRetryOutcome("r", "attempted")
		m.RecordShapeRetryOutcome("", "attempted")
		m.RecordShapeRetryOutcome("r", "")
	})
}
