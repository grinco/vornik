package service

// subsystem_blackbox.go — context-value utilities for the container-stamping
// seam used by all subsystems (both CE and EE) to reach back into the Container
// from their Start method.
//
// The BlackboxSubsystem struct has moved to internal/enterprise/blackbox/subsystem.go
// (Task 6 step 2). This file retains only the shared context primitives because
// they are used by the dozen+ CE subsystems that still need container access
// in their Start() (e.g. subsystem_slack_channels, subsystem_trading, etc.).

import "context"

// ctxContainerKey is the context key used to stamp the Container onto the
// subsystem Start context (withContainer) so subsystems can retrieve it via
// containerFromDetectorCtx / ContainerFromDetectorCtx.
type ctxContainerKey struct{}

// containerFromDetectorCtx retrieves the Container stamped on ctx by
// startSubsystems (via withContainer). Returns nil when the stamp is absent
// (e.g. in unit tests that call Start directly without a full boot sequence).
func containerFromDetectorCtx(ctx context.Context) *Container {
	if v, ok := ctx.Value(ctxContainerKey{}).(*Container); ok {
		return v
	}
	return nil
}

// ContainerFromDetectorCtx is the exported variant of containerFromDetectorCtx
// for use by EE subsystems (in internal/enterprise/*) that have moved out of
// the service package. Behaviour is identical — returns the Container stamped on
// ctx by startSubsystems, or nil when absent.
func ContainerFromDetectorCtx(ctx context.Context) *Container {
	return containerFromDetectorCtx(ctx)
}

// withContainer stamps c onto ctx for the transitional period during which
// subsystems need to reach back into the container. Called from startSubsystems.
// Internal-only; callers outside this package use ContainerFromDetectorCtx.
func withContainer(ctx context.Context, c *Container) context.Context {
	return context.WithValue(ctx, ctxContainerKey{}, c)
}
