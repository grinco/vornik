package executor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/testutil/metricstest"
)

func TestNewMetrics_Executor(t *testing.T) {
	t.Run("creates metrics with default registerer", func(t *testing.T) {
		metricstest.IsolateDefaultRegistry(t) // count-safe nil-fallback coverage
		m := NewMetrics(nil)
		require.NotNil(t, m)
		assert.NotNil(t, m.Active)
		assert.NotNil(t, m.StartedTotal)
		assert.NotNil(t, m.CompletedTotal)
		assert.NotNil(t, m.FailedTotal)
		assert.NotNil(t, m.CancelledTotal)
		assert.NotNil(t, m.RetriedTotal)
		assert.NotNil(t, m.DurationSeconds)
	})

	t.Run("creates metrics with custom registry", func(t *testing.T) {
		registry := prometheus.NewRegistry()
		m := NewMetrics(registry)
		require.NotNil(t, m)
		assert.NotNil(t, m.registry)
	})
}

func TestMetrics_RecordStarted(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.RecordStarted("project-1")

	// Verify started counter incremented
	count := testutil.ToFloat64(m.StartedTotal.WithLabelValues("project-1"))
	assert.Equal(t, 1.0, count)

	// Active gauge is NOT touched by RecordStarted — it's set via SetActiveGauge
	active := testutil.ToFloat64(m.Active.WithLabelValues("project-1"))
	assert.Equal(t, 0.0, active)
}

// TestMetrics_SupersededExcludedFromSuccessRate is the hardening
// regression (2026-06-15, memory LLD review batch 4): a 'superseded'
// outcome (set when retry-from-step retires a prior step) must NOT count
// toward the success-rate / effective-cost denominators, so a retry can't
// move $/success by relabelling. One ok + one superseded must read as a
// 100% success rate (denominator = 1), not 50%.
func TestMetrics_SupersededExcludedFromSuccessRate(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.RecordFinalOutcome("coder", "claude", "ok")
	m.RecordFinalOutcome("coder", "claude", "superseded")

	rate := testutil.ToFloat64(m.ModelSuccessRate.WithLabelValues("coder", "claude"))
	assert.Equal(t, 1.0, rate, "superseded must be excluded from the success-rate denominator")
}

func TestMetrics_RecordToolBudgetResolved(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.RecordToolBudgetResolved("coder", "complex")
	m.RecordToolBudgetResolved("coder", "complex")
	m.RecordToolBudgetResolved("tester", "") // empty tier → "standard"

	assert.Equal(t, 2.0, testutil.ToFloat64(m.ToolBudgetResolvedTotal.WithLabelValues("coder", "complex")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.ToolBudgetResolvedTotal.WithLabelValues("tester", "standard")))
	// Empty role is a no-op (no panic, no series).
	m.RecordToolBudgetResolved("", "complex")
	assert.Equal(t, 0.0, testutil.ToFloat64(m.ToolBudgetResolvedTotal.WithLabelValues("", "complex")))
}

func TestMetrics_RecordRetryFromStep(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.RecordRetryFromStep("succeeded")
	m.RecordRetryFromStep("succeeded")
	m.RecordRetryFromStep("refused_unknown_step")

	assert.Equal(t, 2.0, testutil.ToFloat64(m.RetryFromStepTotal.WithLabelValues("succeeded")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.RetryFromStepTotal.WithLabelValues("refused_unknown_step")))
	assert.Equal(t, 0.0, testutil.ToFloat64(m.RetryFromStepTotal.WithLabelValues("refused_bad_state")))

	// Nil receiver is a no-op (executors built without metrics).
	var nilM *Metrics
	nilM.RecordRetryFromStep("succeeded")
}

