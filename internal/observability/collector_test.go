package observability

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/testutil/metricstest"
)

func TestNewStateCollector(t *testing.T) {
	t.Run("creates with custom registry", func(t *testing.T) {
		registry := prometheus.NewRegistry()
		sc := NewStateCollector(registry)
		require.NotNil(t, sc)
		assert.NotNil(t, sc.RegistryEntities)
		assert.NotNil(t, sc.Tasks)
		assert.NotNil(t, sc.Executions)
		assert.NotNil(t, sc.PodmanContainers)
		assert.NotNil(t, sc.PodmanHealthy)
		assert.NotNil(t, sc.DBTableRows)
		assert.NotNil(t, sc.DBTableSizeBytes)
		assert.NotNil(t, sc.ProjectBudgetCapsUSD)
		assert.NotNil(t, sc.ProjectBudgetTimezoneInfo)
		assert.NotNil(t, sc.ProjectTaskCreationLimits)
		assert.NotNil(t, sc.ProjectAutonomyMaxTasksPerHour)
	})

	t.Run("creates with nil registerer", func(t *testing.T) {
		metricstest.IsolateDefaultRegistry(t) // count-safe nil-fallback coverage
		require.NotPanics(t, func() {
			NewStateCollector(nil)
		})
	})
}

func TestStateCollector_RecordUISessionCounts(t *testing.T) {
	registry := prometheus.NewRegistry()
	sc := NewStateCollector(registry)

	sc.RecordUISessionCounts(7, 3, 12)
	assert.Equal(t, 7.0, testutil.ToFloat64(sc.UISessions.WithLabelValues("active")))
	assert.Equal(t, 3.0, testutil.ToFloat64(sc.UISessions.WithLabelValues("expired_not_revoked")))
	assert.Equal(t, 12.0, testutil.ToFloat64(sc.UISessions.WithLabelValues("revoked")))

	// Reset semantics: a re-record overwrites, doesn't accumulate.
	sc.RecordUISessionCounts(5, 0, 0)
	assert.Equal(t, 5.0, testutil.ToFloat64(sc.UISessions.WithLabelValues("active")))
	assert.Equal(t, 0.0, testutil.ToFloat64(sc.UISessions.WithLabelValues("expired_not_revoked")))
}

func TestStateCollector_RecordRegistryCounts(t *testing.T) {
	registry := prometheus.NewRegistry()
	sc := NewStateCollector(registry)

	sc.RecordRegistryCounts(3, 2, 5)

	assert.Equal(t, 3.0, testutil.ToFloat64(sc.RegistryEntities.WithLabelValues("project")))
	assert.Equal(t, 2.0, testutil.ToFloat64(sc.RegistryEntities.WithLabelValues("swarm")))
	assert.Equal(t, 5.0, testutil.ToFloat64(sc.RegistryEntities.WithLabelValues("workflow")))
}

func TestStateCollector_RecordTaskCounts(t *testing.T) {
	registry := prometheus.NewRegistry()
	sc := NewStateCollector(registry)

	counts := map[string]map[string]int64{
		"proj-a": {
			"QUEUED":    10,
			"RUNNING":   3,
			"COMPLETED": 50,
		},
		"proj-b": {
			"PENDING": 2,
			"FAILED":  1,
		},
	}
	sc.RecordTaskCounts(counts)

	assert.Equal(t, 10.0, testutil.ToFloat64(sc.Tasks.WithLabelValues("proj-a", "QUEUED")))
	assert.Equal(t, 3.0, testutil.ToFloat64(sc.Tasks.WithLabelValues("proj-a", "RUNNING")))
	assert.Equal(t, 50.0, testutil.ToFloat64(sc.Tasks.WithLabelValues("proj-a", "COMPLETED")))
	assert.Equal(t, 2.0, testutil.ToFloat64(sc.Tasks.WithLabelValues("proj-b", "PENDING")))
	assert.Equal(t, 1.0, testutil.ToFloat64(sc.Tasks.WithLabelValues("proj-b", "FAILED")))

	// Reset clears stale labels
	sc.RecordTaskCounts(map[string]map[string]int64{
		"proj-a": {"RUNNING": 1},
	})
	assert.Equal(t, 1.0, testutil.ToFloat64(sc.Tasks.WithLabelValues("proj-a", "RUNNING")))
	// Old label set should be gone (reads as 0 after reset)
	assert.Equal(t, 0.0, testutil.ToFloat64(sc.Tasks.WithLabelValues("proj-b", "PENDING")))
}

func TestStateCollector_RecordExecutionCounts(t *testing.T) {
	registry := prometheus.NewRegistry()
	sc := NewStateCollector(registry)

	counts := map[string]map[string]int64{
		"proj-a": {
			"RUNNING":   2,
			"COMPLETED": 15,
		},
	}
	sc.RecordExecutionCounts(counts)

	assert.Equal(t, 2.0, testutil.ToFloat64(sc.Executions.WithLabelValues("proj-a", "RUNNING")))
	assert.Equal(t, 15.0, testutil.ToFloat64(sc.Executions.WithLabelValues("proj-a", "COMPLETED")))
}

