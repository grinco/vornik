package persistence

import (
	"context"
	"database/sql"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	dbNamespace = "vornik"
	dbSubsystem = "db"
)

// DBMetrics holds Prometheus metrics for database connection pool monitoring.
type DBMetrics struct {
	OpenConnections   *prometheus.GaugeVec
	InUse             *prometheus.GaugeVec
	Idle              *prometheus.GaugeVec
	WaitCount         *prometheus.GaugeVec
	WaitDuration      *prometheus.GaugeVec
	MaxIdleClosed     *prometheus.GaugeVec
	MaxLifetimeClosed *prometheus.GaugeVec
	QueryLatency      *prometheus.HistogramVec
	QueryTotal        *prometheus.CounterVec

	registry prometheus.Registerer
}

var dbMetricOperations = []string{
	"query",
	"query_row",
	"exec",
	"begin",
	"commit",
	"rollback",
}

// NewDBMetrics creates a new DBMetrics instance with the given Prometheus registerer.
// If registerer is nil, prometheus.DefaultRegisterer is used.
func NewDBMetrics(registerer prometheus.Registerer) *DBMetrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}

	return &DBMetrics{
		registry: registerer,
		OpenConnections: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: dbNamespace,
				Subsystem: dbSubsystem,
				Name:      "open_connections",
				Help:      "Number of open database connections.",
			},
			[]string{"database"},
		),
		InUse: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: dbNamespace,
				Subsystem: dbSubsystem,
				Name:      "in_use_connections",
				Help:      "Number of database connections currently in use.",
			},
			[]string{"database"},
		),
		Idle: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: dbNamespace,
				Subsystem: dbSubsystem,
				Name:      "idle_connections",
				Help:      "Number of idle database connections.",
			},
			[]string{"database"},
		),
		WaitCount: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: dbNamespace,
				Subsystem: dbSubsystem,
				Name:      "wait_count_total",
				Help:      "Cumulative number of connections waited for.",
			},
			[]string{"database"},
		),
		WaitDuration: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: dbNamespace,
				Subsystem: dbSubsystem,
				Name:      "wait_duration_seconds_total",
				Help:      "Cumulative time waited for database connections in seconds.",
			},
			[]string{"database"},
		),
		MaxIdleClosed: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: dbNamespace,
				Subsystem: dbSubsystem,
				Name:      "max_idle_closed_total",
				Help:      "Cumulative number of connections closed due to max idle limit.",
			},
			[]string{"database"},
		),
		MaxLifetimeClosed: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: dbNamespace,
				Subsystem: dbSubsystem,
				Name:      "max_lifetime_closed_total",
				Help:      "Cumulative number of connections closed due to max lifetime.",
			},
			[]string{"database"},
		),
		QueryLatency: promauto.With(registerer).NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: dbNamespace,
				Subsystem: dbSubsystem,
				Name:      "query_latency_seconds",
				Help:      "Time spent executing database queries in seconds.",
				Buckets: []float64{
					0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5,
					1, 2.5, 5, 10,
				},
			},
			[]string{"database", "operation"},
		),
		QueryTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: dbNamespace,
				Subsystem: dbSubsystem,
				Name:      "queries_total",
				Help:      "Total number of database queries executed.",
			},
			[]string{"database", "operation", "status"},
		),
	}
}

// RecordPoolStats records the current connection pool statistics.
func (m *DBMetrics) RecordPoolStats(database string, stats sql.DBStats) {
	if m == nil {
		return
	}

	m.OpenConnections.WithLabelValues(database).Set(float64(stats.OpenConnections))
	m.InUse.WithLabelValues(database).Set(float64(stats.InUse))
	m.Idle.WithLabelValues(database).Set(float64(stats.Idle))
	m.WaitCount.WithLabelValues(database).Set(float64(stats.WaitCount))
	m.WaitDuration.WithLabelValues(database).Set(stats.WaitDuration.Seconds())
	m.MaxIdleClosed.WithLabelValues(database).Set(float64(stats.MaxIdleClosed))
	m.MaxLifetimeClosed.WithLabelValues(database).Set(float64(stats.MaxLifetimeClosed))
}

// RecordQuery records a query execution with latency.
func (m *DBMetrics) RecordQuery(database, operation string, duration time.Duration, err error) {
	if m == nil {
		return
	}

	status := "success"
	if err != nil {
		status = "error"
	}

	m.QueryLatency.WithLabelValues(database, operation).Observe(duration.Seconds())
	m.QueryTotal.WithLabelValues(database, operation, status).Inc()
}