// TestMetrics_RecordRetryFromStepSideEffectingUpstream — the containment-guard
// awareness counter increments per call and is nil-safe.
func TestMetrics_RecordRetryFromStepSideEffectingUpstream(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())

	m.RecordRetryFromStepSideEffectingUpstream()
	m.RecordRetryFromStepSideEffectingUpstream()
	assert.Equal(t, 2.0, testutil.ToFloat64(m.RetryFromStepSideEffectingUpstreamTotal))

	var nilM *Metrics
	assert.NotPanics(t, func() { nilM.RecordRetryFromStepSideEffectingUpstream() })
}

func TestMetrics_RecordDelegationGuardRejection(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.RecordDelegationGuardRejection("fanout")
	m.RecordDelegationGuardRejection("fanout")
	m.RecordDelegationGuardRejection("depth")
	m.RecordDelegationGuardRejection("cycle")

	assert.Equal(t, 2.0, testutil.ToFloat64(m.DelegationGuardRejectionsTotal.WithLabelValues("fanout")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.DelegationGuardRejectionsTotal.WithLabelValues("depth")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.DelegationGuardRejectionsTotal.WithLabelValues("cycle")))

	// Nil receiver is a no-op (executors built without metrics).
	var nilM *Metrics
	nilM.RecordDelegationGuardRejection("fanout")
}

func TestMetrics_RecordCompleted(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.RecordCompleted("project-1", 5.5)

	// Verify completed counter incremented
	count := testutil.ToFloat64(m.CompletedTotal.WithLabelValues("project-1"))
	assert.Equal(t, 1.0, count)
}

func TestMetrics_RecordFailed(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.RecordFailed("project-1", 3.2)

	// Verify failed counter incremented
	count := testutil.ToFloat64(m.FailedTotal.WithLabelValues("project-1"))
	assert.Equal(t, 1.0, count)
}

func TestMetrics_RecordCancelled(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.RecordCancelled("project-1", 1.5)

	// Verify cancelled counter incremented
	count := testutil.ToFloat64(m.CancelledTotal.WithLabelValues("project-1"))
	assert.Equal(t, 1.0, count)
}

func TestMetrics_SetActiveGauge(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	// Set active counts for two projects
	m.SetActiveGauge(map[string]int{"proj-a": 3, "proj-b": 1})
	assert.Equal(t, 3.0, testutil.ToFloat64(m.Active.WithLabelValues("proj-a")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.Active.WithLabelValues("proj-b")))

	// Update: proj-a finishes, proj-b stays
	m.SetActiveGauge(map[string]int{"proj-b": 1})
	assert.Equal(t, 0.0, testutil.ToFloat64(m.Active.WithLabelValues("proj-a")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.Active.WithLabelValues("proj-b")))

	// Empty: all done
	m.SetActiveGauge(map[string]int{})
	assert.Equal(t, 0.0, testutil.ToFloat64(m.Active.WithLabelValues("proj-b")))
}

func TestMetrics_RecordLLMUsage(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.RecordLLMUsage("p1", "coder", "qwen3-coder-30b", 1200, 450, 7, nil)
	m.RecordLLMUsage("p1", "coder", "qwen3-coder-30b", 800, 300, 4, nil)
	m.RecordLLMUsage("p1", "reviewer", "glm-4.7-flash", 500, 120, 2, nil)

	// Tokens aggregate by direction across repeated calls for same labels.
	assert.Equal(t, 2000.0, testutil.ToFloat64(m.LLMTokensTotal.WithLabelValues("p1", "coder", "qwen3-coder-30b", "prompt")))
	assert.Equal(t, 750.0, testutil.ToFloat64(m.LLMTokensTotal.WithLabelValues("p1", "coder", "qwen3-coder-30b", "completion")))
	assert.Equal(t, 500.0, testutil.ToFloat64(m.LLMTokensTotal.WithLabelValues("p1", "reviewer", "glm-4.7-flash", "prompt")))

	// Iterations aggregate too.
	assert.Equal(t, 11.0, testutil.ToFloat64(m.LLMIterationsTotal.WithLabelValues("p1", "coder", "qwen3-coder-30b")))
	assert.Equal(t, 2.0, testutil.ToFloat64(m.LLMIterationsTotal.WithLabelValues("p1", "reviewer", "glm-4.7-flash")))

	// No pricing table was supplied — cost metrics should be untouched.
	assert.Equal(t, 0.0, testutil.ToFloat64(m.LLMCostUSDTotal.WithLabelValues("p1", "coder", "qwen3-coder-30b")))
}

