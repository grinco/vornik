package service

// SlackChannelsSubsystem owns the Slack channels lifecycle.
// Per-project (one ConversationChannel per project with a
// fully-configured slack block); mirrors EmailChannelsSubsystem's
// shape. Slack is webhook-driven so Channel.Start blocks on
// ctx.Done rather than running a poll loop, but the goroutine
// layout is the same as Telegram/GitHub.
//
// Pre-extraction this lived in container.go:1250-1285.

import (
	"context"
	"errors"
	"fmt"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/dispatcher"
)

// SlackChannelsSubsystem owns the Slack channel lifecycle. Channel
// instances are read from the container at Start time because
// initHTTPServer rebuilds them after subsystem Build.
type SlackChannelsSubsystem struct {
	logger zerolog.Logger
}

// NewSlackChannelsSubsystem returns a fresh subsystem.
func NewSlackChannelsSubsystem() *SlackChannelsSubsystem {
	return &SlackChannelsSubsystem{}
}

// Name implements Subsystem.
func (s *SlackChannelsSubsystem) Name() string { return "slack_channels" }

// Build validates that Slack was configured and captures only the
// subsystem logger. Channel instances must not be captured here:
// observability initialization rebuilds the HTTP server (and its
// mounted Slack channels) after subsystem Build.
func (s *SlackChannelsSubsystem) Build(deps *BuildDeps) error {
	if deps == nil || deps.Container == nil {
		return SubsystemSkipped("nil deps")
	}
	c := deps.Container
	s.logger = c.Logger.With().Str("subsystem", s.Name()).Logger()

	if len(c.SlackChannels) == 0 {
		return SubsystemSkipped("no slack channels configured")
	}
	return nil
}

// Start wires a per-project receiver for each channel + launches
// one goroutine per channel. Dispatcher-missing logs + skips the
// runtime wiring; the channels are inbound-only (events log).
func (s *SlackChannelsSubsystem) Start(ctx context.Context) error {
	if s == nil {
		return nil
	}
	c := containerFromDetectorCtx(ctx)
	if c == nil || len(c.SlackChannels) == 0 {
		return nil
	}
	if len(c.SlackChannels) != len(c.SlackProjects) {
		return fmt.Errorf(
			"slack channel/project slice length mismatch: %d channels, %d projects",
			len(c.SlackChannels),
			len(c.SlackProjects),
		)
	}

	if c.Dispatcher == nil {
		s.logger.Warn().
			Int("channels", len(c.SlackChannels)).
			Msg("dispatcher not configured (chat client missing) — inbound events will land in logs only")
		return nil
	}

	// Collected across the loop and wired once, below.
	var threadReaders slackThreadReaderSet

	for i, ch := range c.SlackChannels {
		project := c.SlackProjects[i]
		maxHistoryTokens := c.Config.Chat.MaxHistoryTokens
		if maxHistoryTokens == 0 && c.Config.Chat.ContextSize > 0 {
			maxHistoryTokens = c.Config.Chat.ContextSize * 70 / 100
		} else if maxHistoryTokens < 0 {
			maxHistoryTokens = 0
		}
		store := newSlackSessionStoreWithLimits(
			c.Registry,
			project.ID,
			c.Config.Chat.MaxHistory,
			maxHistoryTokens,
		)
		store.SetPersister(c.channelSessionPersister("slack"))
		// Back get_channel_thread so the lead can pull an earlier thread in the
		// same channel when a channel-level follow-up refers to one.
		// Late-bound: stores are built here, after the dispatcher agent.
		//
		// Accumulated across channels rather than overwritten — with several
		// Slack workspaces wired, a single store's in-memory map only holds its
		// own channel's threads, so keeping the last one would silently lose
		// in-process history for every other workspace.
		threadReaders = append(threadReaders, store)
		// Let the channel recognise a thread it is already part of, so a follow-up
		// reply that carries no mention still starts a turn (incident 2026-07-30).
		// Bound per channel: engagement is answered by THIS workspace's store.
		ch.SetThreadEngagementChecker(store)
		receiver := &dispatcher.ChannelReceiver{
			Channel:           ch,
			Agent:             c.Dispatcher,
			Sessions:          store,
			Disclosure:        c.AIDisclosure,
			Media:             c.mediaSight(),
			DisclosureMetrics: c.disclosureObserver(),
		}
		s.logger.Info().
			Str("project_id", project.ID).
			Str("team_id", project.Slack.TeamID).
			Msg("slack dispatcher receiver wired")

		// Capture loop variables — every goroutine needs its own
		// channel+project pair (Go ≤1.21 semantics).
		ch := ch
		projectID := project.ID
		go func() {
			if err := ch.Start(ctx, receiver); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Warn().Err(err).Str("project_id", projectID).Msg("slack channel.Start returned")
			}
		}()
	}
	if len(threadReaders) > 0 {
		c.Dispatcher.SetChannelThreadReader(threadReaders)
	}
	// Let the dispatcher say what it is doing mid-turn. Wired here because Slack is the
	// only channel that can display a transient status today; the reporter itself is
	// channel-agnostic and skips channels that cannot.
	c.Dispatcher.SetTurnStatusReporter(&turnStatusReporter{
		resolver: &containerChannelResolver{c: c},
	})
	return nil
}

// Stop is a no-op — channel.Start respects ctx cancellation.
func (s *SlackChannelsSubsystem) Stop(_ context.Context) error { return nil }
