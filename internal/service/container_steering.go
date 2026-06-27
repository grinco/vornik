package service

import (
	"vornik.io/vornik/internal/steering"
)

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
	if src.Channel == "" || src.Session == "" {
		return nil
	}
	return steering.NewOperatorAlert(
		&containerChannelResolver{c: c},
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
	return steering.New(
		c.repos.ChatAudit,
		&containerChannelResolver{c: c},
		baseURL,
		enabled,
		c.Logger.With().Str("component", "steering").Logger(),
	)
}