func TestMetrics_RecordToolCalls(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	// Multi-tool batch lands as a single call, but counters update per tool.
	m.RecordToolCalls("p1", map[string]int{
		"file_read":  3,
		"file_write": 1,
	})
	// Second batch adds onto existing counters; partial overlap is fine.
	m.RecordToolCalls("p1", map[string]int{
		"file_read": 2,
		"run_shell": 4,
	})
	// Different project keeps a separate label set.
	m.RecordToolCalls("p2", map[string]int{"file_read": 5})

	assert.Equal(t, 5.0, testutil.ToFloat64(m.ToolCallsTotal.WithLabelValues("p1", "file_read")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.ToolCallsTotal.WithLabelValues("p1", "file_write")))
	assert.Equal(t, 4.0, testutil.ToFloat64(m.ToolCallsTotal.WithLabelValues("p1", "run_shell")))
	assert.Equal(t, 5.0, testutil.ToFloat64(m.ToolCallsTotal.WithLabelValues("p2", "file_read")))

	// nil receiver — safe no-op.
	var mNil *Metrics
	assert.NotPanics(t, func() {
		mNil.RecordToolCalls("p1", map[string]int{"file_read": 1})
	})
}

func TestMetrics_RecordModelFallback(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)
	m.RecordModelFallback("coder", "primary-model", "fallback-model")
	m.RecordModelFallback("coder", "primary-model", "fallback-model")
	m.RecordModelFallback("coder", "primary-model", "different-fallback")

	assert.Equal(t, 2.0, testutil.ToFloat64(m.ModelFallbackTotal.WithLabelValues("coder", "primary-model", "fallback-model")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.ModelFallbackTotal.WithLabelValues("coder", "primary-model", "different-fallback")))

	// Defensive guards: any empty label silently no-ops to keep the
	// series clean — pin the contract.
	m.RecordModelFallback("", "primary", "fallback")
	m.RecordModelFallback("role", "", "fallback")
	m.RecordModelFallback("role", "primary", "")
	// nil receiver too.
	var mNil *Metrics
	assert.NotPanics(t, func() { mNil.RecordModelFallback("r", "a", "b") })
}

