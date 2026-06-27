package service

// RemindersSubsystem owns the reminders heartbeat lifecycle.
// Initialised earlier in container build so the dispatcher has
// its Kicker; the goroutine starts here alongside every other
// long-running worker.
//
// Pre-extraction this lived in container.go:1112-1128 as an
// imperative `if c.reminderRunner != nil { ... }` block.

import (
	"context"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/reminders"
)

type RemindersSubsystem struct {
	logger zerolog.Logger
	runner *reminders.Runner
}

func NewRemindersSubsystem() *RemindersSubsystem {
	return &RemindersSubsystem{}
}

func (s *RemindersSubsystem) Name() string { return "reminders_runner" }

func (s *RemindersSubsystem) Build(deps *BuildDeps) error {
	if deps == nil || deps.Container == nil {
		return SubsystemSkipped("nil deps")
	}
	c := deps.Container
	s.logger = c.Logger.With().Str("subsystem", s.Name()).Logger()
	if c.reminderRunner == nil {
		return SubsystemSkipped("reminders runner not configured")
	}
	s.runner = c.reminderRunner
	return nil
}

func (s *RemindersSubsystem) Start(ctx context.Context) error {
	if s == nil || s.runner == nil {
		return nil
	}
	c := containerFromDetectorCtx(ctx)
	// Leader-gate the heartbeat so multi-replica deployments only
	// claim due rows once per interval globally. Nil elector
	// (SQLite branch) keeps legacy behaviour. Kick remains an
	// in-process signal — non-leader replicas can still receive
	// Kick but tickOnce short-circuits on the gate. Cross-instance
	// Kick propagation is a follow-on (LISTEN/NOTIFY slice in §3
	// of the design doc).
	if c != nil {
		c.remindersElector = c.initWorkerElector(s.Name())
		if c.remindersElector != nil {
			s.runner.SetLeaderGate(c.remindersElector)
			c.remindersElector.BootstrapAcquire(ctx)
			go c.remindersElector.Run(ctx)
		}
	}
	go s.runner.Run(ctx)
	s.logger.Info().Msg("reminders heartbeat started")
	return nil
}

func (s *RemindersSubsystem) Stop(_ context.Context) error { return nil }
