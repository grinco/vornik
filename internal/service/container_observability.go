package service

// Observability + state-collector wiring extracted from
// container.go as part of the 2026-05-16 service-package split.
// Owns the Prometheus / metrics + state-snapshot collectors plus
// the systemd-readiness notify helper.

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/observability"
	"vornik.io/vornik/internal/persistence"
)

func buildObservabilityConfig(cfg *config.Config) observability.Config {
	if cfg == nil {
		return observability.Config{}
	}

	obsCfg := observability.Config{
		TracingEnabled:  cfg.Tracing.Enabled,
		TracingEndpoint: cfg.Tracing.Endpoint,
	}

	if cfg.Metrics.Enabled {
		obsCfg.MetricsAddr = cfg.Metrics.Addr
	}

	return obsCfg
}

func (c *Container) observabilityRegistry() *prometheus.Registry {
	if c == nil || c.Observability == nil || c.Observability.Metrics == nil {
		return nil
	}

	return c.Observability.Metrics.Registry()
}

// instrumentedDB returns a DBTX that wraps database queries with Prometheus
// metrics when observability is initialized. Falls back to the raw *sql.DB
// when metrics are not available.
func (c *Container) instrumentedDB() persistence.DBTX {
	if c.dbMetrics != nil {
		return persistence.NewDBWithMetrics(c.DB, c.dbMetrics, c.Config.Database.Name)
	}
	return c.DB // raw *sql.DB
}

// initStateCollector starts periodic collection of business state metrics.
func (c *Container) initStateCollector() {
	registry := c.observabilityRegistry()
	if registry == nil {
		return
	}
	c.stateCollector = observability.NewStateCollector(registry)
	go c.collectStateMetrics()
	c.Logger.Info().Msg("state metrics collector started")
}

// collectStateMetrics periodically snapshots task, execution, registry,
// Podman, and Postgres table metrics.
// Context pointer is re-read each iteration for the same reason as collectDBMetrics.
func (c *Container) collectStateMetrics() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		var done <-chan struct{}
		if ctx := c.collectorsCtx; ctx != nil {
			done = ctx.Done()
		}

		select {
		case <-ticker.C:
			c.collectRegistryCounts()
			c.collectTaskCounts()
			c.collectExecutionCounts()
			c.collectPodmanState()
			c.collectTableStats()
			c.collectUISessionCounts()
			if c.autonomyManager != nil && c.stateCollector != nil {
				c.stateCollector.RecordAutonomyActiveLoops(c.autonomyManager.ActiveLoops())
			}
		case <-done:
			return
		}
	}
}

func (c *Container) collectRegistryCounts() {
	if c.stateCollector == nil {
		return
	}
	var projects, swarms, workflows int
	var financialControls []observability.ProjectFinancialControls
	if c.Registry != nil {
		projectList := c.Registry.ListProjects()
		projects = len(projectList)
		swarms = len(c.Registry.ListSwarms())
		workflows = len(c.Registry.ListWorkflows())
		financialControls = make([]observability.ProjectFinancialControls, 0, len(projectList))
		for _, project := range projectList {
			if project == nil {
				continue
			}
			financialControls = append(financialControls, observability.ProjectFinancialControls{
				ProjectID:               project.ID,
				BudgetDailySoftUSD:      project.Budget.DailySoftUSD,
				BudgetDailyHardUSD:      project.Budget.DailyHardUSD,
				BudgetMonthlySoftUSD:    project.Budget.MonthlySoftUSD,
				BudgetMonthlyHardUSD:    project.Budget.MonthlyHardUSD,
				BudgetTimezone:          project.Budget.Timezone,
				RateLimitTasksPerMinute: project.RateLimit.TasksPerMinute,
				RateLimitTasksPerHour:   project.RateLimit.TasksPerHour,
				AutonomyMaxTasksPerHour: project.Autonomy.MaxTasksPerHour,
			})
		}
	}
	c.stateCollector.RecordRegistryCounts(projects, swarms, workflows)
	c.stateCollector.RecordProjectFinancialControls(financialControls)
}

// collectUISessionCounts refreshes the ui_sessions lifecycle gauge from the
// session repository. Global table (no project label); best-effort — a query
// error (or a deployment without the table) logs and skips, like the other
// collectors. The expired_not_revoked bucket surfaces the 2026-06-23 leak
// class so accumulation is visible without manual SQL.
func (c *Container) collectUISessionCounts() {
	if c.stateCollector == nil || c.repos == nil || c.repos.UISessions == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	counts, err := c.repos.UISessions.CountByStatus(ctx)
	if err != nil {
		c.Logger.Warn().Err(err).Msg("failed to collect ui_sessions count metrics")
		return
	}
	c.stateCollector.RecordUISessionCounts(counts.Active, counts.ExpiredNotRevoked, counts.Revoked)
}