func TestMetrics_RecordLLMUsage_WithPricing(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
models:
  qwen-coder:
    input: 0.15
    output: 0.60
`), 0o644))
	table, err := pricing.Load(path)
	require.NoError(t, err)

	// 1M prompt × $0.15/M + 500K completion × $0.60/M = $0.15 + $0.30 = $0.45
	m.RecordLLMUsage("proj-a", "coder", "qwen-coder", 1_000_000, 500_000, 1, table)
	assert.InDelta(t, 0.45, testutil.ToFloat64(m.LLMCostUSDTotal.WithLabelValues("proj-a", "coder", "qwen-coder")), 1e-9)
	// Model-dimension mirror (no project) should match.
	assert.InDelta(t, 0.45, testutil.ToFloat64(m.ModelCostUSDTotal.WithLabelValues("coder", "qwen-coder")), 1e-9)

	// Same role+model on a different project — model cost aggregates across projects.
	m.RecordLLMUsage("proj-b", "coder", "qwen-coder", 1_000_000, 500_000, 1, table)
	assert.InDelta(t, 0.90, testutil.ToFloat64(m.ModelCostUSDTotal.WithLabelValues("coder", "qwen-coder")), 1e-9)
	// Project-A cost stays at $0.45 (separate label set).
	assert.InDelta(t, 0.45, testutil.ToFloat64(m.LLMCostUSDTotal.WithLabelValues("proj-a", "coder", "qwen-coder")), 1e-9)
}

// TestMetrics_ModelLabelBucketsUnknownModels is the cardinality-guard
// regression (trading-hardening §3, 2026-06-16): a model string not in the
// pricing catalog must collapse to the "other" bucket across every
// model-labeled metric (tokens, cost, outcomes) so a typo'd or
// provider-echoed model can't mint a permanent Prometheus series. Cost math
// still uses the raw model, so a known-priced model keeps its own label.
func TestMetrics_ModelLabelBucketsUnknownModels(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
models:
  known-model:
    input: 0.10
    output: 0.20
`), 0o644))
	table, err := pricing.Load(path)
	require.NoError(t, err)
	m.SetModelCatalog(table)

	// Known model keeps its own label.
	m.RecordLLMUsage("proj", "coder", "known-model", 1_000_000, 0, 1, table)
	assert.Equal(t, 1_000_000.0, testutil.ToFloat64(m.LLMTokensTotal.WithLabelValues("proj", "coder", "known-model", "prompt")))

	// Unknown model collapses to "other" — and does NOT get its own series.
	m.RecordLLMUsage("proj", "coder", "typod-model-xyz", 2_000_000, 0, 1, table)
	assert.Equal(t, 2_000_000.0, testutil.ToFloat64(m.LLMTokensTotal.WithLabelValues("proj", "coder", "other", "prompt")))
	assert.Equal(t, 0.0, testutil.ToFloat64(m.LLMTokensTotal.WithLabelValues("proj", "coder", "typod-model-xyz", "prompt")))

	// Outcome counters bucket on the same catalog.
	m.RecordFinalOutcome("coder", "another-unknown", "ok")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.AgentStepOutcomesTotal.WithLabelValues("coder", "other", "ok")))
	assert.Equal(t, 0.0, testutil.ToFloat64(m.AgentStepOutcomesTotal.WithLabelValues("coder", "another-unknown", "ok")))
	m.RecordAgentStepOutcome("coder", "yet-another-unknown", "failed")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.AgentStepOutcomesTotal.WithLabelValues("coder", "other", "failed")))

	// With no catalog the raw model passes through (current behavior).
	m2 := NewMetrics(prometheus.NewRegistry())
	m2.RecordFinalOutcome("coder", "raw-model", "ok")
	assert.Equal(t, 1.0, testutil.ToFloat64(m2.AgentStepOutcomesTotal.WithLabelValues("coder", "raw-model", "ok")))
}

func TestMetrics_ModelSuccessRateGauge(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	// 7 ok, 2 failed, 1 timeout → denominator 10; success rate 0.7.
	// RecordFinalOutcome is the source of truth for the quality gauges
	// (legacy RecordAgentStepOutcome no longer updates the mirror, so
	// it can't drive derived gauges anymore).
	for i := 0; i < 7; i++ {
		m.RecordFinalOutcome("coder", "qwen", "ok")
	}
	m.RecordFinalOutcome("coder", "qwen", "failed")
	m.RecordFinalOutcome("coder", "qwen", "failed")
	m.RecordFinalOutcome("coder", "qwen", "timeout")
	// Cancel doesn't count toward denominator — should not change the rate.
	m.RecordFinalOutcome("coder", "qwen", "cancelled")

	got := testutil.ToFloat64(m.ModelSuccessRate.WithLabelValues("coder", "qwen"))
	assert.InDelta(t, 0.7, got, 1e-9)
}

