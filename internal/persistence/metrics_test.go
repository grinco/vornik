package persistence

import (
	"database/sql"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"vornik.io/vornik/internal/testutil/metricstest"
)

func TestNewDBMetrics(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewDBMetrics(registry)

	if metrics == nil {
		t.Fatal("expected metrics to be non-nil")
	}

	if metrics.OpenConnections == nil {
		t.Error("OpenConnections metric not initialized")
	}
	if metrics.InUse == nil {
		t.Error("InUse metric not initialized")
	}
	if metrics.Idle == nil {
		t.Error("Idle metric not initialized")
	}
	if metrics.WaitCount == nil {
		t.Error("WaitCount metric not initialized")
	}
	if metrics.QueryLatency == nil {
		t.Error("QueryLatency metric not initialized")
	}
}

func TestDBMetrics_RecordPoolStats(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewDBMetrics(registry)

	stats := sql.DBStats{
		OpenConnections:   10,
		InUse:             3,
		Idle:              7,
		WaitCount:         5,
		WaitDuration:      100 * time.Millisecond,
		MaxIdleClosed:     2,
		MaxLifetimeClosed: 1,
	}

	metrics.RecordPoolStats("testdb", stats)

	openVal := testutil.ToFloat64(metrics.OpenConnections.WithLabelValues("testdb"))
	if openVal != 10 {
		t.Errorf("expected OpenConnections=10, got %v", openVal)
	}

	inUseVal := testutil.ToFloat64(metrics.InUse.WithLabelValues("testdb"))
	if inUseVal != 3 {
		t.Errorf("expected InUse=3, got %v", inUseVal)
	}

	idleVal := testutil.ToFloat64(metrics.Idle.WithLabelValues("testdb"))
	if idleVal != 7 {
		t.Errorf("expected Idle=7, got %v", idleVal)
	}
}

func TestDBMetrics_RecordQuery(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewDBMetrics(registry)

	metrics.RecordQuery("testdb", "select", 10*time.Millisecond, nil)
	metrics.RecordQuery("testdb", "insert", 5*time.Millisecond, sql.ErrConnDone)

	count := testutil.ToFloat64(metrics.QueryTotal.WithLabelValues("testdb", "select", "success"))
	if count != 1 {
		t.Errorf("expected 1 successful select, got %v", count)
	}

	count = testutil.ToFloat64(metrics.QueryTotal.WithLabelValues("testdb", "insert", "error"))
	if count != 1 {
		t.Errorf("expected 1 failed insert, got %v", count)
	}
}

func TestDBMetrics_NilSafe(t *testing.T) {
	// NewDBMetrics(nil) falls back to the default registry; isolate it so the
	// fallback re-registers cleanly under `go test -count>1`.
	metricstest.IsolateDefaultRegistry(t)
	var m *DBMetrics

	m.RecordPoolStats("testdb", sql.DBStats{})
	m.RecordQuery("testdb", "select", time.Millisecond, nil)
	m.RecordQuery("testdb", "select", time.Millisecond, sql.ErrConnDone)

	m = NewDBMetrics(nil)
	if m == nil {
		t.Error("expected non-nil metrics even with nil registry")
	}
}

func TestNewDBWithMetrics_InitializesZeroErrorSeries(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewDBMetrics(registry)

	_ = NewDBWithMetrics(nil, metrics, "testdb")

	gathered, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather failed: %v", err)
	}

	found := false
	for _, mf := range gathered {
		if mf.GetName() != "vornik_db_queries_total" {
			continue
		}
		found = true
		hasZeroError := false
		for _, metric := range mf.GetMetric() {
			labels := make(map[string]string)
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["database"] == "testdb" && labels["operation"] == "query" && labels["status"] == "error" {
				if metric.GetCounter().GetValue() != 0 {
					t.Fatalf("expected zero-valued error series, got %v", metric.GetCounter().GetValue())
				}
				hasZeroError = true
			}
		}
		if !hasZeroError {
			t.Fatal("expected zero-valued error series for query/error labels")
		}
	}
	if !found {
		t.Fatal("expected vornik_db_queries_total to be gathered")
	}
}
