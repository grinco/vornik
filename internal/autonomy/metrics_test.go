package autonomy

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/registry"
)

func TestNewMetrics_RegistersAndCollects(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	require.NotNil(t, m)
	require.NotNil(t, m.EvaluationsTotal)
	require.NotNil(t, m.TasksCreated)
	require.NotNil(t, m.NoActionTotal)
	require.NotNil(t, m.ErrorsTotal)
	require.NotNil(t, m.EvalDuration)
	// The three series the 2026-09-10 degradation surface added. They
	// were left out of this enumeration when they shipped, so a series
	// silently dropped from MustRegister would not have been caught by
	// the test whose whole job is enumerating what is registered.
	require.NotNil(t, m.EvaluationOutcomesTotal)
	require.NotNil(t, m.FeedLagSeconds)
	require.NotNil(t, m.FeedCadenceBreachTotal)

	projectID := "proj-1"
	m.EvaluationsTotal.WithLabelValues(projectID).Add(2)
	m.TasksCreated.WithLabelValues(projectID).Inc()
	m.NoActionTotal.WithLabelValues(projectID).Add(3)
	m.ErrorsTotal.WithLabelValues(projectID).Inc()
	m.EvalDuration.WithLabelValues(projectID).Observe(5.5)
	m.EvaluationOutcomesTotal.WithLabelValues(projectID, "CREATED").Inc()
	m.FeedLagSeconds.WithLabelValues(projectID, "czech-news").Set(70920)
	m.FeedCadenceBreachTotal.WithLabelValues(projectID, "czech-news", "slow").Inc()

	families, err := reg.Gather()
	require.NoError(t, err)

	found := map[string]bool{}
	for _, mf := range families {
		found[mf.GetName()] = true
	}

	assert.True(t, found["vornik_autonomy_evaluations_total"])
	assert.True(t, found["vornik_autonomy_tasks_created_total"])
	assert.True(t, found["vornik_autonomy_no_action_total"])
	assert.True(t, found["vornik_autonomy_errors_total"])
	assert.True(t, found["vornik_autonomy_evaluation_duration_seconds"])
	assert.True(t, found["vornik_autonomy_evaluation_outcomes_total"])
	assert.True(t, found["vornik_autonomy_feed_lag_seconds"])
	assert.True(t, found["vornik_autonomy_feed_cadence_breach_total"])
}

func TestNewMetrics_DuplicateRegistrationPanics(t *testing.T) {
	reg := prometheus.NewRegistry()
	_ = NewMetrics(reg)

	assert.Panics(t, func() {
		_ = NewMetrics(reg)
	})
}

func TestNewMetrics_UsesExpectedHistogramBuckets(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	projectID := "proj-2"
	m.EvalDuration.WithLabelValues(projectID).Observe(0.5)
	m.EvalDuration.WithLabelValues(projectID).Observe(250)

	families, err := reg.Gather()
	require.NoError(t, err)

	var durationMetricFound bool
	for _, mf := range families {
		if mf.GetName() != "vornik_autonomy_evaluation_duration_seconds" {
			continue
		}
		durationMetricFound = true
		require.Len(t, mf.GetMetric(), 1)

		h := mf.GetMetric()[0].GetHistogram()
		require.NotNil(t, h)
		assert.EqualValues(t, 2, h.GetSampleCount())
		assert.InDelta(t, 250.5, h.GetSampleSum(), 0.0001)

		buckets := h.GetBucket()
		require.Len(t, buckets, 7)
		assert.InDelta(t, 1, buckets[0].GetUpperBound(), 0.0001)
		assert.InDelta(t, 5, buckets[1].GetUpperBound(), 0.0001)
		assert.InDelta(t, 10, buckets[2].GetUpperBound(), 0.0001)
		assert.InDelta(t, 30, buckets[3].GetUpperBound(), 0.0001)
		assert.InDelta(t, 60, buckets[4].GetUpperBound(), 0.0001)
		assert.InDelta(t, 120, buckets[5].GetUpperBound(), 0.0001)
		assert.InDelta(t, 300, buckets[6].GetUpperBound(), 0.0001)
	}

	assert.True(t, durationMetricFound)
}

func TestNewMetrics_NilRegistryPanics(t *testing.T) {
	assert.Panics(t, func() {
		_ = NewMetrics(nil)
	})
}

