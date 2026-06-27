package service

// Cross-instance config-reload broadcast. Slice 3a of the
// horizontal-scaling design:
// https://docs.vornik.io §3b.
//
// Without this, POST /api/v1/config/reload on instance A only
// refreshes A's in-process caches; peer replicas serve stale
// registry / MCP state until their own file-watcher fires
// (~5s polling window) or they receive their own reload
// request. With it: A's successful reload fires
// NOTIFY vornik_config_reloaded; every instance has a LISTEN
// goroutine that calls reloader.Reload() on receipt, idempotent
// on its own YAML state.
//
// Self-broadcast suppression: the NOTIFY payload carries this
// instance's holderID; the LISTEN side drops messages whose
// payload matches its own ID so an instance doesn't loop on
// its own broadcasts.

import (
	"context"
	"time"

	"github.com/lib/pq"
)

const configReloadChannel = "vornik_config_reloaded"

// installConfigReloadBroadcast wires the post-reload NOTIFY +
// the LISTEN consumer. Called from container Start once the DB
// is ready AND the ConfigReloader has been built. No-op when
// the DB isn't wired (SQLite branch / unit-test deployments).
//
// The LISTEN goroutine lives until ctx is cancelled (i.e. until
// the daemon's shutdown sequence cancels the root context). The
// pq.Listener handles transient reconnects internally — same
// pattern container_live_publisher.go already runs for the
// live-events channel.
func (c *Container) installConfigReloadBroadcast(ctx context.Context) {
	if c.DB == nil || c.ConfigReloader == nil {
		return
	}
	holderID := c.daemonHolderID()

	// Sender hook: after every successful Reload, broadcast.
	c.ConfigReloader.SetPostReloadHook(func() {
		// Fire-and-forget. A failed NOTIFY just means peers
		// won't reload until their own watcher catches the file
		// change (worst-case 5s polling window). The next
		// successful reload's NOTIFY catches up.
		if _, err := c.DB.ExecContext(ctx, "SELECT pg_notify($1, $2)", configReloadChannel, holderID); err != nil {
			c.Logger.Warn().Err(err).
				Str("channel", configReloadChannel).
				Msg("config reload: NOTIFY failed; peer replicas may serve stale config until their file-watcher catches up")
			return
		}
		c.Logger.Debug().Str("channel", configReloadChannel).Msg("config reload: broadcast")
	})

	// Receiver: dedicated pq.Listener (own connection — NOTIFY
	// can't share the pool because LISTEN holds the connection
	// open for the channel's lifetime).
	dsn := postgresDSNFromConfig(c.Config.Database.Host, c.Config.Database.Port,
		c.Config.Database.User, c.Config.Database.Password,
		c.Config.Database.Name, c.Config.Database.SSLMode)
	if dsn == "" {
		c.Logger.Warn().Msg("config reload: no DSN; LISTEN goroutine not started — peer broadcasts will not refresh this replica")
		return
	}
	go c.runConfigReloadListener(ctx, dsn, holderID)
}

func (c *Container) runConfigReloadListener(ctx context.Context, dsn, ownHolderID string) {
	const (
		minReconnect = 1 * time.Second
		maxReconnect = 60 * time.Second
	)
	logger := c.Logger.With().Str("component", "config-reload-listener").Logger()
	pqL := pq.NewListener(dsn, minReconnect, maxReconnect, func(ev pq.ListenerEventType, err error) {
		switch ev {
		case pq.ListenerEventConnected:
			logger.Info().Msg("LISTEN connected")
		case pq.ListenerEventDisconnected:
			logger.Warn().Err(err).Msg("LISTEN disconnected; reconnecting")
		case pq.ListenerEventReconnected:
			logger.Info().Msg("LISTEN reconnected")
		case pq.ListenerEventConnectionAttemptFailed:
			logger.Warn().Err(err).Msg("LISTEN connection attempt failed")
		}
	})
	if err := pqL.Listen(configReloadChannel); err != nil {
		logger.Warn().Err(err).Msg("LISTEN setup failed; peer config-reload broadcasts will be ignored")
		_ = pqL.Close()
		return
	}
	defer func() { _ = pqL.Close() }()

	for {
		select {
		case <-ctx.Done():
			return
		case n, ok := <-pqL.Notify:
			if !ok {
				return
			}
			if n == nil {
				// pq.Listener emits nil on reconnect blips; the
				// internal reconnect machinery handles recovery.
				continue
			}
			// Self-broadcast suppression: ignore notifications
			// whose payload matches THIS instance's holderID.
			// Otherwise instance A's own NOTIFY would arrive at
			// A's LISTEN goroutine and trigger a recursive
			// reload → another NOTIFY → loop.
			if n.Extra == ownHolderID {
				continue
			}
			logger.Info().
				Str("peer", n.Extra).
				Msg("peer config-reload broadcast received; reloading local caches")
			if err := c.ConfigReloader.Reload(); err != nil {
				// Soft-fail: a peer's reload broadcast means
				// SOME instance succeeded; ours might fail for
				// local reasons (e.g. stale file cache, race
				// with operator edits). Log and let the next
				// broadcast / file-watcher catch up.
				logger.Warn().Err(err).Msg("local reload after peer broadcast failed; will retry on next broadcast or file change")
			}
		}
	}
}