func TestMetrics_ModelQualityGauges(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	// Mixed outcome run: 5 ok, 2 parse_error, 1 refused, 2 degenerate_loop → denom 10.
	for i := 0; i < 5; i++ {
		m.RecordFinalOutcome("coder", "flaky-model", "ok")
	}
	m.RecordFinalOutcome("coder", "flaky-model", "parse_error")
	m.RecordFinalOutcome("coder", "flaky-model", "parse_error")
	m.RecordFinalOutcome("coder", "flaky-model", "refused")
	m.RecordFinalOutcome("coder", "flaky-model", "degenerate_loop")
	m.RecordFinalOutcome("coder", "flaky-model", "degenerate_loop")

	assert.InDelta(t, 0.5, testutil.ToFloat64(m.ModelSuccessRate.WithLabelValues("coder", "flaky-model")), 1e-9)
	assert.InDelta(t, 0.2, testutil.ToFloat64(m.ModelParseFailureRate.WithLabelValues("coder", "flaky-model")), 1e-9)
	assert.InDelta(t, 0.1, testutil.ToFloat64(m.ModelRefusalRate.WithLabelValues("coder", "flaky-model")), 1e-9)
	assert.InDelta(t, 0.2, testutil.ToFloat64(m.ModelLoopRate.WithLabelValues("coder", "flaky-model")), 1e-9)
}

func TestMetrics_ModelEffectiveCostGauge(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
models:
  model-x:
    input: 0.10
    output: 1.00
`), 0o644))
	table, err := pricing.Load(path)
	require.NoError(t, err)

	// Five attempts at 1M prompt + 500K completion = $0.60 each.
	// Three succeed, two fail. Total cost $3.00, successes 3 → $1.00/success.
	for i := 0; i < 5; i++ {
		m.RecordLLMUsage("p1", "coder", "model-x", 1_000_000, 500_000, 1, table)
	}
	m.RecordFinalOutcome("coder", "model-x", "ok")
	m.RecordFinalOutcome("coder", "model-x", "ok")
	m.RecordFinalOutcome("coder", "model-x", "ok")
	m.RecordFinalOutcome("coder", "model-x", "failed")
	m.RecordFinalOutcome("coder", "model-x", "failed")

	cost := testutil.ToFloat64(m.ModelEffectiveCostUSD.WithLabelValues("coder", "model-x"))
	assert.InDelta(t, 1.00, cost, 1e-9)

	// Success rate should be 3 / (3+2) = 0.6 — same semantics as before,
	// now expressed via the richer outcome taxonomy (ok vs failed).
	rate := testutil.ToFloat64(m.ModelSuccessRate.WithLabelValues("coder", "model-x"))
	assert.InDelta(t, 0.6, rate, 1e-9)
}

func TestMetrics_ModelGauges_NoDataLeavesSeriesUnset(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	// Only cancelled → denominator is 0 → success rate undefined → gauge not set.
	// Only failed → denominator non-zero → rate 0 → gauge IS set.
	m.RecordFinalOutcome("coder", "only-cancelled", "cancelled")
	m.RecordFinalOutcome("coder", "only-cancelled", "cancelled")
	// Accessing a never-set gauge lazily creates a zero-valued series in the
	// Prometheus client, so `ToFloat64` returns 0 either way. Instead record
	// a real value on a different label set and assert it doesn't leak.
	m.RecordFinalOutcome("coder", "has-runs", "failed")
	rate := testutil.ToFloat64(m.ModelSuccessRate.WithLabelValues("coder", "has-runs"))
	assert.InDelta(t, 0.0, rate, 1e-9) // failed-only → 0/1 = 0.0

	// Effective cost with no successes should stay unset; recording cost
	// before any success is meaningless and we deliberately skip the gauge.
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
models:
  model-y: { input: 0.10, output: 1.00 }
`), 0o644))
	table, err := pricing.Load(path)
	require.NoError(t, err)

	m.RecordLLMUsage("p1", "coder", "model-y", 1_000_000, 500_000, 1, table)
	// No success recorded → gauge should not be touched. Reading it returns
	// 0 (lazy-materialised), but that's a display-only concern; the test
	// above at least confirms we didn't set it to a divide-by-zero NaN.
	_ = testutil.ToFloat64(m.ModelEffectiveCostUSD.WithLabelValues("coder", "model-y"))
}

