package service

// CPCTimeoutSubsystem owns the cross-project-call timeout
// scanner's lifecycle. Sweeps `cross_project_calls` rows past
// their `timeout_at` and resolves them as `timed_out` so the
// caller's `on_fail` branch fires (LLD §8.1).
//
// Pre-extraction this lived in container.go:1169-1192. Skip
// preconditions: no executor (sqlite/test deployment), or
// executor.NewCPCTimeoutScanner returns nil (CPC repo not wired
// — feature flag off, postgres-only). Matches the
// pre-extraction nested-nil-checks shape exactly.

import (
	"context"
	"errors"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/executor"
)

// CPCTimeoutSubsystem encapsulates the timeout scanner + its
// leader gate. Scanner construction happens in Build; the
// elector + goroutine launches happen in Start.
type CPCTimeoutSubsystem struct {
	logger  zerolog.Logger
	scanner *executor.CPCTimeoutScanner
}

// NewCPCTimeoutSubsystem returns a fresh subsystem.
func NewCPCTimeoutSubsystem() *CPCTimeoutSubsystem {
	return &CPCTimeoutSubsystem{}
}

// Name implements Subsystem. Matches the elector lease name.
func (s *CPCTimeoutSubsystem) Name() string { return "cpc_timeout_scanner" }

// Build constructs the scanner from the executor. Returns a
// skip-sentinel when the executor isn't wired or the scanner's
// constructor returns nil (CPC repo not wired — feature flag
// off, or sqlite deployment).
func (s *CPCTimeoutSubsystem) Build(deps *BuildDeps) error {
	if deps == nil || deps.Container == nil {
		return SubsystemSkipped("nil deps")
	}
	c := deps.Container
	s.logger = c.Logger.With().Str("subsystem", s.Name()).Logger()

	if c.Executor == nil {
		return SubsystemSkipped("executor not wired")
	}
	sc := executor.NewCPCTimeoutScanner(c.Executor)
	if sc == nil {
		return SubsystemSkipped("CPC repo not wired (feature flag off or sqlite)")
	}
	s.scanner = sc
	return nil
}

// Start mints the leader elector + launches the scanner
// goroutine. Nil elector (SQLite branch) keeps legacy
// "run on every replica" semantics.
func (s *CPCTimeoutSubsystem) Start(ctx context.Context) error {
	if s == nil || s.scanner == nil {
		return nil
	}
	c := containerFromDetectorCtx(ctx)
	if c == nil {
		return nil
	}

	c.cpcTimeoutElector = c.initWorkerElector(s.Name())
	if c.cpcTimeoutElector != nil {
		s.scanner.SetLeaderGate(c.cpcTimeoutElector)
		c.cpcTimeoutElector.BootstrapAcquire(ctx)
		go c.cpcTimeoutElector.Run(ctx)
	}
	go func() {
		if err := s.scanner.Run(collectorsCtxFrom(ctx, c)); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Warn().Err(err).Msg("cpc timeout scanner exited unexpectedly")
		}
	}()
	s.logger.Info().Msg("cpc timeout scanner started")
	return nil
}

// Stop is a no-op — the scanner respects collectorsCtx
// cancellation handed down through the daemon's drain.
func (s *CPCTimeoutSubsystem) Stop(_ context.Context) error { return nil }
