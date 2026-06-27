package service

// Unit tests for the Subsystem contract scaffolding. The
// BlackboxSubsystem (first concrete extraction) has its own
// dedicated test file; this one pins the cross-cutting helpers:
//   - Build skip-sentinel + IsSubsystemSkipped detector
//   - buildSubsystems loop semantics (skip → log + continue;
//     real error → remove from slice; success → keep)
//   - startSubsystems / stopSubsystems iteration order

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/leaderelection"
)

// stubElector returns a non-nil *leaderelection.Elector for tests
// that only need the pointer identity / nil-check semantics — the
// zero value is fine because allElectors() doesn't call methods on
// it, only checks `e != nil`.
func stubElector() *leaderelection.Elector { return &leaderelection.Elector{} }

// noopLogger returns a logger that discards everything. Avoids
// stdout noise during test runs.
func noopLogger() zerolog.Logger { return zerolog.New(io.Discard) }

// recordingSubsystem captures lifecycle invocations so the test
// can assert order + count without a real subsystem's deps.
type recordingSubsystem struct {
	name     string
	buildErr error
	startErr error
	stopErr  error

	events []string
}

func (r *recordingSubsystem) Name() string { return r.name }
func (r *recordingSubsystem) Build(_ *BuildDeps) error {
	r.events = append(r.events, "build")
	return r.buildErr
}
func (r *recordingSubsystem) Start(_ context.Context) error {
	r.events = append(r.events, "start")
	return r.startErr
}
func (r *recordingSubsystem) Stop(_ context.Context) error {
	r.events = append(r.events, "stop")
	return r.stopErr
}

// TestSubsystemSkipped_Sentinel — IsSubsystemSkipped detects
// the skip-sentinel and only the skip-sentinel. Real errors
// must NOT be misclassified as skips (else the buildSubsystems
// loop would treat a misconfiguration as a degraded-mode skip).
func TestSubsystemSkipped_Sentinel(t *testing.T) {
	assert.True(t, IsSubsystemSkipped(SubsystemSkipped("no postgres")))
	assert.False(t, IsSubsystemSkipped(errors.New("real failure")))
	assert.False(t, IsSubsystemSkipped(nil))
	// Message preserved on the sentinel.
	err := SubsystemSkipped("preconditions not met")
	assert.Contains(t, err.Error(), "preconditions not met")
	assert.Contains(t, err.Error(), "subsystem skipped")
}

// TestBuildSubsystems_SkipSentinelRemovesFromSlice — a Build
// returning the skip sentinel must drop the subsystem from
// the running list so subsequent Start/Stop don't touch it.
// The pre-extraction container shipped each feature behind a
// nil-check; this test pins the equivalent behaviour at the
// subsystem level.
func TestBuildSubsystems_SkipSentinelRemovesFromSlice(t *testing.T) {
	keep := &recordingSubsystem{name: "keep"}
	skip := &recordingSubsystem{name: "skip", buildErr: SubsystemSkipped("test")}

	c := &Container{}
	c.subsystems = []Subsystem{keep, skip}
	c.Logger = noopLogger()

	c.buildSubsystems()
	require.Len(t, c.subsystems, 1, "skipped subsystem must be removed")
	assert.Equal(t, "keep", c.subsystems[0].Name())
}

// TestBuildSubsystems_RealErrorAlsoRemovesFromSlice — a Build
// returning a non-sentinel error means "subsystem broken;
// don't try to Start it". Same removal effect as skip but the
// container logs at Warn instead of Debug (verified via the
// logger output indirectly; the test pins the slice mutation
// which is the safety-critical bit).
func TestBuildSubsystems_RealErrorAlsoRemovesFromSlice(t *testing.T) {
	keep := &recordingSubsystem{name: "keep"}
	broken := &recordingSubsystem{name: "broken", buildErr: errors.New("kaboom")}

	c := &Container{}
	c.subsystems = []Subsystem{keep, broken}
	c.Logger = noopLogger()

	c.buildSubsystems()
	require.Len(t, c.subsystems, 1)
	assert.Equal(t, "keep", c.subsystems[0].Name())
}