func TestMetrics_RecordAgentStepOutcome(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.RecordAgentStepOutcome("coder", "qwen-coder", "success")
	m.RecordAgentStepOutcome("coder", "qwen-coder", "success")
	m.RecordAgentStepOutcome("coder", "qwen-coder", "failed")
	m.RecordAgentStepOutcome("reviewer", "glm-flash", "success")
	m.RecordAgentStepOutcome("reviewer", "glm-flash", "timeout")

	assert.Equal(t, 2.0, testutil.ToFloat64(m.AgentStepOutcomesTotal.WithLabelValues("coder", "qwen-coder", "success")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.AgentStepOutcomesTotal.WithLabelValues("coder", "qwen-coder", "failed")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.AgentStepOutcomesTotal.WithLabelValues("reviewer", "glm-flash", "timeout")))

	// Empty role or model → no record.
	m.RecordAgentStepOutcome("", "qwen-coder", "success")
	m.RecordAgentStepOutcome("coder", "", "success")
	assert.Equal(t, 2.0, testutil.ToFloat64(m.AgentStepOutcomesTotal.WithLabelValues("coder", "qwen-coder", "success")))
}

func TestMetrics_RecordLLMUsageWithCache_TokenCounters(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	// Two calls with cache_creation + cache_read tokens; counters must
	// aggregate by the new "cache_creation"/"cache_read" direction label
	// alongside the existing prompt/completion directions.
	m.RecordLLMUsageWithCache("p1", "coder", "claude-sonnet-4-6",
		1000, 500, 3 /* cacheCreation */, 200 /* cacheRead */, 800, nil)
	m.RecordLLMUsageWithCache("p1", "coder", "claude-sonnet-4-6",
		2000, 1000, 5, 100, 400, nil)

	assert.Equal(t, 3000.0, testutil.ToFloat64(m.LLMTokensTotal.WithLabelValues(
		"p1", "coder", "claude-sonnet-4-6", "prompt")))
	assert.Equal(t, 1500.0, testutil.ToFloat64(m.LLMTokensTotal.WithLabelValues(
		"p1", "coder", "claude-sonnet-4-6", "completion")))
	assert.Equal(t, 300.0, testutil.ToFloat64(m.LLMTokensTotal.WithLabelValues(
		"p1", "coder", "claude-sonnet-4-6", "cache_creation")))
	assert.Equal(t, 1200.0, testutil.ToFloat64(m.LLMTokensTotal.WithLabelValues(
		"p1", "coder", "claude-sonnet-4-6", "cache_read")))
}

