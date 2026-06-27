package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// StateCollector holds Prometheus gauges for business entity state.
// Metrics are updated periodically by the service container.
type StateCollector struct {
	// RegistryEntities tracks registered projects, swarms, and workflows.
	RegistryEntities *prometheus.GaugeVec

	// Tasks tracks current task counts by project and status.
	Tasks *prometheus.GaugeVec

	// Executions tracks current execution counts by project and status.
	Executions *prometheus.GaugeVec

	// PodmanContainers tracks discovered vornik-managed containers by project and state.
	PodmanContainers *prometheus.GaugeVec

	// PodmanHealthy indicates whether Podman is reachable (1 = yes, 0 = no).
	PodmanHealthy prometheus.Gauge

	// DBTableRows tracks estimated live row counts per table.
	DBTableRows *prometheus.GaugeVec

	// DBTableSizeBytes tracks total disk size per table (including indexes and TOAST).
	DBTableSizeBytes *prometheus.GaugeVec

	// AutonomyActiveLoops tracks how many project autonomy loops are running.
	AutonomyActiveLoops prometheus.Gauge

	// ProjectBudgetCapsUSD exposes configured project budget caps. Zero means disabled.
	ProjectBudgetCapsUSD *prometheus.GaugeVec

	// ProjectBudgetTimezoneInfo exposes the budget reset timezone as an info-style gauge.
	ProjectBudgetTimezoneInfo *prometheus.GaugeVec

	// ProjectTaskCreationLimits exposes configured project task creation rate limits.
	ProjectTaskCreationLimits *prometheus.GaugeVec

	// ProjectAutonomyMaxTasksPerHour exposes configured autonomous task creation caps.
	ProjectAutonomyMaxTasksPerHour *prometheus.GaugeVec

	// UISessions is the live browser-login session count by lifecycle status
	// (active | expired_not_revoked | revoked). The expired_not_revoked
	// bucket is the leak class the operator reported 2026-06-23 — rows
	// counted active until retention deletes them — so a steadily climbing
	// value flags accumulation without manual SQL.
	UISessions *prometheus.GaugeVec
}

// ProjectFinancialControls is the registry-derived subset of project config
// that controls task volume and LLM spend.
type ProjectFinancialControls struct {
	ProjectID               string
	BudgetDailySoftUSD      float64
	BudgetDailyHardUSD      float64
	BudgetMonthlySoftUSD    float64
	BudgetMonthlyHardUSD    float64
	BudgetTimezone          string
	RateLimitTasksPerMinute int
	RateLimitTasksPerHour   int
	AutonomyMaxTasksPerHour int
}

// NewStateCollector creates a StateCollector and registers its metrics.
func NewStateCollector(registerer prometheus.Registerer) *StateCollector {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}

	return &StateCollector{
		RegistryEntities: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "vornik",
				Subsystem: "registry",
				Name:      "entities",
				Help:      "Number of registered configuration entities.",
			},
			[]string{"kind"},
		),
		Tasks: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "vornik",
				Subsystem: "tasks",
				Name:      "count",
				Help:      "Current number of tasks by project and status.",
			},
			[]string{"project_id", "status"},
		),
		Executions: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "vornik",
				Subsystem: "executions",
				Name:      "count",
				Help:      "Current number of executions by project and status.",
			},
			[]string{"project_id", "status"},
		),
		PodmanContainers: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "vornik",
				Subsystem: "podman",
				Name:      "containers",
				Help:      "Number of vornik-managed Podman containers by project and state.",
			},
			[]string{"project_id", "state"},
		),
		PodmanHealthy: promauto.With(registerer).NewGauge(
			prometheus.GaugeOpts{
				Namespace: "vornik",
				Subsystem: "podman",
				Name:      "healthy",
				Help:      "Whether Podman is reachable (1 = healthy, 0 = unhealthy).",
			},
		),
		DBTableRows: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "vornik",
				Subsystem: "db",
				Name:      "table_estimated_rows",
				Help:      "Estimated number of live rows per table from pg_stat_user_tables.",
			},
			[]string{"table"},
		),
		DBTableSizeBytes: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "vornik",
				Subsystem: "db",
				Name:      "table_size_bytes",
				Help:      "Total disk size per table in bytes (including indexes and TOAST).",
			},
			[]string{"table"},
		),
		AutonomyActiveLoops: promauto.With(registerer).NewGauge(
			prometheus.GaugeOpts{
				Namespace: "vornik",
				Subsystem: "autonomy",
				Name:      "active_loops",
				Help:      "Number of project autonomy loops currently running.",
			},
		),
		ProjectBudgetCapsUSD: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "vornik",
				Subsystem: "project",
				Name:      "budget_cap_usd",
				Help:      "Configured project budget cap in USD. Zero means disabled.",
			},
			[]string{"project_id", "period", "level"},
		),
		ProjectBudgetTimezoneInfo: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "vornik",
				Subsystem: "project",
				Name:      "budget_timezone_info",
				Help:      "Budget reset timezone configured for the project. Value is always 1.",
			},
			[]string{"project_id", "timezone"},
		),
		ProjectTaskCreationLimits: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "vornik",
				Subsystem: "project",
				Name:      "task_creation_limit",
				Help:      "Configured project task creation limit. Zero means disabled.",
			},
			[]string{"project_id", "window"},
		),
		ProjectAutonomyMaxTasksPerHour: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "vornik",
				Subsystem: "project",
				Name:      "autonomy_max_tasks_per_hour",
				Help:      "Configured maximum autonomous tasks per hour for the project. Zero means disabled.",
			},
			[]string{"project_id"},
		),
		UISessions: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "vornik",
				Name:      "ui_sessions",
				Help:      "Browser login session count by lifecycle status (active|expired_not_revoked|revoked). A climbing expired_not_revoked is the 2026-06-23 leak class — counted until retention deletes it.",
			},
			[]string{"status"},
		),
	}
}

