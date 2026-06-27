package service

// EmailChannelsSubsystem owns the email channels lifecycle plus
// the cross-channel CompletionNotifier multiplex wiring. Per-
// project (one ConversationChannel per project with a fully-
// configured email block); each channel gets its own
// per-project elector ("email_imap_receiver_<projectID>") so
// projects fail over independently — losing one project's lease
// doesn't take down email for the rest.
//
// Two responsibilities beyond per-channel start:
//   - SetCompletionNotifier on the executor with a multi-channel
//     fan-out (TelegramBot + every EmailChannel). Without this,
//     tasks created in an email session resume on Telegram only.
//   - SetChannelFollowupRegistrar on the dispatcher for each
//     channel so create_task records sessionID→taskID against
//     the channel that produced the inbound message.
//
// Pre-extraction this lived in container.go:1287-1376.

import (
	"context"
	"errors"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/email"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/registry"
)

// EmailChannelsSubsystem encapsulates the slice of email
// channels + the cross-channel notifier wiring.
type EmailChannelsSubsystem struct {
	logger zerolog.Logger

	channels []*email.Channel
	projects []*registry.Project
}

// NewEmailChannelsSubsystem returns a fresh subsystem.
func NewEmailChannelsSubsystem() *EmailChannelsSubsystem {
	return &EmailChannelsSubsystem{}
}

// Name implements Subsystem.
func (s *EmailChannelsSubsystem) Name() string { return "email_channels" }

// Build captures pre-constructed channel/project slices.
func (s *EmailChannelsSubsystem) Build(deps *BuildDeps) error {
	if deps == nil || deps.Container == nil {
		return SubsystemSkipped("nil deps")
	}
	c := deps.Container
	s.logger = c.Logger.With().Str("subsystem", s.Name()).Logger()

	if len(c.EmailChannels) == 0 {
		return SubsystemSkipped("no email channels configured")
	}
	s.channels = c.EmailChannels
	s.projects = c.EmailProjects
	return nil
}

// Start wires per-project receivers + per-project electors +
// launches one goroutine per channel. After all channels are
// launched, runs the multi-channel CompletionNotifier wiring +
// per-channel followup-registrar wiring.
func (s *EmailChannelsSubsystem) Start(ctx context.Context) error {
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
			Msg("dispatcher not configured (chat client missing) — inbound mail will land in logs only")
		return nil
	}

	for i, ch := range s.channels {
		project := s.projects[i]
		store := newEmailSessionStore(c.Registry, project.ID, ch)
		store.SetPersister(c.channelSessionPersister("email"))
		receiver := &dispatcher.ChannelReceiver{
			Channel:  ch,
			Agent:    c.Dispatcher,
			Sessions: store,
		}

		// Cluster gate (per-project): only the elected leader for
		// THIS project's lock fetches mail. Per-project locks let
		// projects fail-over independently. Nil elector
		// (single-process default) leaves the cycle running
		// unconditionally.
		//
		// Write back to Container.extraElectors so allElectors()
		// (used by the drain loop to release leases before DB
		// close) sees this elector — without the write-back,
		// peer replicas waited the full TTL before claiming the
		// per-project email lock on shutdown.
		elector := c.initWorkerElector("email_imap_receiver_" + project.ID)
		if elector != nil {
			ch.SetLeaderGate(elector)
			elector.BootstrapAcquire(ctx)
			go elector.Run(ctx)
			c.extraElectorsMu.Lock()
			c.extraElectors = append(c.extraElectors, elector)
			c.extraElectorsMu.Unlock()
		}

		s.logger.Info().
			Str("project_id", project.ID).
			Bool("leader_gated", elector != nil).
			Msg("email dispatcher receiver wired")

		// Capture loop variables — every goroutine needs its own
		// channel+project pair (Go ≤1.21 semantics).
		ch := ch
		projectID := project.ID
		go func() {
			if err := ch.Start(ctx, receiver); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Warn().Err(err).Str("project_id", projectID).Msg("email channel.Start returned")
			}
		}()
	}

	// Multi-channel auto-resume wiring (2026-05-21). The
	// TelegramBot was the single notifier pre-fix; email channels
	// now implement the same interface keyed on their own
	// pending-followup map. The multiplexer fans out events so a
	// task created in any channel's session resumes on the right
	// channel.
	notifiers := []executor.CompletionNotifier{}
	if c.TelegramBot != nil {
		notifiers = append(notifiers, c.TelegramBot)
	}
	for _, ch := range s.channels {
		if ch != nil {
			notifiers = append(notifiers, ch)
		}
	}
	// A2A webhook push rides the same terminal-state hook.
	if p := c.a2aPushNotifier(); p != nil {
		notifiers = append(notifiers, p)
	}
	if multi := newMultiCompletionNotifier(notifiers...); multi != nil && c.Executor != nil {
		c.Executor.SetCompletionNotifier(multi)
	}

	// Per-channel registrar: only one wired today (email). All
	// email channels share the same channel name ("email") so the
	// dispatcher's create_task picks the right registrar by
	// Channel.Name(); each instance's pending-followup map
	// filters by taskID so cross-project leaks are impossible.
	//
	// Multi-email-channel deployments: the LAST one wins. Slice-2
	// will route per-project once Request carries project-scoped
	// channel resolution; today every email channel sees every
	// task-completion event and filters by its own map, so the
	// last-wins is functional but inefficient.
	for _, ch := range s.channels {
		if ch == nil {
			continue
		}
		c.Dispatcher.SetChannelFollowupRegistrar(ch.Name(), ch)
	}
	return nil
}

// Stop is a no-op — channel.Start + elector goroutines respect
// ctx cancellation handed down through the daemon's drain.
func (s *EmailChannelsSubsystem) Stop(_ context.Context) error { return nil }