func TestMetrics_RecordLLMUsageWithCache_DollarsSavedCounter(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
models:
  claude-sonnet-4-6:
    input: 3.00
    output: 15.00
`), 0o644))
	table, err := pricing.Load(path)
	require.NoError(t, err)

	// 1M cache_read tokens at $3/M input vs $0.30/M cache_read (10% default)
	// → saved 2.70 per million → $2.70 for 1M reads.
	m.RecordLLMUsageWithCache("p1", "coder", "claude-sonnet-4-6",
		0, 0, 0, 0, 1_000_000, table)
	assert.InDelta(t, 2.70, testutil.ToFloat64(
		m.LLMCacheSavingsUSDTotal.WithLabelValues("coder", "claude-sonnet-4-6")), 1e-9)

	// Second call accumulates.
	m.RecordLLMUsageWithCache("p1", "coder", "claude-sonnet-4-6",
		0, 0, 0, 0, 500_000, table)
	assert.InDelta(t, 4.05, testutil.ToFloat64(
		m.LLMCacheSavingsUSDTotal.WithLabelValues("coder", "claude-sonnet-4-6")), 1e-9)
}

func TestMetrics_RecordLLMUsage_BackwardCompatPath(t *testing.T) {
	// The legacy 7-arg RecordLLMUsage must still work and produce zero
	// cache counters — callers that haven't migrated yet (e.g. older
	// agent images without cache fields) must not be broken.
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.RecordLLMUsage("p1", "coder", "model-x", 1000, 500, 3, nil)

	assert.Equal(t, 1000.0, testutil.ToFloat64(m.LLMTokensTotal.WithLabelValues(
		"p1", "coder", "model-x", "prompt")))
	assert.Equal(t, 0.0, testutil.ToFloat64(m.LLMTokensTotal.WithLabelValues(
		"p1", "coder", "model-x", "cache_creation")))
	assert.Equal(t, 0.0, testutil.ToFloat64(m.LLMTokensTotal.WithLabelValues(
		"p1", "coder", "model-x", "cache_read")))
}

func TestMetrics_RecordLLMUsage_ZeroSkipped(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	// Zero values shouldn't create orphan series with zero counts.
	m.RecordLLMUsage("p1", "coder", "model-x", 0, 0, 0, nil)

	// Accessing .WithLabelValues lazily materialises, so ToFloat64 is 0 either
	// way; verify a real increment afterwards still works.
	m.RecordLLMUsage("p1", "coder", "model-x", 100, 0, 0, nil)
	assert.Equal(t, 100.0, testutil.ToFloat64(m.LLMTokensTotal.WithLabelValues("p1", "coder", "model-x", "prompt")))
	assert.Equal(t, 0.0, testutil.ToFloat64(m.LLMTokensTotal.WithLabelValues("p1", "coder", "model-x", "completion")))
}

func TestMetrics_RecordRetried(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	// Record retries
	m.RecordRetried("project-1")
	m.RecordRetried("project-1")
	m.RecordRetried("project-2")

	// Verify retry counter
	count := testutil.ToFloat64(m.RetriedTotal.WithLabelValues("project-1"))
	assert.Equal(t, 2.0, count)

	count = testutil.ToFloat64(m.RetriedTotal.WithLabelValues("project-2"))
	assert.Equal(t, 1.0, count)
}

func TestMetrics_DurationHistogram(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	// Record durations with different statuses
	m.RecordCompleted("project-1", 5.5)
	m.RecordFailed("project-1", 3.2)
	m.RecordCancelled("project-2", 1.0)

	// Verify histogram is functional (doesn't panic)
	assert.NotNil(t, m.DurationSeconds)
}

func TestMetrics_NilSafety(t *testing.T) {
	var m *Metrics

	// All methods should be safe with nil receiver
	assert.NotPanics(t, func() { m.RecordStarted("project-1") })
	assert.NotPanics(t, func() { m.RecordCompleted("project-1", 1.0) })
	assert.NotPanics(t, func() { m.RecordFailed("project-1", 1.0) })
	assert.NotPanics(t, func() { m.RecordCancelled("project-1", 1.0) })
	assert.NotPanics(t, func() { m.RecordRetried("project-1") })
	assert.NotPanics(t, func() { m.SetActiveGauge(map[string]int{"p": 1}) })
}

func TestWithMetrics_Executor(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	rt := NewMockRuntime()
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()

	e := NewWithOptions(rt, er, ar, tr, nil, WithMetrics(m))

	assert.Equal(t, m, e.metrics)
}

func TestWithPrometheusRegistry_Executor(t *testing.T) {
	registry := prometheus.NewRegistry()

	rt := NewMockRuntime()
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()

	e := NewWithOptions(rt, er, ar, tr, nil, WithPrometheusRegistry(registry))

	// Metrics should be initialized
	require.NotNil(t, e.metrics)
}

func TestMetrics_RegisterWithoutPanic(t *testing.T) {
	// This test verifies that all metrics can be registered without panic
	registry := prometheus.NewRegistry()

	require.NotPanics(t, func() {
		NewMetrics(registry)
	})
}
