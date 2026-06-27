package service

// EffectiveCostSubsystem owns the effective-cost drift monitor.
// Compares recent per-project task cost against a baseline window
// and emits Prometheus + log alerts when the ratio exceeds the
// configured threshold. No elector — cheap read-mostly scan,
// running on every replica is fine.
//
// Pre-extraction this lived in container.go:1068-1076 as an
// imperative `if c.EffectiveCostMon != nil { ... }` block.

import (
	"context"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/budget"
)

type EffectiveCostSubsystem struct {
	logger  zerolog.Logger
	monitor *budget.EffectiveCostMonitor
}

func NewEffectiveCostSubsystem() *EffectiveCostSubsystem {
	return &EffectiveCostSubsystem{}
}

func (s *EffectiveCostSubsystem) Name() string { return "effective_cost_monitor" }

func (s *EffectiveCostSubsystem) Build(deps *BuildDeps) error {
	if deps == nil || deps.Container == nil {
		return SubsystemSkipped("nil deps")
	}
	c := deps.Container
	s.logger = c.Logger.With().Str("subsystem", s.Name()).Logger()
	if c.EffectiveCostMon == nil {
		return SubsystemSkipped("effective-cost monitor not configured")
	}
	s.monitor = c.EffectiveCostMon
	return nil
}

func (s *EffectiveCostSubsystem) Start(_ context.Context) error {
	if s == nil || s.monitor == nil {
		return nil
	}
	if err := s.monitor.Start(); err != nil {
		// Non-fatal — same posture as the watchdog: drift alerts
		// are valuable but the daemon still works without them.
		s.logger.Error().Err(err).Msg("effective-cost monitor failed to start — daemon will run without drift alerts")
		return nil
	}
	s.logger.Info().Msg("effective-cost monitor started")
	return nil
}

func (s *EffectiveCostSubsystem) Stop(_ context.Context) error { return nil }
