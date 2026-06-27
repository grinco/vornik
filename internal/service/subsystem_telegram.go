package service

// TelegramSubsystem owns the Telegram bot's poll-loop lifecycle.
// The bot itself is constructed in NewContainer (initialisation
// ordering: the dispatcher needs a wired bot to register itself
// as a CompletionNotifier — see container.go:623). This
// subsystem only owns the runtime side: the leader elector
// bootstrap + the bot.Start() call.
//
// Pre-extraction this lived in container.go:1186-1202. The
// telegramPollerElector field stays on Container so allElectors()
// keeps draining it on shutdown without a parallel refactor.

import (
	"context"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/telegram"
)

// TelegramSubsystem encapsulates the Telegram bot's runtime
// lifecycle. Skip preconditions: no bot (deployment without
// a Telegram block).
type TelegramSubsystem struct {
	logger zerolog.Logger
	bot    *telegram.Bot
}

// NewTelegramSubsystem returns a fresh subsystem instance.
func NewTelegramSubsystem() *TelegramSubsystem {
	return &TelegramSubsystem{}
}

// Name implements Subsystem.
func (s *TelegramSubsystem) Name() string { return "telegram_channel" }

// Build captures the already-constructed bot pointer. Returns a
// skip-sentinel when no bot is wired (deployment without a
// Telegram block in config). The bot itself is built in
// NewContainer, not here — Build only reads the wired state.
func (s *TelegramSubsystem) Build(deps *BuildDeps) error {
	if deps == nil || deps.Container == nil {
		return SubsystemSkipped("nil deps")
	}
	c := deps.Container
	s.logger = c.Logger.With().Str("subsystem", s.Name()).Logger()

	if c.TelegramBot == nil {
		return SubsystemSkipped("telegram bot not configured")
	}
	s.bot = c.TelegramBot
	return nil
}

// Start runs the elector bootstrap (so first pollLoop iteration
// sees an authoritative IsLeader bit) then bot.Start. A start
// failure is non-fatal: the daemon continues without Telegram.
func (s *TelegramSubsystem) Start(ctx context.Context) error {
	if s == nil || s.bot == nil {
		return nil
	}
	c := containerFromDetectorCtx(ctx)

	// Bootstrap the poller elector synchronously — without this,
	// the first pollLoop iteration could race the elector's first
	// acquire and log a spurious "not the leader" line. Nil
	// elector (SQLite single-process / unwired LeaderLocks)
	// leaves the bot polling on every replica, the legacy
	// behaviour.
	if c != nil && c.telegramPollerElector != nil {
		c.telegramPollerElector.BootstrapAcquire(ctx)
		go c.telegramPollerElector.Run(ctx)
	}

	if err := s.bot.Start(ctx); err != nil {
		s.logger.Warn().Err(err).Msg("failed to start telegram bot (continuing without telegram)")
		return nil
	}
	s.logger.Info().Msg("telegram bot started")
	return nil
}

// Stop is a no-op — the bot's internal goroutines respect ctx
// cancellation handed down through the daemon's drain loop.
func (s *TelegramSubsystem) Stop(_ context.Context) error { return nil }
