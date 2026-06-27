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

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/slack"
)

// SlackChannelsSubsystem encapsulates the slice of Slack
// channels. Skip preconditions: empty channel slice (no
// project declared a slack block).
type SlackChannelsSubsystem struct {
	logger zerolog.Logger

	channels []*slack.Channel
	projects []*registry.Project
}

// NewSlackChannelsSubsystem returns a fresh subsystem.
func NewSlackChannelsSubsystem() *SlackChannelsSubsystem {
	return &SlackChannelsSubsystem{}
}

// Name implements Subsystem.
func (s *SlackChannelsSubsystem) Name() string { return "slack_channels" }

// Build captures pre-constructed channel/project slices. The
// channels are built during NewContainer (signing-secret + bot
// token loading happens there).
func (s *SlackChannelsSubsystem) Build(deps *BuildDeps) error {
	if deps == nil || deps.Container == nil {
		return SubsystemSkipped("nil deps")
	}
	c := deps.Container
	s.logger = c.Logger.With().Str("subsystem", s.Name()).Logger()

	if len(c.SlackChannels) == 0 {
		return SubsystemSkipped("no slack channels configured")
	}
	s.channels = c.SlackChannels
	s.projects = c.SlackProjects
	return nil
}

// Start wires a per-project receiver for each channel + launches
// one goroutine per channel. Dispatcher-missing logs + skips the
// runtime wiring; the channels are inbound-only (events log).
func (s *SlackChannelsSubsystem) Start(ctx context.Context) error {
	if s == nil || len(s.channels) == 0 {
		return nil
	}
	c := containerFromDetectorCtx(ctx)
	if c == nil {
		return nil
	}

	if c.Dispatcher == nil {
		s.logger.Warn().
			Int("channels", len(s.channels)).
			Msg("dispatcher not configured (chat client missing) — inbound events will land in logs only")
		return nil
	}

	for i, ch := range s.channels {
		project := s.projects[i]
		store := newSlackSessionStore(c.Registry, project.ID)
		store.SetPersister(c.channelSessionPersister("slack"))
		receiver := &dispatcher.ChannelReceiver{
			Channel:  ch,
			Agent:    c.Dispatcher,
			Sessions: store,
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
	return nil
}

// Stop is a no-op — channel.Start respects ctx cancellation.
func (s *SlackChannelsSubsystem) Stop(_ context.Context) error { return nil }
