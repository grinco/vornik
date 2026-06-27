package autonomy

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	projectID := "proj-1"
	m.EvaluationsTotal.WithLabelValues(projectID).Add(2)
	m.TasksCreated.WithLabelValues(projectID).Inc()
	m.NoActionTotal.WithLabelValues(projectID).Add(3)
	m.ErrorsTotal.WithLabelValues(projectID).Inc()
	m.EvalDuration.WithLabelValues(projectID).Observe(5.5)

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

	families, err := reg.Gather()
	require.NoError(t, err)

	expected := map[string]struct {
		help     string
		typeName string
	}{
		"vornik_autonomy_evaluations_total": {
			help:     "Total autonomous evaluations run.",
			typeName: "COUNTER",
		},
		"vornik_autonomy_tasks_created_total": {
			help:     "Total tasks created by autonomous lead.",
			typeName: "COUNTER",
		},
		"vornik_autonomy_no_action_total": {
			help:     "Total evaluations where the lead decided no action was needed.",
			typeName: "COUNTER",
		},
		"vornik_autonomy_errors_total": {
			help:     "Total autonomous evaluation errors.",
			typeName: "COUNTER",
		},
		"vornik_autonomy_evaluation_duration_seconds": {
			help:     "Duration of each autonomous evaluation.",
			typeName: "HISTOGRAM",
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
			require.Len(t, metric.GetLabel(), 1)
			assert.Equal(t, "project_id", metric.GetLabel()[0].GetName())
		}
	}

	assert.Equal(t, len(expected), seen)
}