func TestStateCollector_RecordPodmanContainers(t *testing.T) {
	registry := prometheus.NewRegistry()
	sc := NewStateCollector(registry)

	counts := map[string]map[string]int{
		"proj-a": {"running": 2, "exited": 1},
		"proj-b": {"running": 1},
	}
	sc.RecordPodmanContainers(counts, true)

	assert.Equal(t, 2.0, testutil.ToFloat64(sc.PodmanContainers.WithLabelValues("proj-a", "running")))
	assert.Equal(t, 1.0, testutil.ToFloat64(sc.PodmanContainers.WithLabelValues("proj-a", "exited")))
	assert.Equal(t, 1.0, testutil.ToFloat64(sc.PodmanContainers.WithLabelValues("proj-b", "running")))
	assert.Equal(t, 1.0, testutil.ToFloat64(sc.PodmanHealthy))

	// Unhealthy state
	sc.RecordPodmanContainers(nil, false)
	assert.Equal(t, 0.0, testutil.ToFloat64(sc.PodmanHealthy))
}

func TestStateCollector_RecordTableStats(t *testing.T) {
	registry := prometheus.NewRegistry()
	sc := NewStateCollector(registry)

	rows := map[string]int64{"tasks": 1500, "executions": 300, "artifacts": 42}
	sizes := map[string]int64{"tasks": 2097152, "executions": 524288, "artifacts": 131072}
	sc.RecordTableStats(rows, sizes)

	assert.Equal(t, 1500.0, testutil.ToFloat64(sc.DBTableRows.WithLabelValues("tasks")))
	assert.Equal(t, 300.0, testutil.ToFloat64(sc.DBTableRows.WithLabelValues("executions")))
	assert.Equal(t, 42.0, testutil.ToFloat64(sc.DBTableRows.WithLabelValues("artifacts")))
	assert.Equal(t, 2097152.0, testutil.ToFloat64(sc.DBTableSizeBytes.WithLabelValues("tasks")))
}

func TestStateCollector_RecordProjectFinancialControls(t *testing.T) {
	registry := prometheus.NewRegistry()
	sc := NewStateCollector(registry)

	sc.RecordProjectFinancialControls([]ProjectFinancialControls{
		{
			ProjectID:               "proj-a",
			BudgetDailySoftUSD:      5,
			BudgetDailyHardUSD:      10,
			BudgetMonthlySoftUSD:    50,
			BudgetMonthlyHardUSD:    100,
			BudgetTimezone:          "Europe/Prague",
			RateLimitTasksPerMinute: 2,
			RateLimitTasksPerHour:   20,
			AutonomyMaxTasksPerHour: 3,
		},
		{
			ProjectID:            "proj-b",
			BudgetMonthlyHardUSD: 25,
		},
	})

	assert.Equal(t, 5.0, testutil.ToFloat64(sc.ProjectBudgetCapsUSD.WithLabelValues("proj-a", "daily", "soft")))
	assert.Equal(t, 10.0, testutil.ToFloat64(sc.ProjectBudgetCapsUSD.WithLabelValues("proj-a", "daily", "hard")))
	assert.Equal(t, 50.0, testutil.ToFloat64(sc.ProjectBudgetCapsUSD.WithLabelValues("proj-a", "monthly", "soft")))
	assert.Equal(t, 100.0, testutil.ToFloat64(sc.ProjectBudgetCapsUSD.WithLabelValues("proj-a", "monthly", "hard")))
	assert.Equal(t, 25.0, testutil.ToFloat64(sc.ProjectBudgetCapsUSD.WithLabelValues("proj-b", "monthly", "hard")))
	assert.Equal(t, 1.0, testutil.ToFloat64(sc.ProjectBudgetTimezoneInfo.WithLabelValues("proj-a", "Europe/Prague")))
	assert.Equal(t, 1.0, testutil.ToFloat64(sc.ProjectBudgetTimezoneInfo.WithLabelValues("proj-b", "UTC")))
	assert.Equal(t, 2.0, testutil.ToFloat64(sc.ProjectTaskCreationLimits.WithLabelValues("proj-a", "minute")))
	assert.Equal(t, 20.0, testutil.ToFloat64(sc.ProjectTaskCreationLimits.WithLabelValues("proj-a", "hour")))
	assert.Equal(t, 3.0, testutil.ToFloat64(sc.ProjectAutonomyMaxTasksPerHour.WithLabelValues("proj-a")))

	sc.RecordProjectFinancialControls([]ProjectFinancialControls{
		{ProjectID: "proj-a", BudgetDailyHardUSD: 7, BudgetTimezone: "UTC"},
	})

	assert.Equal(t, 7.0, testutil.ToFloat64(sc.ProjectBudgetCapsUSD.WithLabelValues("proj-a", "daily", "hard")))
	assert.Equal(t, 0.0, testutil.ToFloat64(sc.ProjectBudgetCapsUSD.WithLabelValues("proj-b", "monthly", "hard")))
	assert.Equal(t, 0.0, testutil.ToFloat64(sc.ProjectBudgetTimezoneInfo.WithLabelValues("proj-a", "Europe/Prague")))
}

func TestStateCollector_NilSafety(t *testing.T) {
	var sc *StateCollector

	assert.NotPanics(t, func() { sc.RecordRegistryCounts(1, 2, 3) })
	assert.NotPanics(t, func() { sc.RecordTaskCounts(nil) })
	assert.NotPanics(t, func() { sc.RecordExecutionCounts(nil) })
	assert.NotPanics(t, func() { sc.RecordPodmanContainers(nil, false) })
	assert.NotPanics(t, func() { sc.RecordTableStats(nil, nil) })
	assert.NotPanics(t, func() { sc.RecordProjectFinancialControls(nil) })
}
