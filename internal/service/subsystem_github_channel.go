package service

// GitHubChannelSubsystem owns the GitHub App conversation
// channel lifecycle. Single-channel (one ConversationChannel
// for the whole installation set), unlike Slack/Email which
// are per-project. Receiver wiring varies based on whether
// the deployment is single- or multi-installation.
//
// Pre-extraction this lived in container.go:1204-1248.

import (
	"context"
	"errors"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/github"
	"vornik.io/vornik/internal/registry"
)

// GitHubChannelSubsystem encapsulates the GitHub App channel.
// Skip preconditions: no channel wired, or no dispatcher. When
// the channel is wired but the dispatcher is missing, we DON'T
// skip — we run inbound-only and log an operator-visible note,
// because the pre-extraction code did the same.
type GitHubChannelSubsystem struct {
	logger zerolog.Logger

	channel  *github.Channel
	projects []*registry.Project // 0 or 1+ installations
	project  *registry.Project   // primary for single-install
}

// NewGitHubChannelSubsystem returns a fresh subsystem.
func NewGitHubChannelSubsystem() *GitHubChannelSubsystem {
	return &GitHubChannelSubsystem{}
}

// Name implements Subsystem.
func (s *GitHubChannelSubsystem) Name() string { return "github_channel" }

// Build captures pre-constructed channel + project state. The
// channel itself is built during NewContainer (auth + webhook
// secret loading happens there). Returns skip when the
// channel isn't wired.
func (s *GitHubChannelSubsystem) Build(deps *BuildDeps) error {
	if deps == nil || deps.Container == nil {
		return SubsystemSkipped("nil deps")
	}
	c := deps.Container
	s.logger = c.Logger.With().Str("subsystem", s.Name()).Logger()

	if c.GitHubChannel == nil {
		return SubsystemSkipped("github channel not configured")
	}
	s.channel = c.GitHubChannel
	s.projects = c.GitHubProjects
	s.project = c.GitHubProject
	return nil
}

// Start wires the receiver + launches the channel goroutine.
// When the dispatcher is missing we log + skip the runtime
// wiring; the channel stays inbound-only (events land in logs).
func (s *GitHubChannelSubsystem) Start(ctx context.Context) error {
	if s == nil || s.channel == nil {
		return nil
	}
	c := containerFromDetectorCtx(ctx)
	if c == nil {
		return nil
	}

	if c.Dispatcher == nil {
		s.logger.Warn().Msg("dispatcher not configured (chat client missing) — @vornik mentions will land in logs only")
		return nil
	}

	// Session-store project resolution: single-install
	// deployments use the legacy constant resolver (every
	// session pinned to the one configured project); multi-
	// install deployments wire the channel itself as the
	// resolver so each session looks up its project via the
	// pin recorded on the first inbound delivery.
	var store *githubSessionStore
	logProjectID := ""
	if len(s.projects) <= 1 {
		if s.project != nil {
			logProjectID = s.project.ID
		}
		store = newGitHubSessionStore(c.Registry, logProjectID)
	} else {
		store = newGitHubSessionStoreWithResolver(c.Registry, s.channel)
		logProjectID = "(multi-installation)"
	}
	store.SetPersister(c.channelSessionPersister("github"))

	receiver := &dispatcher.ChannelReceiver{
		Channel:  s.channel,
		Agent:    c.Dispatcher,
		Sessions: store,
	}
	s.logger.Info().
		Str("project_id", logProjectID).
		Int("installations", len(s.projects)).
		Msg("github-app dispatcher receiver wired")

	go func() {
		if err := s.channel.Start(ctx, receiver); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Warn().Err(err).Msg("github-app channel.Start returned")
		}
	}()
	return nil
}

// Stop is a no-op — channel.Start respects ctx cancellation.
func (s *GitHubChannelSubsystem) Stop(_ context.Context) error { return nil }
