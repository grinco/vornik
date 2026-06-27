package service

import (
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/telegram"
)

// wireTelegramReceiver assembles the ConversationChannel plumbing
// for Telegram: builds the Channel adapter over the existing *Bot,
// wraps the bot's per-chat state in a telegram.SessionStore, and
// constructs a dispatcher.ChannelReceiver that fires
// receiver.Receive on every inbound non-slash message. The
// receiver is attached to the bot via SetReceiver so HandleMessage
// + the auto-resume follow-up route every dispatcher-bound turn
// through it.
//
// Returns nil when the container has no telegram bot or no
// dispatcher wired — both are required to make the receiver
// functional. The legacy path remains active in those cases.
//
// Idempotent: calling twice replaces the previously-wired receiver.
// In practice this fires once during initDispatcher.
func (c *Container) wireTelegramReceiver() *dispatcher.ChannelReceiver {
	if c.TelegramBot == nil || c.Dispatcher == nil {
		return nil
	}
	channel := telegram.NewChannel(c.TelegramBot)
	store := telegram.NewSessionStore(c.TelegramBot, c.Registry)
	receiver := &dispatcher.ChannelReceiver{
		Channel:             channel,
		Agent:               c.Dispatcher,
		Sessions:            store,
		ResultPostprocessor: telegram.GuardFooterPostprocessor(),
	}
	c.TelegramBot.SetReceiver(receiver)
	c.Logger.Info().Msg("telegram channel-receiver wired (slice 2 of ConversationChannel rollout)")
	return receiver
}
