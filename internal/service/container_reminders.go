package service

// Wires the scheduled-reminders subsystem into the daemon. The
// Runner itself lives in internal/reminders; this file is the
// container glue (channel-resolver adapter + lifecycle).
//
// See https://docs.vornik.io

import (
	"context"
	"io"
	"strconv"
	"strings"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/reminders"
	"vornik.io/vornik/internal/taskcreate"
	"vornik.io/vornik/internal/telegram"
)

// containerChannelResolver implements reminders.ChannelResolver
// against the container's wired channels. Phase B adds email +
// Slack alongside Telegram; webchat stays unmapped because the
// channel is request-scoped (constructed per page load in
// internal/ui/chat.go) and can't receive a daemon-initiated
// outbound today.
type containerChannelResolver struct {
	c *Container
}

type reminderTelegramFileSender struct {
	bot    *telegram.Bot
	chatID int64
}

func (s reminderTelegramFileSender) SendArtifactFile(ctx context.Context, fileName string, content io.Reader, caption string) error {
	return s.bot.SendDocumentReader(ctx, s.chatID, content, fileName, caption)
}

// containerReminderFileSenderResolver mirrors containerChannelResolver for
// attachment-capable destinations. Slack is selected by the team prefix in its
// durable session ref so multi-workspace deployments do not upload through the
// first unrelated bot token.
type containerReminderFileSenderResolver struct{ c *Container }

func (r *containerReminderFileSenderResolver) ResolveReminderFileSender(channel, channelRef string) reminders.ReminderFileSender {
	if r == nil || r.c == nil {
		return nil
	}
	switch channel {
	case "slack":
		for i, ch := range r.c.SlackChannels {
			if ch == nil || i >= len(r.c.SlackProjects) || r.c.SlackProjects[i] == nil {
				continue
			}
			teamID := strings.TrimSpace(r.c.SlackProjects[i].Slack.TeamID)
			if teamID != "" && strings.HasPrefix(channelRef, teamID+"/") {
				return slackFileSender{channel: ch, sessionID: channelRef}
			}
		}
	case "email":
		for _, ch := range r.c.EmailChannels {
			if ch != nil {
				return emailFileSender{ch: ch, sessionID: channelRef}
			}
		}
	case "telegram":
		if r.c.TelegramBot == nil {
			return nil
		}
		chatID, err := strconv.ParseInt(channelRef, 10, 64)
		if err == nil {
			return reminderTelegramFileSender{bot: r.c.TelegramBot, chatID: chatID}
		}
	}
	return nil
}

// ResolveChannel returns the conversation.Channel for the given
// name. Empty / unknown names return nil so the runner records
// the row as errored rather than crashing.
//
// Multi-channel deployments (multiple Email/Slack channels, one
// per project) return the FIRST enabled instance. v1 deployments
// usually have ≤1 of each; per-project routing is a v1.3
// follow-on tied to the project_id column on
// dispatcher_reminders.
func (cr *containerChannelResolver) ResolveChannel(name string) conversation.Channel {
	if cr == nil || cr.c == nil {
		return nil
	}
	switch name {
	case "telegram":
		if cr.c.TelegramBot == nil {
			return nil
		}
		// *telegram.Bot itself does NOT implement conversation.Channel
		// — the adapter is *telegram.Channel, constructed via
		// NewChannel(bot). Before this fix, a direct type assertion
		// on the *Bot returned ok=false silently and every reminder
		// landed in status=firing with last_error="channel telegram
		// not configured" (rem_20260523220608 was the canary).
		return telegram.NewChannel(cr.c.TelegramBot)
	case "email":
		for _, ch := range cr.c.EmailChannels {
			if ch != nil {
				return ch
			}
		}
	case "slack":
		for _, ch := range cr.c.SlackChannels {
			if ch != nil {
				return ch
			}
		}
	}
	return nil
}