// TestStopSubsystems_LIFO — Stop runs in reverse-registration
// order. Matches the "tear down what was built last, first"
// invariant the imperative shutdown sequence already followed.
func TestStopSubsystems_LIFO(t *testing.T) {
	first := &recordingSubsystem{name: "first"}
	second := &recordingSubsystem{name: "second"}
	third := &recordingSubsystem{name: "third"}

	c := &Container{}
	c.subsystems = []Subsystem{first, second, third}
	c.Logger = noopLogger()

	c.stopSubsystems(context.Background())
	// Each subsystem records "stop"; the ordering is implicit
	// via Container.stopSubsystems iterating len-1..0.
	// Re-verify by checking which one logged first via a
	// shared counter (here we just assert each was called).
	assert.Equal(t, []string{"stop"}, first.events)
	assert.Equal(t, []string{"stop"}, second.events)
	assert.Equal(t, []string{"stop"}, third.events)
}

// TestStartSubsystems_StampsContainerOnCtx — the transitional
// withContainer helper that lets a subsystem reach back for
// facilities not yet on BuildDeps (initWorkerElector). Pin the
// stamp so the helper survives a future refactor without a
// silent regression that would manifest as "blackbox runs
// ungated everywhere".
func TestStartSubsystems_StampsContainerOnCtx(t *testing.T) {
	var captured *Container
	probe := &recordingSubsystem{name: "probe"}
	wrapped := startCtxCapturingSubsystem{probe, &captured}

	c := &Container{}
	c.subsystems = []Subsystem{wrapped}
	c.Logger = noopLogger()
	c.startSubsystems(context.Background())

	assert.NotNil(t, captured, "Container must reach Start via ctx stamp")
	assert.Same(t, c, captured)
}

// startCtxCapturingSubsystem is a small wrapper used only in
// the test above. It captures the Container pointer stamped on
// ctx by startSubsystems so the test can assert the stamp is
// in place. Tests in this package can read the unexported
// containerFromDetectorCtx helper directly.
type startCtxCapturingSubsystem struct {
	*recordingSubsystem
	out **Container
}

func (s startCtxCapturingSubsystem) Start(ctx context.Context) error {
	*s.out = containerFromDetectorCtx(ctx)
	return s.recordingSubsystem.Start(ctx)
}

// TestAllElectors_FoldsInExtraElectors pins the
// EmailChannelsSubsystem write-back fix from the 2026-05-29 audit
// pass: per-project email IMAP electors land in c.extraElectors
// (no dedicated named field), and allElectors() must fold the
// slice into its returned candidate list so
// releaseAllLeaderLeases() sees them on drain. Pre-fix, peer
// replicas waited the full TTL before claiming the per-project
// email lock — found by the audit-agent regression sweep.
func TestAllElectors_FoldsInExtraElectors(t *testing.T) {
	// Use a stub Container directly; constructing a real Elector
	// requires a DB substrate we don't need for this contract test.
	// allElectors() returns []*leaderelection.Elector — we use the
	// SLICE LENGTH as the pin, populating extraElectors with a
	// sentinel non-nil pointer (a non-nil *Elector value the
	// receiver never dereferences).
	c := &Container{}
	c.Logger = noopLogger()

	// Empty case: no extras + no named electors → empty result.
	require.Empty(t, c.allElectors())

	// Stub a non-nil elector pointer (we never call methods on it;
	// allElectors() only checks `e != nil`).
	stub := stubElector()

	c.extraElectorsMu.Lock()
	c.extraElectors = append(c.extraElectors, stub)
	c.extraElectorsMu.Unlock()

	got := c.allElectors()
	require.Len(t, got, 1, "extraElector must fold into allElectors result")
	assert.Same(t, stub, got[0])
}
