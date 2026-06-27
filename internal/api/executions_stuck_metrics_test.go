package api

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAPIMetrics_SetExecutionsStuck_SetsGaugePerStatus — the watchdog
// gauge (audit R13) SETs the per-status count, and a later scan that
// clears a status drops it back to 0 (not a stale pin).
func TestAPIMetrics_SetExecutionsStuck_SetsGaugePerStatus(t *testing.T) {
	m := NewAPIMetrics(prometheus.NewRegistry())
	require.NotNil(t, m.ExecutionsStuck)

	m.SetExecutionsStuck(map[string]int{"RUNNING": 3, "PENDING": 1})
	assert.Equal(t, 3.0, testutil.ToFloat64(m.ExecutionsStuck.WithLabelValues("RUNNING")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.ExecutionsStuck.WithLabelValues("PENDING")))

	// Next scan: RUNNING cleared, PENDING still 1 → RUNNING resets to 0.
	m.SetExecutionsStuck(map[string]int{"RUNNING": 0, "PENDING": 1})
	assert.Equal(t, 0.0, testutil.ToFloat64(m.ExecutionsStuck.WithLabelValues("RUNNING")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.ExecutionsStuck.WithLabelValues("PENDING")))
}

// TestAPIMetrics_SetExecutionsStuck_NilReceiverSafe — the doctor check
// runs without a wired metrics registry on SQLite / test rigs.
func TestAPIMetrics_SetExecutionsStuck_NilReceiverSafe(t *testing.T) {
	var m *APIMetrics
	assert.NotPanics(t, func() { m.SetExecutionsStuck(map[string]int{"RUNNING": 2}) })
}

// TestNewAPIMetrics_RegistersTwiceWithoutPanic — fresh registries don't
// collide on repeated construction.
func TestNewAPIMetrics_RegistersTwiceWithoutPanic(t *testing.T) {
	assert.NotPanics(t, func() {
		_ = NewAPIMetrics(prometheus.NewRegistry())
		_ = NewAPIMetrics(prometheus.NewRegistry())
	})
}
