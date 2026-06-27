package service

// Subsystem is the per-feature wiring contract introduced by the
// 2026-05-28 refactor arc's recommended #3. The audit agent's
// proposal: container.go (~2000 lines) wires ~30 optional features
// in one long function. Extracting each feature behind a
// Subsystem-implementing struct shrinks the container's lifecycle
// loop to "register all → build all → start all → stop all", and
// makes each feature unit-testable in isolation.
//
// This file lands the contract. The first concrete extraction —
// BlackboxSubsystem in subsystem_blackbox.go — proves the pattern.
// Subsequent extractions (memory-ingest, trading, ...) follow the
// same shape; container.go's imperative wiring is replaced one
// subsystem at a time, each in its own commit, never with a
// big-bang rewrite.
//
// Pragmatic choice: BuildDeps embeds *Container. The audit agent's
// design called for a curated "narrow primitives" struct, but
// every realistic subsystem reaches for ≥4 container fields, and a
// curated subset just becomes a fanning-out import surface. We
// pass the container by pointer for now; if a future subsystem
// has a genuinely small dependency footprint, that subsystem can
// take its narrow interface as a build parameter instead. The
// container itself is the right scope of dependency injection
// today.

import (
	"context"
)

// Subsystem is the per-feature lifecycle contract.
type Subsystem interface {
	// Name identifies the subsystem in logs + future doctor
	// surfaces. Convention: lowercase, snake_case, matches the
	// feature's elector lease name where applicable (e.g.
	// "blackbox_detector").
	Name() string

	// Build constructs the subsystem's internal state from the
	// shared dependencies. Called once during daemon boot, after
	// every dependency on BuildDeps is itself wired. Returning a
	// non-nil error from Build means "skip this subsystem"; the
	// container logs the reason and continues with the rest.
	// Subsystems that legitimately can't run (missing repo on
	// sqlite, etc.) return a sentinel-shape error matched by
	// errors.Is or a typed status — fail-fast is for genuine
	// config bugs, not degraded-mode operation.
	Build(deps *BuildDeps) error

	// Start launches the subsystem's goroutines + background
	// workers. Called after every subsystem's Build has returned.
	// MUST NOT BLOCK — subsystems with long-running loops kick
	// them off via `go ...` and return immediately. Returning a
	// non-nil error means "subsystem failed to start"; the
	// container logs + continues (degraded mode).
	Start(ctx context.Context) error

	// Stop performs graceful shutdown. Called during daemon drain.
	// The ctx is bounded by the daemon's shutdown grace window
	// (default 30s); subsystems should respect ctx.Done() rather
	// than blocking indefinitely. Stop is best-effort: an error
	// here is logged but doesn't abort other subsystems' drain.
	Stop(ctx context.Context) error
}

// BuildDeps is the dependency-injection seam. Embeds *Container so
// subsystems can reach every wired primitive without an
// import-surface explosion; future refactors can narrow this once
// the right slices have emerged from N extractions. The level of
// indirection that matters now is "subsystems get their deps from
// ONE source, not from package-level globals" — not "deps are
// minimal-surface" (which becomes premature abstraction at this
// stage).
type BuildDeps struct {
	*Container
}

// SubsystemSkipped is the sentinel error a Subsystem.Build returns
// when the deployment legitimately can't host it (e.g. SQLite-only
// deployment + a postgres-only subsystem). The container's run
// loop treats this as "log + skip", not "abort". Distinct from a
// real Build error so misconfigurations stay visible.
type subsystemSkipped struct {
	reason string
}

func (e *subsystemSkipped) Error() string { return "subsystem skipped: " + e.reason }

// SubsystemSkipped builds a skip-sentinel error. Use this in
// Subsystem.Build when the deployment doesn't support the
// feature; the container logs the reason and continues.
func SubsystemSkipped(reason string) error { return &subsystemSkipped{reason: reason} }

// IsSubsystemSkipped reports whether err is a skip sentinel.
func IsSubsystemSkipped(err error) bool {
	if err == nil {
		return false
	}
	_, ok := err.(*subsystemSkipped)
	return ok
}
