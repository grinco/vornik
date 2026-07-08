package service

import (
	"strconv"

	"vornik.io/vornik/internal/steering"
)

// telegramProjectRecipients resolves ownerless-task steering alerts to the
// Telegram chat IDs of allowed users with access to the task's project
// (wildcard users + users scoped to that project). Implements
// steering.ProjectRecipients. Only the telegram channel is backed today;
// other channels return nil so the notifier uses its fallback session.
type telegramProjectRecipients struct{ c *Container }

func (t telegramProjectRecipients) RecipientsForProject(channel, projectID string) []string {
	if channel != "telegram" || t.c == nil || t.c.TelegramBot == nil {
		return nil
	}
	ids := t.c.TelegramBot.OperatorChatIDsForProject(projectID)
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, strconv.FormatInt(id, 10))
	}
	return out
}

// operatorAlertNotifier builds the fallback steering sink for ownerless
// autonomy tasks (those the chat/DM notifier can't reach). Returns nil when no
// operator recipient is configured — the backwards-compatible default. Shares
// the same lazy channel resolver as the chat notifier and is gated by the same
// SteeringNotificationsEnabled flag.
func (c *Container) operatorAlertNotifier() *steering.OperatorAlertNotifier {
	if c == nil || c.Config == nil {
		return nil
	}
	src := c.Config.SteeringOperatorAlert
	// A channel is required; Session is now only a fallback for projects that
	// resolve to no per-project recipient, so it's optional.
	if src.Channel == "" {
		return nil
	}
	return steering.NewOperatorAlert(
		&containerChannelResolver{c: c},
		telegramProjectRecipients{c: c},
		c.steeringTaskGetter(),
		c.Config.Auth.ExternalBaseURL,
		steering.OperatorAlertConfig{Channel: src.Channel, Session: src.Session, Address: src.Address},
		c.Config.SteeringNotificationsEnabled,
		c.Logger.With().Str("component", "operator-alert").Logger(),
	)
}

// steeringNotifier builds the steering-notification sink (AWAITING_INPUT /
// AWAITING_APPROVAL → push to the originating chat/DM). Returns nil when the
// chat-audit repo isn't wired — the notifier resolves a task's originating
// channel from its ChatTurnID via chat_audit, so without that repo there's
// nothing to resolve. The channel resolver is the same lazy one reminders
// uses (reads c.TelegramBot / EmailChannels / SlackChannels at send time).
//
// A fresh instance per caller is fine: the executor hooks AWAITING_INPUT and
// autonomy hooks AWAITING_APPROVAL — disjoint states, so the per-instance
// dedup never needs to be shared.
func (c *Container) steeringNotifier() *steering.Notifier {
	if c == nil || c.repos == nil || c.repos.ChatAudit == nil {
		return nil
	}
	baseURL := ""
	if c.Config != nil {
		baseURL = c.Config.Auth.ExternalBaseURL
	}
	enabled := c.Config != nil && c.Config.SteeringNotificationsEnabled
	var checkpoints steering.CheckpointReader
	if c.repos.Messages != nil {
		checkpoints = c.repos.Messages // enables decision-option buttons on steering prompts
	}
	return steering.New(
		c.repos.ChatAudit,
		&containerChannelResolver{c: c},
		c.steeringTaskGetter(),
		checkpoints,
		baseURL,
		enabled,
		c.Logger.With().Str("component", "steering").Logger(),
	)
}

// steeringTaskGetter returns the task repo the steering notifiers use to walk
// a task's ParentTaskID ancestry (to find the originating chat of a
// chat-scheduled task's children). Returns nil when the task repo isn't wired
// — the notifiers then fall back to inspecting only the immediate task.
func (c *Container) steeringTaskGetter() steering.TaskGetter {
	if c == nil || c.repos == nil || c.repos.Tasks == nil {
		return nil
	}
	return c.repos.Tasks
}
