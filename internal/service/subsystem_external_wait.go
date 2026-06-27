package service

// ExternalWaitSubsystem owns the external_wait deadline monitor's
// lifecycle. Phase-30 worker that re-queues AWAITING_EXTERNAL
// tasks whose expected_by has passed, plus auto-closes COMPLETED
// tasks whose closure_request grace window has elapsed
// (scanClosureGrace).
//
// Pre-extraction the construction + start lived in
// container.go:1027-1049, intermixed with scheduler init.
// Construction happens in Build (after Scheduler is wired); Start
// runs the monitor + elector goroutines.

import (
	"context"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/scheduler"
)

type ExternalWaitSubsystem struct {
	logger  zerolog.Logger
	monitor *scheduler.ExternalWaitMonitor
}

func NewExternalWaitSubsystem() *ExternalWaitSubsystem {
	return &ExternalWaitSubsystem{}
}

func (s *ExternalWaitSubsystem) Name() string { return "external_wait_monitor" }

func (s *ExternalWaitSubsystem) Build(deps *BuildDeps) error {
	if deps == nil || deps.Container == nil {
		return SubsystemSkipped("nil deps")
	}
	c := deps.Container
	s.logger = c.Logger.With().Str("subsystem", s.Name()).Logger()
	if c.Scheduler == nil {
		return SubsystemSkipped("scheduler not wired")
	}
	if c.repos == nil || c.repos.Tasks == nil || c.repos.Messages == nil {
		return SubsystemSkipped("task/messages repos not wired")
	}
	s.monitor = scheduler.NewExternalWaitMonitor(
		c.repos.Tasks,
		c.repos.Tasks,
		c.repos.Messages,
		c.Scheduler,
		60*time.Second,
		c.Logger.With().Str("component", "external_wait").Logger(),
	)
	// Write back onto Container for the existing field reference
	// (container_http.go's drain reads externalWaitMonitor for the
	// nil-check). Lifts to a Subsystem-facing surface in a later pass.
	c.externalWaitMonitor = s.monitor
	return nil
}

func (s *ExternalWaitSubsystem) Start(ctx context.Context) error {
	if s == nil || s.monitor == nil {
		return nil
	}
	c := containerFromDetectorCtx(ctx)
	// Leader-gate so only one replica re-queues AWAITING_EXTERNAL
	// tasks. Nil elector (SQLite branch) keeps legacy behaviour.
	if c != nil {
		c.externalWaitElector = c.initWorkerElector(s.Name())
		if c.externalWaitElector != nil {
			s.monitor.SetLeaderGate(c.externalWaitElector)
			c.externalWaitElector.BootstrapAcquire(ctx)
			go c.externalWaitElector.Run(ctx)
		}
	}
	s.monitor.Start(collectorsCtxFrom(ctx, c))
	return nil
}

func (s *ExternalWaitSubsystem) Stop(_ context.Context) error { return nil }