// DBWithMetrics wraps a *sql.DB with metrics collection. Backend-
// agnostic — the wrapped handle is the stdlib type rather than a
// Postgres-specific subtype so any future backend (SQLite, etc.)
// reuses the same instrumentation.
type DBWithMetrics struct {
	*sql.DB
	metrics  *DBMetrics
	database string
}

// NewDBWithMetrics wraps a *sql.DB with metrics collection.
func NewDBWithMetrics(db *sql.DB, metrics *DBMetrics, database string) *DBWithMetrics {
	if metrics != nil && database != "" {
		metrics.initializeQuerySeries(database)
	}
	return &DBWithMetrics{
		DB:       db,
		metrics:  metrics,
		database: database,
	}
}

func (m *DBMetrics) initializeQuerySeries(database string) {
	if m == nil || database == "" {
		return
	}
	for _, operation := range dbMetricOperations {
		m.QueryTotal.WithLabelValues(database, operation, "success")
		m.QueryTotal.WithLabelValues(database, operation, "error")
	}
}

// RecordPoolStats records the current pool statistics to metrics.
func (d *DBWithMetrics) RecordPoolStats() {
	if d.metrics == nil || d.DB == nil {
		return
	}
	d.metrics.RecordPoolStats(d.database, d.Stats())
}

// QueryContext executes a query with metrics tracking.
func (d *DBWithMetrics) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	start := time.Now()
	rows, err := d.DB.QueryContext(ctx, query, args...)
	d.metrics.RecordQuery(d.database, "query", time.Since(start), err)
	return rows, err
}

// QueryRowContext executes a query row with metrics tracking.
func (d *DBWithMetrics) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	start := time.Now()
	row := d.DB.QueryRowContext(ctx, query, args...)
	d.metrics.RecordQuery(d.database, "query_row", time.Since(start), nil)
	return row
}

// ExecContext executes a statement with metrics tracking.
func (d *DBWithMetrics) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	start := time.Now()
	result, err := d.DB.ExecContext(ctx, query, args...)
	d.metrics.RecordQuery(d.database, "exec", time.Since(start), err)
	return result, err
}

// BeginTx starts a transaction with metrics tracking.
func (d *DBWithMetrics) BeginTx(ctx context.Context, opts *sql.TxOptions) (*TxWithMetrics, error) {
	start := time.Now()
	tx, err := d.DB.BeginTx(ctx, opts)
	d.metrics.RecordQuery(d.database, "begin", time.Since(start), err)
	if err != nil {
		return nil, err
	}
	return &TxWithMetrics{
		Tx:       tx,
		metrics:  d.metrics,
		database: d.database,
	}, nil
}

// TxWithMetrics wraps a transaction with metrics collection.
type TxWithMetrics struct {
	*sql.Tx
	metrics  *DBMetrics
	database string
}

// ExecContext executes a statement with metrics tracking.
func (t *TxWithMetrics) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	start := time.Now()
	result, err := t.Tx.ExecContext(ctx, query, args...)
	t.metrics.RecordQuery(t.database, "exec", time.Since(start), err)
	return result, err
}

// QueryContext executes a query with metrics tracking.
func (t *TxWithMetrics) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	start := time.Now()
	rows, err := t.Tx.QueryContext(ctx, query, args...)
	t.metrics.RecordQuery(t.database, "query", time.Since(start), err)
	return rows, err
}

// QueryRowContext executes a query row with metrics tracking.
func (t *TxWithMetrics) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	start := time.Now()
	row := t.Tx.QueryRowContext(ctx, query, args...)
	t.metrics.RecordQuery(t.database, "query_row", time.Since(start), nil)
	return row
}

// Commit commits the transaction with metrics tracking.
func (t *TxWithMetrics) Commit() error {
	start := time.Now()
	err := t.Tx.Commit()
	t.metrics.RecordQuery(t.database, "commit", time.Since(start), err)
	return err
}

// Rollback rolls back the transaction with metrics tracking.
func (t *TxWithMetrics) Rollback() error {
	start := time.Now()
	err := t.Tx.Rollback()
	t.metrics.RecordQuery(t.database, "rollback", time.Since(start), err)
	return err
}
