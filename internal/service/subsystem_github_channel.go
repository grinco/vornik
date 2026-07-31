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
)

// GitHubChannelSubsystem encapsulates the GitHub App channel.
// Skip preconditions: no channel wired, or no dispatcher. When
// the channel is wired but the dispatcher is missing, we DON'T
// skip — we run inbound-only and log an operator-visible note,
// because the pre-extraction code did the same.
type GitHubChannelSubsystem struct {
	logger zerolog.Logger
}

// NewGitHubChannelSubsystem returns a fresh subsystem.
func NewGitHubChannelSubsystem() *GitHubChannelSubsystem {
	return &GitHubChannelSubsystem{}
}

// Name implements Subsystem.
func (s *GitHubChannelSubsystem) Name() string { return "github_channel" }

// Build validates that GitHub App was configured and captures only
// the subsystem logger. The channel itself is read at Start because
// observability initialization rebuilds the HTTP server and replaces
// the instance mounted by the first initialization.
func (s *GitHubChannelSubsystem) Build(deps *BuildDeps) error {
	if deps == nil || deps.Container == nil {
		return SubsystemSkipped("nil deps")
	}
	c := deps.Container
	s.logger = c.Logger.With().Str("subsystem", s.Name()).Logger()

	if c.GitHubChannel == nil {
		return SubsystemSkipped("github channel not configured")
	}
	return nil
}

// Start wires the receiver + launches the channel goroutine.
// When the dispatcher is missing we log + skip the runtime
// wiring; the channel stays inbound-only (events land in logs).
func (s *GitHubChannelSubsystem) Start(ctx context.Context) error {
	if s == nil {
		return nil
	}
	c := containerFromDetectorCtx(ctx)
	if c == nil || c.GitHubChannel == nil {
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
	if len(c.GitHubProjects) <= 1 {
		if c.GitHubProject != nil {
			logProjectID = c.GitHubProject.ID
		}
		store = newGitHubSessionStore(c.Registry, logProjectID)
	} else {
		store = newGitHubSessionStoreWithResolver(c.Registry, c.GitHubChannel)
		logProjectID = "(multi-installation)"
	}
	store.SetPersister(c.channelSessionPersister("github"))

	receiver := &dispatcher.ChannelReceiver{
		Channel:                  c.GitHubChannel,
		Agent:                    c.Dispatcher,
		Sessions:                 store,
		Disclosure:               c.AIDisclosure,
		Media:                    c.mediaSight(),
		DisclosureMetrics:        c.disclosureObserver(),
		MemoryWriteConfirmations: c.chatMemoryConfirmations(),
	}
	s.logger.Info().
		Str("project_id", logProjectID).
		Int("installations", len(c.GitHubProjects)).
		Msg("github-app dispatcher receiver wired")

	channel := c.GitHubChannel
	go func() {
		if err := channel.Start(ctx, receiver); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Warn().Err(err).Msg("github-app channel.Start returned")
		}
	}()
	return nil
}

// Stop is a no-op — channel.Start respects ctx cancellation.
func (s *GitHubChannelSubsystem) Stop(_ context.Context) error { return nil }
