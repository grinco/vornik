package service

// Wires the scheduled-reminders subsystem into the daemon. The
// Runner itself lives in internal/reminders; this file is the
// container glue (channel-resolver adapter + lifecycle).
//
// See https://docs.vornik.io

import (
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/reminders"
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
	})
}
