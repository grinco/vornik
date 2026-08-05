package service

// EmailChannelsSubsystem owns the email channels lifecycle plus
// the cross-channel CompletionNotifier multiplex wiring. Per-
// project (one ConversationChannel per project with a fully-
// configured email block); each channel gets its own
// per-project elector ("email_imap_receiver_<projectID>") so
// projects fail over independently — losing one project's lease
// doesn't take down email for the rest.
//
// One responsibility beyond per-channel start: SetCompletionNotifier on the
// executor with a multi-channel fan-out. This subsystem rebuilds the whole
// multiplexer, so anything omitted here is silently dropped on any deployment
// that has email configured — which is why the A2A push, reminder and
// chat-completion notifiers are all re-added below rather than assumed.
//
// Email itself is no longer in that fan-out (2026-08-05): it announces
// completion through the durable chatCompletionSink, and the follow-up
// registrar that fed its in-memory resume map went with it. Both halves are
// pinned by TestEmailIsNotAnnouncedTwice.
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
			Channel:                  ch,
			Agent:                    c.Dispatcher,
			Sessions:                 store,
			Disclosure:               c.AIDisclosure,
			Media:                    c.mediaSight(),
			DisclosureMetrics:        c.disclosureObserver(),
			MemoryWriteConfirmations: c.chatMemoryConfirmations(),
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
	// Email channels are deliberately NOT registered as their own notifiers
	// any more (2026-08-05). They announce completion through the durable
	// chatCompletionSink below; registering both would announce every task
	// twice. Removing this is the other half of that move, not an oversight —
	// see the allowlist comment in container_chat_completion.go.
	//
	// A2A webhook push rides the same terminal-state hook.
	if p := c.a2aPushNotifier(); p != nil {
		notifiers = append(notifiers, p)
	}
	// Reminders completion notifier (Task 7): re-added here (alongside
	// the Telegram-fallback wiring in container.go) because this
	// subsystem's Start rebuilds the whole multiplexer from scratch —
	// without this, an email-configured deployment would silently drop
	// task-kind reminder notifications the moment email channels start.
	if n := c.reminderCompletionNotifier(); n != nil {
		notifiers = append(notifiers, n)
	}
	// Re-added here for the same reason the reminder notifier is: this subsystem
	// rebuilds the whole multiplexer, so omitting it would silently drop the Slack
	// completion notice on any deployment that also has email configured.
	if n := c.chatCompletionSink(); n != nil {
		notifiers = append(notifiers, n)
	}
	if multi := newMultiCompletionNotifier(notifiers...); multi != nil && c.Executor != nil {
		c.Executor.SetCompletionNotifier(multi)
	}

	// The email follow-up registrar is intentionally NOT wired (2026-08-05).
	//
	// It existed to populate Channel.followups, which only Channel's own
	// NotifyTaskCompleted drained. With email moved to the durable notifier
	// nothing claims those entries, so registering would grow that map for the
	// life of the process — one entry per awaited task, never released. The
	// registration and the drain are one mechanism and both go together.
	//
	// Telegram keeps its registrar; it still runs its own auto-resume.
	return nil
}

// Stop is a no-op — channel.Start + elector goroutines respect
// ctx cancellation handed down through the daemon's drain.
func (s *EmailChannelsSubsystem) Stop(_ context.Context) error { return nil }
