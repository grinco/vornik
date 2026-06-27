package service

// RateLimitCounterSweepSubsystem owns the postgres-only periodic
// sweep of expired ratelimit_counters rows. Idempotent:
// SweepExpired DELETEs window_start < cutoff, so running on the
// wrong replica is wasted work, not corruption. Leader-elected
// anyway to keep DB load proportional to one instance.
//
// Pre-extraction this lived in container.go:1140-1148 as an
// imperative `if c.rateLimiterPostgres != nil { ... }` block.

import (
	"context"

	"github.com/rs/zerolog"
)

type RateLimitCounterSweepSubsystem struct {
	logger zerolog.Logger
	wired  bool // true when rateLimiterPostgres was non-nil at Build
}

func NewRateLimitCounterSweepSubsystem() *RateLimitCounterSweepSubsystem {
	return &RateLimitCounterSweepSubsystem{}
}

func (s *RateLimitCounterSweepSubsystem) Name() string { return "ratelimit_counter_sweeper" }

func (s *RateLimitCounterSweepSubsystem) Build(deps *BuildDeps) error {
	if deps == nil || deps.Container == nil {
		return SubsystemSkipped("nil deps")
	}
	c := deps.Container
	s.logger = c.Logger.With().Str("subsystem", s.Name()).Logger()
	if c.rateLimiterPostgres == nil {
		return SubsystemSkipped("postgres limiter not configured")
	}
	s.wired = true
	return nil
}

func (s *RateLimitCounterSweepSubsystem) Start(ctx context.Context) error {
	if s == nil || !s.wired {
		return nil
	}
	c := containerFromDetectorCtx(ctx)
	if c == nil {
		return nil
	}
	c.ratelimitCounterSweepElector = c.initWorkerElector(s.Name())
	if c.ratelimitCounterSweepElector != nil {
		c.ratelimitCounterSweepElector.BootstrapAcquire(ctx)
		go c.ratelimitCounterSweepElector.Run(ctx)
	}
	go c.runRateLimiterCounterSweep(ctx)
	s.logger.Info().Msg("ratelimit counter sweeper started")
	return nil
}

func (s *RateLimitCounterSweepSubsystem) Stop(_ context.Context) error { return nil }
