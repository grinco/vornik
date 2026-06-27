package service

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDegradedModeTracker_UpdateDependency(t *testing.T) {
	logger := zerolog.Nop()
	tracker := NewDegradedModeTracker(logger)

	// Initially not degraded
	assert.False(t, tracker.IsDegraded())

	// Mark a dependency as unhealthy
	tracker.UpdateDependency("podman", false, "connection refused")

	// Should be degraded now
	assert.True(t, tracker.IsDegraded())
	assert.Len(t, tracker.GetUnhealthyDeps(), 1)
	assert.Contains(t, tracker.GetUnhealthyDeps(), "podman")

	// Mark as healthy again
	tracker.UpdateDependency("podman", true, "ok")

	// Should not be degraded
	assert.False(t, tracker.IsDegraded())
	assert.Len(t, tracker.GetUnhealthyDeps(), 0)
}

func TestDegradedModeTracker_Callbacks(t *testing.T) {
	logger := zerolog.Nop()
	tracker := NewDegradedModeTracker(logger)

	var enterCalled, exitCalled bool
	tracker.OnEnterDegraded(func() { enterCalled = true })
	tracker.OnExitDegraded(func() { exitCalled = true })

	// Trigger degraded mode
	tracker.UpdateDependency("postgresql", false, "connection timeout")
	assert.True(t, enterCalled)
	assert.False(t, exitCalled)

	// Recover
	tracker.UpdateDependency("postgresql", true, "ok")
	assert.True(t, exitCalled)
}

func TestDegradedModeTracker_MultipleDependencies(t *testing.T) {
	logger := zerolog.Nop()
	tracker := NewDegradedModeTracker(logger)

	// Both healthy
	tracker.UpdateDependency("podman", true, "ok")
	tracker.UpdateDependency("postgresql", true, "ok")
	assert.False(t, tracker.IsDegraded())

	// One unhealthy
	tracker.UpdateDependency("podman", false, "socket not found")
	assert.True(t, tracker.IsDegraded())
	unhealthy := tracker.GetUnhealthyDeps()
	assert.Len(t, unhealthy, 1)

	// Other becomes unhealthy too
	tracker.UpdateDependency("postgresql", false, "connection refused")
	assert.True(t, tracker.IsDegraded())
	unhealthy = tracker.GetUnhealthyDeps()
	assert.Len(t, unhealthy, 2)

	// One recovers - still degraded
	tracker.UpdateDependency("podman", true, "ok")
	assert.True(t, tracker.IsDegraded())
	assert.Len(t, tracker.GetUnhealthyDeps(), 1)

	// Both recovered
	tracker.UpdateDependency("postgresql", true, "ok")
	assert.False(t, tracker.IsDegraded())
}

func TestDegradedModeHandler_CheckReturnsError(t *testing.T) {
	logger := zerolog.Nop()
	tracker := NewDegradedModeTracker(logger)
	handler := NewDegradedModeHandler(tracker)

	// Not degraded - no error
	err := handler.CheckReturnsError("create_task")
	assert.NoError(t, err)

	// Degraded - error
	tracker.UpdateDependency("podman", false, "down")
	err = handler.CheckReturnsError("create_task")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "degraded mode")
	assert.Contains(t, err.Error(), "create_task")
}

func TestDegradedModeHandler_CanServe(t *testing.T) {
	logger := zerolog.Nop()
	tracker := NewDegradedModeTracker(logger)
	handler := NewDegradedModeHandler(tracker)

	// Set up: database healthy, runtime unhealthy
	tracker.UpdateDependency("postgresql", true, "ok")
	tracker.UpdateDependency("podman", false, "down")

	// Read operations can still be served
	assert.True(t, handler.CanServe("list_tasks"))
	assert.True(t, handler.CanServe("get_task"))
	assert.True(t, handler.CanServe("list_executions"))
	assert.True(t, handler.CanServe("get_execution"))

	// Write operations cannot
	assert.False(t, handler.CanServe("create_task"))
	assert.False(t, handler.CanServe("cancel_task"))

	// If database is also down, nothing can be served
	tracker.UpdateDependency("postgresql", false, "down")
	assert.False(t, handler.CanServe("list_tasks"))
	assert.False(t, handler.CanServe("get_task"))
}

func TestDegradedModeError(t *testing.T) {
	err := &DegradedModeError{
		Operation:     "test_op",
		UnhealthyDeps: []string{"podman", "postgresql"},
	}

	assert.Contains(t, err.Error(), "test_op")
	assert.Contains(t, err.Error(), "podman")
	assert.Contains(t, err.Error(), "postgresql")
}

func TestDegradedModeTracker_GetStatus(t *testing.T) {
	logger := zerolog.Nop()
	tracker := NewDegradedModeTracker(logger)

	tracker.UpdateDependency("podman", true, "running")
	tracker.UpdateDependency("postgresql", false, "timeout")

	status := tracker.GetStatus()
	assert.Len(t, status, 2)
	assert.True(t, status["podman"].Healthy)
	assert.False(t, status["postgresql"].Healthy)
	assert.Equal(t, "running", status["podman"].Message)
	assert.Equal(t, "timeout", status["postgresql"].Message)
}

func TestDependencyHealthCheckerStartStopRunsChecks(t *testing.T) {
	logger := zerolog.Nop()
	tracker := NewDegradedModeTracker(logger)
	checker := NewDependencyHealthChecker(tracker, time.Hour)

	checker.RegisterChecker("postgresql", func() (bool, string) {
		return false, "connection refused"
	})

	ctx, cancel := context.WithCancel(context.Background())
	checker.Start(ctx)

	require.Eventually(t, func() bool {
		status := tracker.GetStatus()
		dep, ok := status["postgresql"]
		return ok && !dep.Healthy && dep.Message == "connection refused"
	}, time.Second, 10*time.Millisecond)

	cancel()
	checker.Stop()
	assert.True(t, tracker.IsDegraded())
}

func TestDependencyHealthCheckerStopsOnStopChannel(t *testing.T) {
	logger := zerolog.Nop()
	tracker := NewDegradedModeTracker(logger)
	checker := NewDependencyHealthChecker(tracker, time.Hour)

	checker.RegisterChecker("podman", func() (bool, string) {
		return true, "ok"
	})

	checker.Start(context.Background())
	require.Eventually(t, func() bool {
		_, ok := tracker.GetStatus()["podman"]
		return ok
	}, time.Second, 10*time.Millisecond)

	checker.Stop()
	assert.False(t, tracker.IsDegraded())
}