// initReminders constructs the Runner, or returns nil to disable the
// heartbeat. Reminders are a leader-elected background worker AND a
// Postgres-only feature, so the runner only starts on a worker node backed by
// Postgres:
//
//   - Non-worker (ui/webhook): the leader elector is nil, so the runner would
//     poll lease_due ungated. On a webhook node that meant an error every 30s.
//   - SQLite: the reminder repository is an explicit "unsupported" stub whose
//     every method returns ErrSQLiteRemindersUnsupported, so polling it just
//     spams the log. (Incident 2026-06-12.)
func (c *Container) initReminders() *reminders.Runner {
	if c.skipNonWorker("reminders") {
		return nil
	}
	if c.Config.Database.Driver == "sqlite" {
		c.Logger.Info().Msg("reminders: disabled on sqlite backend (Postgres required)")
		return nil
	}
	if c.repos == nil || c.repos.Reminders == nil {
		c.Logger.Debug().Msg("reminders: repo not wired; heartbeat disabled")
		return nil
	}
	return reminders.New(reminders.Config{
		Repo:      c.repos.Reminders,
		Resolver:  &containerChannelResolver{c: c},
		AuditRepo: c.repos.AdminAudit,
		Logger:    c.Logger.With().Str("component", "reminders").Logger(),
		// Creator is deliberately left nil here — c.taskCreator doesn't
		// exist yet at this point in the boot sequence (initReminders
		// runs before initHTTPServer constructs it). Task-kind
		// reminders are enabled later, once the creator exists, by
		// RemindersSubsystem.Start calling Runner.SetCreator. See the
		// taskCreator field comment on Container.
	})
}

// containerTaskCreator adapts *taskcreate.Creator to reminders.TaskCreator
// so the reminders package stays decoupled from taskcreate.Params. Mirrors
// a2aTaskCreatorAdapter (container_a2a.go), the existing precedent for
// wrapping the shared task-creation core for a narrow consumer interface.
type containerTaskCreator struct {
	creator interface {
		Create(context.Context, taskcreate.Params) (*persistence.Task, error)
	}
}

// CreateScheduledTask implements reminders.TaskCreator. Maps the runner's
// narrow ScheduledTaskParams onto taskcreate.Params, tagging the task with
// CreationSource=SCHEDULED and threading the reminder id through
// ExtraContext so downstream consumers (audit, UI) can trace which
// reminder fired it.
func (a *containerTaskCreator) CreateScheduledTask(ctx context.Context, p reminders.ScheduledTaskParams) (string, error) {
	task, err := a.creator.Create(ctx, taskcreate.Params{
		ProjectID:      p.ProjectID,
		TaskType:       p.TaskType,
		Prompt:         p.Prompt,
		CreationSource: persistence.TaskCreationSourceScheduled,
		IdempotencyKey: p.IdempotencyKey,
		ExtraContext:   map[string]any{"scheduled_reminder_id": p.ReminderID},
	})
	if err != nil {
		return "", err
	}
	return task.ID, nil
}

// reminderCompletionNotifier builds the reminders completion notifier when
// its dependencies are wired: Postgres-backed repo + admin audit repo.
// Registered into the executor's completion-notifier fan-out from two
// call sites (container.go's Telegram-fallback block and
// EmailChannelsSubsystem.Start) — see subsystem_email_channels.go for
// why both exist. Returns nil (not registered) when reminders aren't
// available on this deployment.
func (c *Container) reminderCompletionNotifier() executor.CompletionNotifier {
	if c == nil || c.repos == nil || c.repos.Reminders == nil {
		return nil
	}
	if c.Config == nil || c.Config.Database.Driver != "postgres" {
		return nil
	}
	var opts []reminders.CompletionOption
	if c.repos.Artifacts != nil && c.artifactStore != nil {
		opts = append(opts, reminders.WithArtifactDelivery(
			c.repos.Artifacts,
			c.artifactStore,
			&containerReminderFileSenderResolver{c: c},
			c.repos.Tasks,
		))
	}
	return reminders.NewCompletionNotifier(
		c.repos.Reminders,
		&containerChannelResolver{c: c},
		c.repos.AdminAudit,
		c.Logger.With().Str("component", "reminders-notify").Logger(),
		nil,
		opts...,
	)
}
