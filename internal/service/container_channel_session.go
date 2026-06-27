package service

// Helper that constructs a sessionstore.Persister bound to a given
// channel kind, pulling the ChannelSessionRepository off the
// container's repos struct. Centralised here so the per-channel
// wiring blocks in container.go stay one-liners.

import (
	"vornik.io/vornik/internal/sessionstore"
)

// channelSessionPersister returns a *sessionstore.Persister bound
// to kind, or nil when the repo isn't wired (single-process
// SQLite deploys + test wiring without a backend). Callers can
// safely pass the result to a store's SetPersister: nil disables
// the DB layer.
func (c *Container) channelSessionPersister(kind string) *sessionstore.Persister {
	if c == nil || c.repos == nil || c.repos.ChannelSessions == nil {
		return nil
	}
	return sessionstore.New(
		c.repos.ChannelSessions,
		kind,
		c.Logger.With().Str("component", "session-store").Str("channel", kind).Logger(),
	)
}
