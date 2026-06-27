package service

// ArchiveSweeperSubsystem owns the archive-sweeper lifecycle.
// Hourly tick + on-demand kick from the UI's delete-now button.
// Non-fatal — daemon comes up either way; the archive YAML is
// the source of truth, so a future daemon-restart with the
// sweeper wired will pick up overdue projects.
//
// Pre-extraction this lived in container.go:1087-1107 as an
// imperative `if c.archiveSweeper == nil { ... }` block.

import (
	"context"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/projectarchive"
)

type ArchiveSweeperSubsystem struct {
	logger  zerolog.Logger
	sweeper *projectarchive.Sweeper
}

func NewArchiveSweeperSubsystem() *ArchiveSweeperSubsystem {
	return &ArchiveSweeperSubsystem{}
}

func (s *ArchiveSweeperSubsystem) Name() string { return "archive_sweeper" }

func (s *ArchiveSweeperSubsystem) Build(deps *BuildDeps) error {
	if deps == nil || deps.Container == nil {
		return SubsystemSkipped("nil deps")
	}
	c := deps.Container
	s.logger = c.Logger.With().Str("subsystem", s.Name()).Logger()

	if c.archiveSweeper == nil {
		c.archiveSweeper = c.initArchiveSweeper()
	}
	if c.archiveSweeper == nil {
		return SubsystemSkipped("archive sweeper not configured")
	}
	s.sweeper = c.archiveSweeper
	return nil
}

func (s *ArchiveSweeperSubsystem) Start(ctx context.Context) error {
	if s == nil || s.sweeper == nil {
		return nil
	}
	c := containerFromDetectorCtx(ctx)
	// Synchronously acquire the leader lock BEFORE goroutines
	// start. Otherwise the sweeper's first immediate tick can
	// run before the elector's first acquire lands, logging a
	// spurious "not the leader" line on every restart. Elector
	// is constructed during initArchiveSweeper (lives on
	// container.archiveSweeperElector).
	if c != nil && c.archiveSweeperElector != nil {
		c.archiveSweeperElector.BootstrapAcquire(ctx)
		go c.archiveSweeperElector.Run(ctx)
	}
	go s.sweeper.Run(ctx)
	s.logger.Info().Msg("archive-sweeper started")
	return nil
}

func (s *ArchiveSweeperSubsystem) Stop(_ context.Context) error { return nil }
