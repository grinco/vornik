package service

// WatchdogSubsystem owns the stuck-execution watchdog lifecycle.
// Independent of the scheduler — runs as a periodic scan against
// the task repo + executor's IsExecuting set, marking RUNNING
// tasks whose execution is gone as FAILED.
//
// Pre-extraction this lived in container.go:1051-1066 as an
// imperative `if c.Watchdog != nil { ... }` block.

import (
	"context"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/watchdog"
)

type WatchdogSubsystem struct {
	logger zerolog.Logger
	wd     *watchdog.Watchdog
}

func NewWatchdogSubsystem() *WatchdogSubsystem {
	return &WatchdogSubsystem{}
}

func (s *WatchdogSubsystem) Name() string { return "watchdog" }

func (s *WatchdogSubsystem) Build(deps *BuildDeps) error {
	if deps == nil || deps.Container == nil {
		return SubsystemSkipped("nil deps")
	}
	c := deps.Container
	s.logger = c.Logger.With().Str("subsystem", s.Name()).Logger()
	if c.Watchdog == nil {
		return SubsystemSkipped("watchdog not configured")
	}
	s.wd = c.Watchdog
	return nil
}

func (s *WatchdogSubsystem) Start(ctx context.Context) error {
	if s == nil || s.wd == nil {
		return nil
	}
	c := containerFromDetectorCtx(ctx)
	if c != nil {
		c.watchdogElector = c.initWorkerElector(s.Name())
		if c.watchdogElector != nil {
			s.wd.SetLeaderGate(c.watchdogElector)
			c.watchdogElector.BootstrapAcquire(ctx)
			go c.watchdogElector.Run(ctx)
		}
	}
	if err := s.wd.Start(); err != nil {
		// Watchdog start failure is non-fatal: it's a safety net,
		// not the critical path. Log loudly so operators see it,
		// but let the daemon come up.
		s.logger.Error().Err(err).Msg("watchdog failed to start — daemon will run without stuck-execution detection")
		return nil
	}
	s.logger.Info().Msg("watchdog started")
	return nil
}

func (s *WatchdogSubsystem) Stop(_ context.Context) error { return nil }
