package config

import (
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewMetrics_RegistersTwiceWithoutPanic — fresh registries don't
// collide; nil falls back to the default registerer (covered indirectly
// by the wired-reload tests against an explicit registry).
func TestNewMetrics_RegistersTwiceWithoutPanic(t *testing.T) {
	assert.NotPanics(t, func() {
		_ = NewMetrics(prometheus.NewRegistry())
		_ = NewMetrics(prometheus.NewRegistry())
	})
}

// TestNewMetrics_NilRegistererFallsBackToDefault — nil falls back to
// the default registerer.
func TestNewMetrics_NilRegistererFallsBackToDefault(t *testing.T) {
	orig := prometheus.DefaultRegisterer
	prometheus.DefaultRegisterer = prometheus.NewRegistry()
	t.Cleanup(func() { prometheus.DefaultRegisterer = orig })

	m := NewMetrics(nil)
	require.NotNil(t, m)
	require.NotNil(t, m.ReloadTotal)
}

// TestConfigReloader_Reload_SuccessObservesMetrics — a clean reload
// bumps reload_total{success}, sets the last-reload timestamp, clears
// validation_errors + staged_changes_pending.
func TestConfigReloader_Reload_SuccessObservesMetrics(t *testing.T) {
	w := NewWatcher(nil)
	r := NewConfigReloader(w, zerolog.Nop())
	m := NewMetrics(prometheus.NewRegistry())
	r.SetMetrics(m)

	r.SetLoader(func() error { return nil })
	r.SetValidator(func() error { return nil })
	r.SetActivator(func() error { return nil })

	require.NoError(t, r.Reload())

	assert.Equal(t, 1.0, testutil.ToFloat64(m.ReloadTotal.WithLabelValues("success")))
	assert.Equal(t, 0.0, testutil.ToFloat64(m.ReloadTotal.WithLabelValues("failure")))
	assert.Equal(t, 0.0, testutil.ToFloat64(m.ValidationErrors))
	assert.Equal(t, 0.0, testutil.ToFloat64(m.StagedChangesPending))
	assert.Greater(t, testutil.ToFloat64(m.LastReloadTimestamp), 0.0)
}

// TestConfigReloader_Reload_LoadFailureObservesFailure — a loader error
// bumps reload_total{failure} and sets validation_errors to 1.
func TestConfigReloader_Reload_LoadFailureObservesFailure(t *testing.T) {
	w := NewWatcher(nil)
	r := NewConfigReloader(w, zerolog.Nop())
	m := NewMetrics(prometheus.NewRegistry())
	r.SetMetrics(m)

	r.SetLoader(func() error { return errors.New("boom") })

	require.Error(t, r.Reload())

	assert.Equal(t, 1.0, testutil.ToFloat64(m.ReloadTotal.WithLabelValues("failure")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.ValidationErrors))
	assert.Equal(t, 0.0, testutil.ToFloat64(m.StagedChangesPending))
}

// TestConfigReloader_Reload_BlockedActivationSetsStagedPending — an
// activation blocked-error keeps the stage pending, so the gauge reads 1
// and the cycle counts as a failure.
func TestConfigReloader_Reload_BlockedActivationSetsStagedPending(t *testing.T) {
	w := NewWatcher(nil)
	r := NewConfigReloader(w, zerolog.Nop())
	m := NewMetrics(prometheus.NewRegistry())
	r.SetMetrics(m)

	r.SetLoader(func() error { return nil })
	r.SetActivator(func() error { return &ActivationBlockedError{Reason: "peer not ready"} })

	require.Error(t, r.Reload())

	assert.Equal(t, 1.0, testutil.ToFloat64(m.ReloadTotal.WithLabelValues("failure")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.StagedChangesPending))
}

// TestMetrics_ObserveReload_NilReceiverSafe — the reloader calls the
// observe helper unconditionally; an unwired reloader must not panic.
func TestMetrics_ObserveReload_NilReceiverSafe(t *testing.T) {
	var m *Metrics
	assert.NotPanics(t, func() { m.observeReload(true, 0, false, time.Now()) })
}