func TestNewMetrics_HelpTextAndLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	projectID := "proj-help"
	m.EvaluationsTotal.WithLabelValues(projectID).Inc()
	m.TasksCreated.WithLabelValues(projectID).Inc()
	m.NoActionTotal.WithLabelValues(projectID).Inc()
	m.ErrorsTotal.WithLabelValues(projectID).Inc()
	m.EvalDuration.WithLabelValues(projectID).Observe(1.0)
	m.EvaluationOutcomesTotal.WithLabelValues(projectID, "CREATED").Inc()
	m.FeedLagSeconds.WithLabelValues(projectID, "czech-news").Set(70920)
	m.FeedCadenceBreachTotal.WithLabelValues(projectID, "czech-news", "slow").Inc()

	families, err := reg.Gather()
	require.NoError(t, err)

	// labels is the EXACT label set, in order: the three series added by
	// the degradation surface are multi-label, which is why this table
	// could not simply assert "one label named project_id" any more --
	// and why those series were quietly left out of it when they
	// shipped. A label set is part of a series' identity; a test that
	// enumerates registered metrics but skips the newest three certifies
	// a registry it did not look at.
	expected := map[string]struct {
		help     string
		typeName string
		labels   []string
	}{
		"vornik_autonomy_evaluations_total": {
			help:     "Total autonomous evaluations run.",
			typeName: "COUNTER",
			labels:   []string{"project_id"},
		},
		"vornik_autonomy_tasks_created_total": {
			help:     "Total tasks created by autonomous lead.",
			typeName: "COUNTER",
			labels:   []string{"project_id"},
		},
		"vornik_autonomy_no_action_total": {
			help:     "Total evaluations where the lead decided no action was needed.",
			typeName: "COUNTER",
			labels:   []string{"project_id"},
		},
		"vornik_autonomy_errors_total": {
			help:     "Total autonomous evaluation errors.",
			typeName: "COUNTER",
			labels:   []string{"project_id"},
		},
		"vornik_autonomy_evaluation_duration_seconds": {
			help:     "Duration of each autonomous evaluation.",
			typeName: "HISTOGRAM",
			labels:   []string{"project_id"},
		},
		"vornik_autonomy_evaluation_outcomes_total": {
			help:     "Autonomy ticks by recorded outcome (CREATED, NO_ACTION, RATE_LIMITED, ...).",
			typeName: "COUNTER",
			labels:   []string{"outcome", "project_id"},
		},
		"vornik_autonomy_feed_lag_seconds": {
			help:     "Seconds since a declared autonomy feed last ran.",
			typeName: "GAUGE",
			labels:   []string{"project_id", "slug"},
		},
		"vornik_autonomy_feed_cadence_breach_total": {
			help:     "Declared-feed cadence violations, by direction (slow|fast).",
			typeName: "COUNTER",
			labels:   []string{"direction", "project_id", "slug"},
		},
	}

	seen := 0
	for _, mf := range families {
		exp, ok := expected[mf.GetName()]
		if !ok {
			continue
		}
		seen++
		assert.Equal(t, exp.help, mf.GetHelp())
		assert.Equal(t, exp.typeName, mf.GetType().String())
		require.NotEmpty(t, mf.GetMetric())
		for _, metric := range mf.GetMetric() {
			names := make([]string, 0, len(metric.GetLabel()))
			for _, lbl := range metric.GetLabel() {
				names = append(names, lbl.GetName())
			}
			// Gather() sorts label pairs by name, which is why the
			// expected sets above are alphabetical rather than in
			// declaration order.
			assert.Equal(t, exp.labels, names, "label set of %s", mf.GetName())
		}
	}

	assert.Equal(t, len(expected), seen)
}

func TestEvaluationOutcomesTotal_LabelledByOutcome(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.EvaluationOutcomesTotal.WithLabelValues("assistant", "CREATED").Inc()
	m.EvaluationOutcomesTotal.WithLabelValues("assistant", "CREATED").Inc()
	m.EvaluationOutcomesTotal.WithLabelValues("assistant", "NO_ACTION").Inc()

	if got := testutil.ToFloat64(
		m.EvaluationOutcomesTotal.WithLabelValues("assistant", "CREATED")); got != 2 {
		t.Errorf("CREATED = %v, want 2", got)
	}
	if got := testutil.ToFloat64(
		m.EvaluationOutcomesTotal.WithLabelValues("assistant", "NO_ACTION")); got != 1 {
		t.Errorf("NO_ACTION = %v, want 1", got)
	}
}

// The existing series must keep its single-label shape: adding an
// `outcome` label to it would change its identity and silently break
// every recording rule and dashboard panel already reading it.
func TestEvaluationsTotal_KeepsSingleLabel(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	// One label value only. A two-value call panics if the label set grew.
	m.EvaluationsTotal.WithLabelValues("assistant").Inc()
	if got := testutil.ToFloat64(m.EvaluationsTotal.WithLabelValues("assistant")); got != 1 {
		t.Errorf("evaluations_total = %v, want 1", got)
	}
}

// TestRecordEvaluation_EmitsMetricsEvenWhenEvalRepoIsNil is a regression
// test for the restructuring in recordEvaluation that moves the metrics
// emission above the evalRepo nil-check. This ensures metrics are recorded
// even when audit persistence is disabled. A future edit moving the metrics
// block below the "if m.evalRepo == nil { return }" statement would cause
// this test to fail, signaling the regression.
func TestRecordEvaluation_EmitsMetricsEvenWhenEvalRepoIsNil(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)

	// Create a Manager with metrics but WITHOUT an evaluation repository.
	// WithEvaluationRepository is deliberately omitted.
	m := New(
		nil, // client unused
		&registry.Registry{},
		nil, // taskRepo unused
		nil, // execRepo unused
		WithMetrics(metrics),
		// NO WithEvaluationRepository call
	)

	// Call recordEvaluation directly with a known outcome.
	m.recordEvaluation(context.Background(), evalRecord{
		projectID: "test-project",
		outcome:   "CREATED",
		reason:    "test evaluation",
	})

	// Assert the metric was incremented even though evalRepo is nil.
	got := testutil.ToFloat64(
		metrics.EvaluationOutcomesTotal.WithLabelValues("test-project", "CREATED"))
	if got != 1 {
		t.Errorf("EvaluationOutcomesTotal with outcome=CREATED = %v, want 1 (evalRepo was nil)", got)
	}
}
