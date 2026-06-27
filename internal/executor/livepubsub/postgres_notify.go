// Postgres-specific NOTIFY/LISTEN driver for the cross-replica
// publisher. Wraps lib/pq's pq.Listener so the rest of the
// livepubsub package stays driver-agnostic (tests use stub
// Notifier + Listener implementations that don't need a real
// Postgres connection).

package livepubsub

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/lib/pq"
	"github.com/rs/zerolog"
)

// PostgresNotifier implements Notifier via pg_notify(). Uses
// the daemon's existing connection pool — no extra connection
// needed since NOTIFY is fire-and-forget.
type PostgresNotifier struct {
	db *sql.DB
}

// NewPostgresNotifier wraps an existing *sql.DB.
func NewPostgresNotifier(db *sql.DB) *PostgresNotifier {
	return &PostgresNotifier{db: db}
}

// Notify fires `SELECT pg_notify($1, $2)`. pg_notify (function
// form) accepts parameter placeholders; the bare `NOTIFY <chan>,
// '<payload>'` statement form doesn't.
func (n *PostgresNotifier) Notify(ctx context.Context, channel, payload string) error {
	if n == nil || n.db == nil {
		return fmt.Errorf("livepubsub: PostgresNotifier requires *sql.DB")
	}
	_, err := n.db.ExecContext(ctx, "SELECT pg_notify($1, $2)", channel, payload)
	return err
}

// PostgresListener implements Listener via lib/pq's pq.Listener.
// pq.Listener owns its own dedicated connection (NOT from the
// pool) and handles reconnect/backoff transparently — the
// caller's notification channel stays alive across transient
// connectivity blips.
type PostgresListener struct {
	dsn    string
	logger zerolog.Logger
}

// NewPostgresListener constructs the listener over a DSN. The
// listener doesn't open the connection until Start fires.
func NewPostgresListener(dsn string, logger zerolog.Logger) *PostgresListener {
	return &PostgresListener{dsn: dsn, logger: logger}
}

// Start opens the LISTEN connection + spawns the receiver
// goroutine. The returned channel emits one Notification per
// inbound NOTIFY frame. Cancelling ctx triggers Close on the
// underlying pq.Listener, which drains then closes the channel.
//
// pq.Listener calls its callback with status events on
// reconnect; we log them but don't surface to the caller —
// during a reconnect window NOTIFYs may be lost (Postgres docs:
// "session-only delivery"). The persistence layer is the
// authoritative source; the local replica's subscribers will
// see whatever the emitting replica's Publish landed in the DB
// once the reconnect succeeds AND a subsequent NOTIFY arrives.
// For events emitted DURING the reconnect window, late-joining
// subscribers can still replay from the DB.
func (l *PostgresListener) Start(ctx context.Context, channel string) (<-chan Notification, error) {
	if l == nil || l.dsn == "" {
		return nil, fmt.Errorf("livepubsub: PostgresListener requires DSN")
	}
	const (
		minReconnect = 1 * time.Second
		maxReconnect = 60 * time.Second
	)
	pqL := pq.NewListener(l.dsn, minReconnect, maxReconnect, func(ev pq.ListenerEventType, err error) {
		switch ev {
		case pq.ListenerEventConnected:
			l.logger.Info().Msg("livepubsub: postgres LISTEN connected")
		case pq.ListenerEventDisconnected:
			l.logger.Warn().Err(err).Msg("livepubsub: postgres LISTEN disconnected; reconnect in progress")
		case pq.ListenerEventReconnected:
			l.logger.Info().Msg("livepubsub: postgres LISTEN reconnected")
		case pq.ListenerEventConnectionAttemptFailed:
			l.logger.Warn().Err(err).Msg("livepubsub: postgres LISTEN connection attempt failed")
		}
	})
	if err := pqL.Listen(channel); err != nil {
		_ = pqL.Close()
		return nil, fmt.Errorf("livepubsub: LISTEN %s: %w", channel, err)
	}
	out := make(chan Notification, 128)
	go l.run(ctx, pqL, out)
	return out, nil
}

// run is the receive loop. Each pq notification becomes one
// Notification on the out channel; ctx cancellation drives
// shutdown via Close + close(out).
func (l *PostgresListener) run(ctx context.Context, pqL *pq.Listener, out chan<- Notification) {
	defer close(out)
	defer func() { _ = pqL.Close() }()
	// Periodic Ping keeps the connection healthy on idle channels
	// (Postgres LISTEN doesn't send keepalives on its own).
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case n, ok := <-pqL.Notify:
			if !ok {
				return
			}
			if n == nil {
				// pq.Listener emits nil on connection blips; the
				// reconnect machinery handles recovery, no action
				// needed from us.
				continue
			}
			select {
			case out <- Notification{Channel: n.Channel, Payload: n.Extra}:
			case <-ctx.Done():
				return
			default:
				// Consumer too slow. The cross-replica flow tolerates
				// drops — the persistence row remains, and a future
				// notification or subscriber-Subscribe replay covers
				// the gap.
				l.logger.Warn().Str("channel", n.Channel).Msg("livepubsub: notification channel full; dropped")
			}
		case <-ticker.C:
			if err := pqL.Ping(); err != nil {
				l.logger.Warn().Err(err).Msg("livepubsub: postgres LISTEN ping failed")
			}
		}
	}
}