func (c *Container) collectTaskCounts() {
	if c.DB == nil || c.stateCollector == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := c.DB.QueryContext(ctx, `
		SELECT project_id, status, COUNT(*)
		FROM tasks
		GROUP BY project_id, status
	`)
	if err != nil {
		c.Logger.Warn().Err(err).Msg("failed to collect task count metrics")
		return
	}
	defer func() { _ = rows.Close() }()

	counts := make(map[string]map[string]int64)
	for rows.Next() {
		var projectID, status string
		var count int64
		if err := rows.Scan(&projectID, &status, &count); err != nil {
			continue
		}
		if counts[projectID] == nil {
			counts[projectID] = make(map[string]int64)
		}
		counts[projectID][status] = count
	}
	if err := rows.Err(); err != nil {
		c.Logger.Warn().Err(err).Msg("task count metrics iteration failed")
		return
	}
	c.stateCollector.RecordTaskCounts(counts)
}

func (c *Container) collectExecutionCounts() {
	if c.DB == nil || c.stateCollector == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := c.DB.QueryContext(ctx, `
		SELECT project_id, status, COUNT(*)
		FROM executions
		GROUP BY project_id, status
	`)
	if err != nil {
		c.Logger.Warn().Err(err).Msg("failed to collect execution count metrics")
		return
	}
	defer func() { _ = rows.Close() }()

	counts := make(map[string]map[string]int64)
	for rows.Next() {
		var projectID, status string
		var count int64
		if err := rows.Scan(&projectID, &status, &count); err != nil {
			continue
		}
		if counts[projectID] == nil {
			counts[projectID] = make(map[string]int64)
		}
		counts[projectID][status] = count
	}
	if err := rows.Err(); err != nil {
		c.Logger.Warn().Err(err).Msg("execution count metrics iteration failed")
		return
	}
	c.stateCollector.RecordExecutionCounts(counts)
}

func (c *Container) collectPodmanState() {
	if c.runtimeManager == nil || c.stateCollector == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	containers, err := c.runtimeManager.ListContainers(ctx, nil)
	if err != nil {
		c.Logger.Debug().Err(err).Msg("failed to collect podman container metrics")
		c.stateCollector.RecordPodmanContainers(nil, false)
		return
	}

	counts := make(map[string]map[string]int)
	for _, ctr := range containers {
		projectID := ctr.ProjectID
		state := string(ctr.Status)
		if counts[projectID] == nil {
			counts[projectID] = make(map[string]int)
		}
		counts[projectID][state]++
	}
	c.stateCollector.RecordPodmanContainers(counts, true)
}

func (c *Container) collectTableStats() {
	if c.DB == nil || c.stateCollector == nil {
		return
	}
	// pg_stat_user_tables is Postgres-specific. SQLite-backed
	// deployments skip the table-stats sample (the dashboard tile
	// just renders empty).
	if c.backend == nil || c.backend.PG == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := c.DB.QueryContext(ctx, `
		SELECT relname, n_live_tup, pg_total_relation_size(relid)
		FROM pg_stat_user_tables
		WHERE schemaname = 'public'
	`)
	if err != nil {
		c.Logger.Warn().Err(err).Msg("failed to collect table stats metrics")
		return
	}
	defer func() { _ = rows.Close() }()

	rowCounts := make(map[string]int64)
	sizeBytes := make(map[string]int64)
	for rows.Next() {
		var name string
		var nRows, size int64
		if err := rows.Scan(&name, &nRows, &size); err != nil {
			continue
		}
		rowCounts[name] = nRows
		sizeBytes[name] = size
	}
	c.stateCollector.RecordTableStats(rowCounts, sizeBytes)
}

// sdNotifyReady sends READY=1 to the systemd notify socket.
// Returns nil silently when NOTIFY_SOCKET is unset (not running under systemd).
func sdNotifyReady() error {
	socketPath := os.Getenv("NOTIFY_SOCKET")
	if socketPath == "" {
		return nil
	}

	conn, err := net.Dial("unixgram", socketPath)
	if err != nil {
		return fmt.Errorf("dial notify socket: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("READY=1")); err != nil {
		return fmt.Errorf("write notify socket: %w", err)
	}
	return nil
}
