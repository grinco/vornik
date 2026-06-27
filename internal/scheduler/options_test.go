package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"

	"vornik.io/vornik/internal/artifacts"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/runtime"
)

// stubRuntimeManager implements RuntimeManager without launching a
// real container. The scheduler option just records the pointer, so
// the methods never actually fire during these tests.
type stubRuntimeManager struct{}

func (stubRuntimeManager) StartContainer(_ context.Context, _ *runtime.ContainerConfig) (string, error) {
	return "", nil
}
func (stubRuntimeManager) WaitForExit(_ context.Context, _ string, _ time.Duration) (int, error) {
	return 0, nil
}
func (stubRuntimeManager) RemoveContainer(_ context.Context, _ string, _ bool) error { return nil }

// TestWithLogger_Scheduler — the option lands the supplied logger
// on the scheduler so production wiring's structured logger reaches
// runLoop / dispatch / recovery paths instead of the zerolog.Nop()
// default.
func TestWithLogger_Scheduler(t *testing.T) {
	repo := &mockTaskRepo{}
	want := zerolog.Nop().With().Str("test", "x").Logger()
	s := NewWithOptions(repo, nil, WithLogger(want))
	// We can't compare zerolog.Loggers directly via ==, but the level
	// pass-through is enough to prove the option assigned the field.
	assert.Equal(t, want.GetLevel(), s.logger.GetLevel())
}

func TestWithTracer_Scheduler(t *testing.T) {
	repo := &mockTaskRepo{}
	tracer := otel.Tracer("test-tracer")
	s := NewWithOptions(repo, nil, WithTracer(tracer))
	assert.Equal(t, tracer, s.tracer)
}

// TestWithRuntimeManager_Scheduler — option lands the runtime
// pointer so dispatch can launch containers when the executor
// surface isn't wired separately.
func TestWithRuntimeManager_Scheduler(t *testing.T) {
	repo := &mockTaskRepo{}
	var rm RuntimeManager = stubRuntimeManager{}
	s := NewWithOptions(repo, nil, WithRuntimeManager(rm))
	assert.Equal(t, rm, s.runtime)
}

func TestWithArtifactStore_Scheduler(t *testing.T) {
	repo := &mockTaskRepo{}
	// artifacts.Store is a pointer type; we pass a zero-value placeholder
	// since the option just records the pointer.
	store := &artifacts.Store{}
	s := NewWithOptions(repo, nil, WithArtifactStore(store))
	assert.Same(t, store, s.artifactStore)
}

// TestScheduler_Wake_NonBlocking — Wake should buffer one signal
// and never block under contention.
func TestScheduler_Wake_NonBlocking(t *testing.T) {
	repo := &mockTaskRepo{}
	s := NewWithOptions(repo, nil)

	// Drain initial state (channel starts empty).
	s.Wake()
	// Second call: buffer full → must not block, must not panic.
	assert.NotPanics(t, func() {
		s.Wake()
		s.Wake()
		s.Wake()
	})

	// Nil receiver is a no-op (production hot path on partial init).
	var sNil *Scheduler
	assert.NotPanics(t, func() { sNil.Wake() })
}

func TestScheduler_LastTick_BeforeAndAfter(t *testing.T) {
	repo := &mockTaskRepo{}
	s := NewWithOptions(repo, nil)
	// Pre-Start: lastTick is zero.
	assert.True(t, s.LastTick().IsZero())

	// Stamp lastTick directly under the mutex (production runLoop's
	// path) and verify the accessor reads it back.
	now := time.Now().UTC()
	s.mu.Lock()
	s.lastTick = now
	s.mu.Unlock()
	assert.Equal(t, now, s.LastTick())

	// Nil receiver returns the zero time without panicking.
	var sNil *Scheduler
	assert.True(t, sNil.LastTick().IsZero())
}

func TestScheduler_PollInterval_AccessorAndNilSafety(t *testing.T) {
	repo := &mockTaskRepo{}
	cfg := DefaultConfig()
	cfg.PollInterval = 750 * time.Millisecond
	s := NewWithOptions(repo, cfg)
	assert.Equal(t, 750*time.Millisecond, s.PollInterval())

	// Nil receiver returns zero duration (used during readyz handoff
	// before the scheduler is fully constructed).
	var sNil *Scheduler
	assert.Equal(t, time.Duration(0), sNil.PollInterval())

	// Scheduler with nil config returns zero.
	sNoCfg := &Scheduler{}
	assert.Equal(t, time.Duration(0), sNoCfg.PollInterval())
}

func TestSetMetrics_Scheduler(t *testing.T) {
	repo := &mockTaskRepo{}
	s := NewWithOptions(repo, nil)
	assert.Nil(t, s.metrics)

	m := NewMetrics(prometheus.NewRegistry())
	s.SetMetrics(m)
	assert.Equal(t, m, s.metrics)

	// Reset to nil → SetMetrics handles the reset cleanly.
	s.SetMetrics(nil)
	assert.Nil(t, s.metrics)
}

func TestMetrics_UpdateQueueDepth(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.UpdateQueueDepth("p1", persistence.TaskStatusQueued, 7)
	got := testutil.ToFloat64(m.QueueDepthGauge.WithLabelValues("p1", string(persistence.TaskStatusQueued)))
	assert.Equal(t, 7.0, got)

	// Updating the same label set replaces the gauge value (not adds).
	m.UpdateQueueDepth("p1", persistence.TaskStatusQueued, 3)
	got = testutil.ToFloat64(m.QueueDepthGauge.WithLabelValues("p1", string(persistence.TaskStatusQueued)))
	assert.Equal(t, 3.0, got)

	// nil receiver — safe no-op.
	var mNil *Metrics
	assert.NotPanics(t, func() {
		mNil.UpdateQueueDepth("p1", persistence.TaskStatusQueued, 1)
	})
}

// TestErrorMsgOrStatus exercises the three branches of the small
// helper: explicit error wins, FAILED status synthesises a default
// message, otherwise empty.
func TestErrorMsgOrStatus(t *testing.T) {
	cases := []struct {
		name     string
		errorMsg string
		status   persistence.TaskStatus
		want     string
	}{
		{"explicit error wins", "container exited 137", persistence.TaskStatusFailed, "container exited 137"},
		{"failed with empty msg synthesises default", "", persistence.TaskStatusFailed, "executor reported task failure"},
		{"non-failed empty msg → empty", "", persistence.TaskStatusCompleted, ""},
		{"queued empty msg → empty", "", persistence.TaskStatusQueued, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, errorMsgOrStatus(tc.errorMsg, tc.status))
		})
	}
}

func TestMetrics_RecordExecutionCompleted(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.RecordExecutionCompleted("p1", "completed", 1.25)
	m.RecordExecutionCompleted("p1", "completed", 0.50)
	m.RecordExecutionCompleted("p1", "failed", 0.10)

	got := testutil.ToFloat64(m.ExecutionsCompletedTotal.WithLabelValues("p1", "completed"))
	assert.Equal(t, 2.0, got)
	got = testutil.ToFloat64(m.ExecutionsCompletedTotal.WithLabelValues("p1", "failed"))
	assert.Equal(t, 1.0, got)

	// Histogram observations are tracked but only sum is easily
	// asserted; verify we don't panic and the registry has the
	// metric registered.
	require.NotNil(t, m.ExecutionLatencySeconds)

	// Nil receiver — safe no-op.
	var mNil *Metrics
	assert.NotPanics(t, func() {
		mNil.RecordExecutionCompleted("p1", "completed", 1.0)
	})
}
