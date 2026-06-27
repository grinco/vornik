package ratelimit

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

func TestMetrics_Observe_OutcomeTaxonomy(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	// allow: not blocked, not warn
	m.Observe(ScopeAPIKey, "k1", KeyDecision{RemainingTokens: 9})
	// warn: not blocked, warn flag set
	m.Observe(ScopeAPIKey, "k1", KeyDecision{Warn: true, RemainingTokens: 2})
	// block: blocked (Observe must not double-count as warn)
	m.Observe(ScopeAPIKey, "k1", KeyDecision{Blocked: true, Warn: true, RemainingTokens: 0})

	assert.Equal(t, 1.0, testutil.ToFloat64(
		m.DecisionsTotal.WithLabelValues(ScopeAPIKey, OutcomeAllow)))
	assert.Equal(t, 1.0, testutil.ToFloat64(
		m.DecisionsTotal.WithLabelValues(ScopeAPIKey, OutcomeWarn)))
	assert.Equal(t, 1.0, testutil.ToFloat64(
		m.DecisionsTotal.WithLabelValues(ScopeAPIKey, OutcomeBlock)))

	// Last gauge wins for scope_id k1.
	assert.Equal(t, 0.0, testutil.ToFloat64(
		m.RemainingTokens.WithLabelValues(ScopeAPIKey, "k1")))
}

func TestMetrics_Observe_NilSafeAndEmptyScope(t *testing.T) {
	var m *Metrics
	m.Observe(ScopeAPIKey, "k1", KeyDecision{}) // must not panic

	live := NewMetrics(prometheus.NewRegistry())
	live.Observe("", "k1", KeyDecision{Blocked: true})

	// No series should exist with an empty scope label — Observe
	// returns early.
	count := testutil.CollectAndCount(live.DecisionsTotal)
	assert.Zero(t, count, "empty scope must not create a series")
}

func TestMetrics_ObserveProject_OutcomeOnly(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.ObserveProject("p-good", Decision{MinuteCount: 5})
	m.ObserveProject("p-good", Decision{Blocked: true, Reason: "minute cap"})

	assert.Equal(t, 1.0, testutil.ToFloat64(
		m.DecisionsTotal.WithLabelValues(ScopeProject, OutcomeAllow)))
	assert.Equal(t, 1.0, testutil.ToFloat64(
		m.DecisionsTotal.WithLabelValues(ScopeProject, OutcomeBlock)))
}

func TestMetrics_RemainingTokensGauge_TracksLatestBucket(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.Observe(ScopeAPIKey, "akey-watch", KeyDecision{RemainingTokens: 100})
	m.Observe(ScopeAPIKey, "akey-watch", KeyDecision{RemainingTokens: 50})
	m.Observe(ScopeAPIKey, "akey-watch", KeyDecision{RemainingTokens: 12})

	assert.Equal(t, 12.0, testutil.ToFloat64(
		m.RemainingTokens.WithLabelValues(ScopeAPIKey, "akey-watch")))
}