// RecordUISessionCounts sets the ui_sessions lifecycle gauge. Reset first so
// the buckets reflect the current snapshot rather than max-ever values.
func (c *StateCollector) RecordUISessionCounts(active, expiredNotRevoked, revoked int64) {
	if c == nil {
		return
	}
	c.UISessions.Reset()
	c.UISessions.WithLabelValues("active").Set(float64(active))
	c.UISessions.WithLabelValues("expired_not_revoked").Set(float64(expiredNotRevoked))
	c.UISessions.WithLabelValues("revoked").Set(float64(revoked))
}

// RecordRegistryCounts sets the registry entity gauges.
func (c *StateCollector) RecordRegistryCounts(projects, swarms, workflows int) {
	if c == nil {
		return
	}
	c.RegistryEntities.WithLabelValues("project").Set(float64(projects))
	c.RegistryEntities.WithLabelValues("swarm").Set(float64(swarms))
	c.RegistryEntities.WithLabelValues("workflow").Set(float64(workflows))
}

// RecordTaskCounts resets and records task count gauges.
// Counts are keyed by project_id → status → count.
func (c *StateCollector) RecordTaskCounts(counts map[string]map[string]int64) {
	if c == nil {
		return
	}
	c.Tasks.Reset()
	for projectID, statuses := range counts {
		for status, count := range statuses {
			c.Tasks.WithLabelValues(projectID, status).Set(float64(count))
		}
	}
}

// RecordExecutionCounts resets and records execution count gauges.
// Counts are keyed by project_id → status → count.
func (c *StateCollector) RecordExecutionCounts(counts map[string]map[string]int64) {
	if c == nil {
		return
	}
	c.Executions.Reset()
	for projectID, statuses := range counts {
		for status, count := range statuses {
			c.Executions.WithLabelValues(projectID, status).Set(float64(count))
		}
	}
}

// RecordPodmanContainers resets and records container counts from Podman discovery.
// Counts are keyed by project_id → state → count.
func (c *StateCollector) RecordPodmanContainers(counts map[string]map[string]int, healthy bool) {
	if c == nil {
		return
	}
	c.PodmanContainers.Reset()
	for projectID, states := range counts {
		for state, count := range states {
			c.PodmanContainers.WithLabelValues(projectID, state).Set(float64(count))
		}
	}
	if healthy {
		c.PodmanHealthy.Set(1)
	} else {
		c.PodmanHealthy.Set(0)
	}
}

// RecordTableStats resets and records Postgres table statistics.
func (c *StateCollector) RecordTableStats(rows map[string]int64, sizeBytes map[string]int64) {
	if c == nil {
		return
	}
	c.DBTableRows.Reset()
	for table, count := range rows {
		c.DBTableRows.WithLabelValues(table).Set(float64(count))
	}
	c.DBTableSizeBytes.Reset()
	for table, size := range sizeBytes {
		c.DBTableSizeBytes.WithLabelValues(table).Set(float64(size))
	}
}

// RecordAutonomyActiveLoops sets the active autonomy loop count.
func (c *StateCollector) RecordAutonomyActiveLoops(count int) {
	if c == nil {
		return
	}
	c.AutonomyActiveLoops.Set(float64(count))
}

// RecordProjectFinancialControls resets and records project budget and task
// volume controls from the current registry snapshot.
func (c *StateCollector) RecordProjectFinancialControls(projects []ProjectFinancialControls) {
	if c == nil {
		return
	}
	c.ProjectBudgetCapsUSD.Reset()
	c.ProjectBudgetTimezoneInfo.Reset()
	c.ProjectTaskCreationLimits.Reset()
	c.ProjectAutonomyMaxTasksPerHour.Reset()

	for _, p := range projects {
		timezone := p.BudgetTimezone
		if timezone == "" {
			timezone = "UTC"
		}
		c.ProjectBudgetCapsUSD.WithLabelValues(p.ProjectID, "daily", "soft").Set(p.BudgetDailySoftUSD)
		c.ProjectBudgetCapsUSD.WithLabelValues(p.ProjectID, "daily", "hard").Set(p.BudgetDailyHardUSD)
		c.ProjectBudgetCapsUSD.WithLabelValues(p.ProjectID, "monthly", "soft").Set(p.BudgetMonthlySoftUSD)
		c.ProjectBudgetCapsUSD.WithLabelValues(p.ProjectID, "monthly", "hard").Set(p.BudgetMonthlyHardUSD)
		c.ProjectBudgetTimezoneInfo.WithLabelValues(p.ProjectID, timezone).Set(1)
		c.ProjectTaskCreationLimits.WithLabelValues(p.ProjectID, "minute").Set(float64(p.RateLimitTasksPerMinute))
		c.ProjectTaskCreationLimits.WithLabelValues(p.ProjectID, "hour").Set(float64(p.RateLimitTasksPerHour))
		c.ProjectAutonomyMaxTasksPerHour.WithLabelValues(p.ProjectID).Set(float64(p.AutonomyMaxTasksPerHour))
	}
}
